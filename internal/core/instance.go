package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/warden/internal/box"
	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/builtin"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/hooks"
	"github.com/nenkoru/warden/internal/mcpx"
	"github.com/nenkoru/warden/internal/meter"
	"github.com/nenkoru/warden/internal/policy"
	"github.com/nenkoru/warden/internal/registry"
	"github.com/nenkoru/warden/internal/skills"
	"github.com/nenkoru/warden/internal/update"
)

// Instance: one warden bound to one domain, with its state lock held.
type Instance struct {
	Cfg    *Config
	Dom    *domain.Domain
	St     *State
	Policy *policy.Engine
	Triage *TriageSkill
	Web    *builtin.Web
	Brain  brain.Brain // nil: no judgement calls
	MCP    *mcpx.Manager
	Skills []skills.Skill
	Hooks  []hooks.Hook
	Now    func() time.Time
	// Run runs one command of a step (contained); tests may replace it.
	Run func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error)
	// Runner: the sandbox runner. "" = none available: Warden observes and
	// reports but refuses whatever writes or executes. "none" = the owner chose
	// to run unsandboxed (sandbox = "none").
	Runner    string
	NoSandbox string

	// Upd: the self-update ([update]; selfupdate.go). nil when it could not
	// be set up (updErr says why, in every tick's errors).
	Upd    *update.Updater
	updErr error

	DryRun   bool
	FirstRun bool              // the domain's first run (dry-run because mode is unset)
	Refused  map[string]string // MCP servers refused, and why
	readOnly map[string]bool   // MCP servers started read-only (their tools may go to the brain)

	// Brains (brains.go): the named brains, the default one, the budget.
	Ledger       *meter.Ledger
	slots        map[string]*brainSlot
	defaultBrain string
	usage        map[string]*BrainUse
	proxyAddr    string
	proxyPort    int
	paused       string // this tick is paused by the owner (pause steps), and why

	mu       sync.Mutex
	notes    []string
	cleanups []func()
	quiet    string                 // this tick's pings wait (the backoff, in words); "" = they may go
	scores   map[string]SourceScore // the owner's labels per source, this tick (sources.go)
}

// pickRunner resolves cfg.Sandbox; tests replace it to simulate a machine without a sandbox.
var pickRunner = box.Pick

func noSandboxLine(what string) string {
	return what + " refused — no sandbox available: set sandbox = \"none\" to run unsandboxed — your choice"
}

var errNoSandbox = errors.New(`no sandbox available: set sandbox = "none" to run unsandboxed — your choice`)

// Open loads the domain, opens the state (taking the tick lock), the policy,
// the extensions, the MCP servers and the brain.
func Open(cfg *Config) (*Instance, error) {
	params := map[string]string{}
	for k, v := range cfg.Params {
		params[k] = v
	}
	if r, ok := params["root"]; ok {
		params["root"] = cfg.abs(r, ".")
	}
	dom, err := domain.Load(cfg.DomainDir(), params)
	if err != nil {
		return nil, fmt.Errorf("domain %s: %w", cfg.Domain, err)
	}
	if r, ok := dom.Params["root"]; ok && !filepath.IsAbs(r) {
		dom.Params["root"] = cfg.abs(r, ".")
	}
	st, err := OpenState(cfg.State)
	if err != nil {
		return nil, err
	}
	in := &Instance{Cfg: cfg, Dom: dom, St: st, Triage: OpenTriage(cfg.State), Web: builtin.DefaultWeb(), Now: time.Now,
		Refused: map[string]string{}, readOnly: map[string]bool{}}
	in.Run = in.contained
	in.Upd, in.updErr = update.New(cfg.Update, cfg.Name, cfg.State, cfg.Path)
	if cfg.Sandbox == "none" {
		in.Runner = "none"
	} else if r, err := pickRunner(cfg.Sandbox); err != nil {
		in.NoSandbox = err.Error()
	} else {
		in.Runner = r
	}
	in.FirstRun = st.Domains[dom.Name] == 0
	in.DryRun = cfg.Mode == "dry-run" || cfg.Mode == "" && in.FirstRun
	if in.Policy, err = policy.Open(policy.Config{DryRun: in.DryRun, Grants: cfg.Grants, Rules: cfg.Rules}, cfg.State); err != nil {
		st.Close()
		return nil, err
	}
	in.Hooks = append(in.Hooks, cfg.Hooks...)
	skillDirs := []string{filepath.Join(dom.Dir, "skills")}
	lock, err := registry.ReadLock(in.LockPath())
	if err != nil {
		in.note("warden.lock: " + err.Error())
	}
	for _, ref := range append(slices.Clone(dom.Extensions), cfg.Extensions...) {
		if strings.HasPrefix(ref, "mcp:") {
			continue // checked with the domain's [mcp] servers below
		}
		l, why := locked(lock, ref)
		if why != "" {
			in.note("extension " + ref + ": " + why)
			continue
		}
		skillDirs = append(skillDirs, l.Skills...)
		if l.Hooks != "" && slices.Contains(cfg.EnableHooks, ref) {
			hs, err := pluginHooks(l.Hooks, ref)
			if err != nil {
				in.note("extension " + ref + " hooks: " + err.Error())
			}
			in.Hooks = append(in.Hooks, hs...)
		} else if l.Hooks != "" {
			in.note("extension " + ref + " brings hooks: off until enable_hooks lists it")
		}
	}
	if in.Skills, err = skills.Load(skillDirs...); err != nil {
		in.note("skills: " + err.Error())
	}
	in.MCP = mcpx.NewManager(in.servers(lock))
	if err := in.openBrain(); err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

func (in *Instance) Close() {
	if in.MCP != nil {
		in.MCP.Close()
	}
	for _, c := range in.cleanups {
		c()
	}
	in.cleanups = nil
	in.St.Close()
}

func (in *Instance) LockPath() string { return filepath.Join(in.Cfg.Dir, "warden.lock") }

func (in *Instance) note(s string) {
	in.mu.Lock()
	in.notes = append(in.notes, s)
	in.mu.Unlock()
}

func (in *Instance) noteOnce(s string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !slices.Contains(in.notes, s) {
		in.notes = append(in.notes, s)
	}
}

// locked: the lock entry for a registry ref, verified; why it can't be used otherwise.
func locked(lock registry.Lock, s string) (registry.Locked, string) {
	ref, err := registry.Parse(s)
	if err != nil {
		return registry.Locked{}, err.Error()
	}
	for _, l := range lock.Entries {
		if l.Ref.Kind != ref.Kind || l.Ref.Name != ref.Name || l.Ref.Source != ref.Source || ref.Version != "" && l.Ref.Version != ref.Version {
			continue
		}
		if err := registry.Verify(l); err != nil {
			return l, "refused: does not match warden.lock (" + err.Error() + "); run `warden add " + s + "` again"
		}
		return l, ""
	}
	return registry.Locked{}, "not in warden.lock — run `warden add " + s + "`"
}

// pluginHooks reads a Claude Code hooks/hooks.json; only entries for Warden's
// own events are kept.
func pluginHooks(path, from string) ([]hooks.Hook, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hj struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &hj); err != nil {
		return nil, err
	}
	var out []hooks.Hook
	for ev, groups := range hj.Hooks {
		if !slices.Contains([]string{"Wake", "Finding", "PreAction", "PostAction", "Ask", "Report", "Signal", "Digest"}, ev) {
			continue
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type == "command" && h.Command != "" {
					out = append(out, hooks.Hook{Event: ev, Matcher: g.Matcher, Command: []string{"sh", "-c", h.Command}, From: from})
				}
			}
		}
	}
	return out, nil
}

// servers: the domain's MCP servers. A registry server must be pinned in
// warden.lock and verified; a stdio server runs in the sandbox (refused if
// there is none and the owner hasn't chosen sandbox = "none").
func (in *Instance) servers(lock registry.Lock) []mcpx.Server {
	var out []mcpx.Server
	for _, name := range sortedKeys(in.Dom.MCP) {
		ref := in.Dom.MCP[name]
		s := mcpx.Server{Name: name, URL: ref.URL, Env: map[string]string{}}
		args := ref.Args
		if ref.Registry != "" {
			spec := "mcp:" + ref.Name
			if ref.Version != "" {
				spec += "@" + ref.Version
			}
			l, why := locked(lock, spec)
			if why != "" {
				in.Refused[name] = why
				continue
			}
			s.Command, s.URL = slices.Clone(l.Command), l.URL
			for _, kv := range l.Env {
				if k, v, ok := strings.Cut(kv, "="); ok {
					s.Env[k] = v
				}
			}
		} else {
			s.Command = slices.Clone(ref.Command)
		}
		if len(s.Command) > 0 {
			s.Command = append(s.Command, args...)
		}
		for k, v := range ref.Env {
			s.Env[k] = v
		}
		if len(s.Command) > 0 && in.Runner != "none" {
			if in.Runner == "" {
				in.Refused[name] = errNoSandbox.Error()
				continue
			}
			p := box.Policy{Name: "mcp " + name, ReadWrite: []string{in.tmp()}, Net: ref.Net}
			runner := in.Runner
			s.Wrap = func(argv, env []string) ([]string, []string, error) {
				a, e, cleanup, err := box.Command(runner, p, argv, env)
				in.mu.Lock()
				in.cleanups = append(in.cleanups, cleanup)
				in.mu.Unlock()
				return a, e, err
			}
		}
		in.readOnly[name] = slices.ContainsFunc(args, func(a string) bool { return a == "--read-only" || a == "--readonly" })
		out = append(out, s)
	}
	return out
}

func (in *Instance) openBrain() error { return in.openBrains() }

func (in *Instance) tmp() string {
	p := filepath.Join(in.Cfg.State, "work", "tmp")
	_ = os.MkdirAll(p, 0o700)
	return p
}

// contained runs a step's command: floors.Sandbox → the sandbox runner with
// writes to dir, Warden's temp and (for Go) the Go caches, egress only to
// floors.Net; otherwise plainly (observers like kubectl get).
func (in *Instance) contained(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
	runner, p := "none", box.Policy{Name: filepath.Base(argv[0]), ReadWrite: []string{dir, in.tmp()}}
	env := os.Environ()
	if f.Sandbox {
		if in.Runner == "" {
			return "", errNoSandbox
		}
		runner, p.Net, p.Deny = in.Runner, f.Net, in.secretFiles()
		if f.CleanEnv {
			env = cleanEnv()
		}
		if usesGo(argv) {
			p.ReadWrite = append(p.ReadWrite, goDirs()...)
			env = append(env, "GOTOOLCHAIN=local", "GOFLAGS=-buildvcs=false", "GOTELEMETRY=off")
		}
	}
	res, err := box.Run(ctx, runner, p, dir, argv, env)
	if f.Sandbox {
		net := "none"
		if len(f.Net) > 0 {
			net = strings.Join(f.Net, ", ")
		}
		in.note(fmt.Sprintf("ran `%s` in %s (net: %s)", short(strings.Join(argv, " "), 120), res.Runner, net))
		if res.Runner == "none" {
			in.noteOnce("⚠ SANDBOX OFF: sandboxed steps run uncontained (sandbox = \"none\", the owner's choice)")
		}
		for _, r := range res.Refused {
			in.note(fmt.Sprintf("egress refused: %s → %s", p.Name, r))
		}
	}
	return res.Output, err
}

// secretFiles: credential files the instance knows of (params *_file such as
// the GitHub token, brains' api_key_file), unreadable inside the sandbox.
func (in *Instance) secretFiles() []string {
	var out []string
	for k, v := range in.Dom.Params {
		if strings.HasSuffix(k, "_file") && v != "" && filepath.IsAbs(v) {
			out = append(out, v)
		}
	}
	for _, b := range append([]BrainConfig{in.Cfg.Brain}, mapValues(in.Cfg.Brains)...) {
		if b.APIKeyFile != "" {
			out = append(out, b.APIKeyFile)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func mapValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}

// cleanEnv: what a sandboxed command with clean_env sees — no secrets.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case slices.Contains([]string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TERM", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE", "GOROOT"}, k), strings.HasPrefix(k, "LC_"):
			env = append(env, kv)
		}
	}
	return env
}

func usesGo(argv []string) bool {
	for _, a := range argv {
		if a == "go" || strings.HasPrefix(a, "go ") || strings.Contains(a, " go ") {
			return true
		}
	}
	return false
}

var goDirsOnce = sync.OnceValue(func() []string {
	var out []string
	for _, k := range []string{"GOCACHE", "GOMODCACHE"} {
		if b, err := exec.Command("go", "env", k).Output(); err == nil && strings.TrimSpace(string(b)) != "" {
			out = append(out, strings.TrimSpace(string(b)))
		}
	}
	if b, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		gp, _, _ := strings.Cut(strings.TrimSpace(string(b)), string(os.PathListSeparator))
		if gp != "" {
			sumdb := filepath.Join(gp, "pkg", "sumdb")
			_ = os.MkdirAll(sumdb, 0o700)
			out = append(out, sumdb)
		}
	}
	return out
})

func goDirs() []string { return goDirsOnce() }

// env: what a builtin gets.
func (in *Instance) env(with map[string]any) *builtin.Env {
	params := map[string]string{}
	for k, v := range in.Dom.Params {
		params[k] = v
	}
	for k, v := range with {
		if s, ok := v.(string); ok {
			params[k] = s
		}
	}
	return &builtin.Env{Params: params, State: in.Cfg.State, Web: in.Web, Run: in.Run, Checks: in.checks, Brain: in.Brain, Brains: in.envBrains(),
		Tools: in.brainTools(context.Background()), Skills: skills.Index(in.Skills), Now: in.Now, Log: func(string) {}}
}

// brainTools: read_skill, and the tools of the domain's read-only MCP servers.
func (in *Instance) brainTools(ctx context.Context) []brain.Tool {
	var out []brain.Tool
	if len(in.Skills) > 0 {
		out = append(out, brain.Tool{Name: "read_skill", Description: "Load one of the domain's skills by name (its full instructions).",
			Schema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}},
			Run: func(_ context.Context, args map[string]any) (string, bool) {
				n, _ := args["name"].(string)
				for _, s := range in.Skills {
					if s.Name == n {
						return s.Body, true
					}
				}
				return "no skill " + n, false
			}})
	}
	if in.Brain == nil {
		return out
	}
	for _, name := range sortedKeys(in.readOnly) {
		if !in.readOnly[name] {
			continue
		}
		tools, err := in.MCP.Tools(ctx, name)
		if err != nil {
			in.noteOnce("mcp " + name + ": " + err.Error())
			continue
		}
		for _, t := range tools {
			var schema map[string]any
			_ = json.Unmarshal(t.Schema, &schema) // a schema that does not parse is sent as none
			server, tool := name, t.Name
			out = append(out, brain.Tool{Name: name + "__" + t.Name, Description: t.Description, Schema: schema,
				Run: func(ctx context.Context, args map[string]any) (string, bool) {
					text, isErr, err := in.MCP.Call(ctx, server, tool, args)
					if err != nil {
						return err.Error(), false
					}
					return text, !isErr
				}})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
