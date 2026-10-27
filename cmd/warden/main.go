// warden: a small always-on agent bound to one domain (a project, a Mac, a
// Kubernetes namespace …). See README.md and DESIGN.md.
//
//	warden init --pack NAME|DIR [--dir D] [--name N]   write warden.toml (mode dry-run)
//	warden tick                       one pass: signals → observe → feeds → triage → decide → act → check → report
//	warden listen                     awake: stream signals and the timer
//	warden serve                      HTTP: POST /signal, A2A at /a2a, GET /report (bearer tokens) + the timer
//	warden mcp                        Warden as a stdio MCP server (status, report, pending, approve, signal)
//	warden signal [--from WHO] [--no-tick] TEXT
//	warden status | report | packs
//	warden approve ID | decline ID
//	warden feedback FINDING noise|digest|important
//	warden improve [--candidate FILE] [--agent] | rollback
//	warden add REF | verify          extensions from registries, pinned in warden.lock
//	warden units [--listen]          launchd plist / systemd units (written, not loaded)
//	warden github-note --event E     a hook command: the event JSON on stdin → a comment on the repo's "Warden log" issue
//	warden update [--check]          check the releases now; install the newest verified one ([update]; see docs/usecases/self-update.md)
//
// Every command takes --config PATH (default ./warden.toml).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // zones for [brain] tz and the packs, also in minimal images

	"github.com/nenkoru/warden/internal/builtin"
	"github.com/nenkoru/warden/internal/core"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/mcpx"
	"github.com/nenkoru/warden/internal/registry"
	"github.com/nenkoru/warden/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "version" || cmd == "--version" {
		fmt.Println("warden " + version.Version)
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "warden.toml", "warden.toml")
	pack := fs.String("pack", "", "init: a pack name (packs/NAME) or a domain folder")
	dir := fs.String("dir", "", "init: the domain's root (a project folder)")
	name := fs.String("name", "", "init: the warden's name")
	cand := fs.String("candidate", "", "improve: a candidate triage TOML file")
	agent := fs.Bool("agent", false, "improve: ask the brain for the candidate")
	from := fs.String("from", "owner", "signal: who says it")
	noTick := fs.Bool("no-tick", false, "signal: only queue it")
	event := fs.String("event", "", "github-note: the hook event (Report, Ask, Digest)")
	listenUnits := fs.Bool("listen", false, "units: keep one process awake (warden listen) instead of a tick per interval")
	checkOnly := fs.Bool("check", false, "update: check and verify, install nothing")
	preflight := fs.Bool("preflight", false, "update: load the config and print the version (how an installer checks a new binary)")
	var pos []string
	for len(args) > 0 {
		_ = fs.Parse(args) // ExitOnError: Parse exits instead of returning
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "init":
		err = initCmd(*cfgPath, *pack, *dir, *name)
	case "tick":
		err = withInstance(*cfgPath, func(in *core.Instance) error { _, err := tick(ctx, in); return err })
	case "listen":
		err = withConfig(*cfgPath, func(cfg *core.Config) error {
			lctx, restart := context.WithCancel(ctx)
			defer restart()
			core.Listen(lctx, cfg, nil, tickFn(*cfgPath, restart), os.Stderr)
			return nil
		})
	case "serve":
		err = withConfig(*cfgPath, func(cfg *core.Config) error {
			if len(cfg.Serve.Tokens) == 0 {
				return errors.New("serve: no [serve] tokens in warden.toml (token → sender)")
			}
			sctx, restart := context.WithCancel(ctx)
			defer restart()
			return core.Serve(sctx, cfg, tickFn(*cfgPath, restart), os.Stderr)
		})
	case "update":
		if *preflight {
			err = withConfig(*cfgPath, func(*core.Config) error { fmt.Println("warden " + version.Version); return nil })
			break
		}
		err = withInstance(*cfgPath, func(in *core.Instance) error {
			msg, err := in.UpdateNow(ctx, *checkOnly)
			if msg != "" {
				fmt.Println(msg)
			}
			return err
		})
	case "mcp":
		err = withConfig(*cfgPath, func(cfg *core.Config) error {
			return mcpx.Serve(ctx, &core.Control{Cfg: cfg, Local: true}, nil, nil)
		})
	case "signal":
		err = withConfig(*cfgPath, func(cfg *core.Config) error {
			if len(pos) == 0 {
				return errors.New("usage: warden signal [--from WHO] [--no-tick] TEXT")
			}
			if err := core.Enqueue(cfg, *from, strings.Join(pos, " "), true); err != nil {
				return err
			}
			if *noTick {
				fmt.Println("queued for the next tick")
				return nil
			}
			return withInstance(*cfgPath, func(in *core.Instance) error { _, err := tick(ctx, in); return err })
		})
	case "github-note":
		err = withConfig(*cfgPath, func(cfg *core.Config) error { return githubNote(ctx, cfg, *event) })
	case "status":
		err = withConfig(*cfgPath, func(cfg *core.Config) error { fmt.Print(core.StatusText(cfg)); return nil })
	case "report":
		err = withConfig(*cfgPath, func(cfg *core.Config) error {
			b, err := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
			os.Stdout.Write(b)
			return err
		})
	case "approve", "decline":
		err = withInstance(*cfgPath, func(in *core.Instance) error {
			if len(pos) != 1 {
				return errors.New("usage: warden " + cmd + " ID")
			}
			pd, err := in.Approve(pos[0], cmd == "approve")
			if err == nil {
				fmt.Printf("%s: %s\n", map[bool]string{true: "approved; it runs on the next tick (or now: warden tick)", false: "declined; this is now always asked"}[cmd == "approve"], pd.What)
			}
			return err
		})
	case "feedback":
		err = withInstance(*cfgPath, func(in *core.Instance) error {
			if len(pos) != 2 {
				return errors.New("usage: warden feedback FINDING-ID noise|digest|important")
			}
			f, err := in.LastFinding(pos[0])
			if err != nil {
				return err
			}
			c, err := in.Triage.Label(f, pos[1])
			if err == nil {
				fmt.Printf("noted: «%s» → %s. `warden improve` will try to adjust the triage (through the gate).\n", f.Title, c.Want)
			}
			return err
		})
	case "improve":
		err = withInstance(*cfgPath, func(in *core.Instance) error { return improve(ctx, in, *cand, *agent) })
	case "rollback":
		err = withInstance(*cfgPath, func(in *core.Instance) error {
			v, err := in.Triage.Rollback()
			if err == nil {
				fmt.Printf("triage rolled back to v%d\n", v)
			}
			return err
		})
	case "add":
		err = withConfig(*cfgPath, func(cfg *core.Config) error { return add(ctx, cfg, pos) })
	case "verify":
		err = withConfig(*cfgPath, verify)
	case "units":
		err = withConfig(*cfgPath, func(cfg *core.Config) error { return units(cfg, *listenUnits) })
	case "packs":
		err = packs()
	default:
		usage()
	}
	if err == nil && ownerLooked[cmd] { // the owner is here: pings are welcome again (the backoff)
		if cfg, lerr := core.Load(*cfgPath); lerr == nil {
			_ = core.Touch(cfg.State, time.Now()) // best effort: the backoff only gets less precise
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "warden:", err)
		os.Exit(1)
	}
}

// ownerLooked: the commands that show the owner is paying attention.
var ownerLooked = map[string]bool{"status": true, "report": true, "approve": true, "decline": true, "feedback": true, "improve": true, "rollback": true, "signal": true}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: warden init|tick|listen|serve|mcp|signal|status|approve|decline|feedback|improve|rollback|add|verify|units|report|packs|github-note|update|version [--config warden.toml]")
	os.Exit(2)
}

func withConfig(path string, fn func(*core.Config) error) error {
	cfg, err := core.Load(path)
	if err != nil {
		return err
	}
	return fn(cfg)
}

func withInstance(path string, fn func(*core.Instance) error) error {
	return withConfig(path, func(cfg *core.Config) error {
		in, err := core.Open(cfg)
		if err != nil {
			return err
		}
		defer in.Close()
		return fn(in)
	})
}

// tickFn: one tick of a long-lived process; restart ends the process when
// the tick replaced the binary (its supervisor — launchd KeepAlive, systemd
// Restart=always — starts the new one).
func tickFn(path string, restart func()) func(context.Context) {
	return func(ctx context.Context) {
		err := withInstance(path, func(in *core.Instance) error {
			t, err := tick(ctx, in)
			if t != nil && t.Restart {
				fmt.Fprintln(os.Stderr, "warden: the binary was replaced (an update or a rollback): exiting so the supervisor starts it")
				restart()
			}
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "warden:", err)
		}
	}
}

func tick(ctx context.Context, in *core.Instance) (*core.Tick, error) {
	start := time.Now()
	t, err := in.Tick(ctx)
	if t != nil {
		n := map[core.Disposition]int{}
		acted, asked := 0, 0
		for _, it := range t.Items {
			n[it.Disp]++
			if it.Result != nil {
				acted++
			}
			if it.Pending != "" {
				asked++
			}
		}
		mode := map[bool]string{true: "dry-run", false: "act"}[in.DryRun]
		fmt.Printf("warden %s · %s · %s · sandbox %s · %d components · %d findings (report %d, digest %d, ignore %d) · acted %d · asks %d · %s\n",
			in.Cfg.Name, in.Dom.Name, mode, in.SandboxLabel(), t.Components, len(t.Items), n[core.Report], n[core.Digest], n[core.Ignore], acted, asked, time.Since(start).Round(time.Millisecond))
		if t.Report != "" {
			fmt.Println("report:", t.Report)
		} else {
			fmt.Println("quiet tick: nothing new worth telling (latest.md updated)")
		}
		for _, e := range t.Errors {
			fmt.Println("error:", e)
		}
	}
	return t, err
}

func improve(ctx context.Context, in *core.Instance, from string, useAgent bool) error {
	sk := in.Triage
	cur := sk.Triage()
	var cand core.Triage
	source := "labels"
	switch {
	case from != "":
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if cand, err = core.ParseTriage(string(b)); err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
		source = "file " + filepath.Base(from)
	case useAgent:
		if in.Brain == nil {
			return errors.New("no [brain] in warden.toml")
		}
		sc := core.Evaluate(cur, sk.Cases())
		if len(sc.Failing) == 0 {
			fmt.Println("the current triage agrees with every label: nothing to improve")
			return nil
		}
		text, err := in.Brain.Do(ctx, "You improve a triage file. Reply with TOML only.", core.AgentPrompt(cur, sc), nil)
		if err != nil {
			return err
		}
		if cand, err = core.ParseTriage(text); err != nil {
			return fmt.Errorf("the brain's candidate: %w", err)
		}
		source = "brain " + in.Cfg.Brain.Provider + " " + in.Cfg.Brain.Model
	default:
		cand = core.ProposeFromLabels(cur, sk.Cases())
	}
	o, err := sk.Gate(cand, source)
	if err != nil {
		return err
	}
	fmt.Println(o.Why)
	fmt.Printf("before: %s\nafter:  %s\n", o.Before, o.After)
	return nil
}

func add(ctx context.Context, cfg *core.Config, refs []string) error {
	if len(refs) == 0 {
		return errors.New("usage: warden add mcp:NAME[@VER] | skill:REPO//PATH[@SHA] | plugin:NAME@MARKETPLACE | pi:PKG[@VER]")
	}
	lockPath := filepath.Join(cfg.Dir, "warden.lock")
	lock, err := registry.ReadLock(lockPath)
	if err != nil {
		return err
	}
	for _, s := range refs {
		ref, err := registry.Parse(s)
		if err != nil {
			return err
		}
		l, err := registry.Resolve(ctx, ref, filepath.Join(cfg.State, "extensions"), nil)
		if err != nil {
			return err
		}
		lock.Put(l)
		fmt.Printf("locked %s → %s (%s)\n", l.Ref, l.Resolved, l.Integrity)
		if len(l.Command) > 0 {
			fmt.Printf("  command: %s\n", strings.Join(l.Command, " "))
		}
		for _, n := range l.Notes {
			fmt.Println("  note:", n)
		}
		if l.Hooks != "" {
			fmt.Printf("  hooks: off until enable_hooks = [%q] in warden.toml\n", s)
		}
	}
	if err := lock.Write(lockPath); err != nil {
		return err
	}
	fmt.Println("wrote", lockPath)
	return nil
}

func verify(cfg *core.Config) error {
	lock, err := registry.ReadLock(filepath.Join(cfg.Dir, "warden.lock"))
	if err != nil {
		return err
	}
	bad := 0
	for _, l := range lock.Entries {
		if err := registry.Verify(l); err != nil {
			bad++
			fmt.Printf("FAIL %s: %v\n", l.Ref, err)
		} else {
			fmt.Printf("ok   %s %s\n", l.Ref, l.Integrity)
		}
	}
	if len(lock.Entries) == 0 {
		fmt.Println("warden.lock: no extensions")
	}
	if bad > 0 {
		return fmt.Errorf("%d extension(s) do not match warden.lock", bad)
	}
	return nil
}

func units(cfg *core.Config, listen bool) error {
	self, _ := os.Executable()
	paths, err := core.Units(cfg, self, listen)
	if err != nil {
		return err
	}
	fmt.Println("written (not loaded — that changes your machine, it's your step):")
	for _, p := range paths {
		fmt.Println(" ", p)
	}
	fmt.Printf("\nmacOS:  cp %s ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/%s\n", paths[0], filepath.Base(paths[0]))
	if len(paths) == 3 {
		fmt.Printf("Linux:  cp %s %s ~/.config/systemd/user/ && systemctl --user enable --now %s\n", paths[1], paths[2], filepath.Base(paths[2]))
	} else {
		fmt.Printf("Linux:  cp %s ~/.config/systemd/user/ && systemctl --user enable --now %s\n", paths[1], filepath.Base(paths[1]))
	}
	return nil
}

func packs() error {
	wd, _ := os.Getwd()
	seen := map[string]bool{}
	var lines []string
	for _, root := range core.PackRoots(wd) {
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			d, err := domain.Load(filepath.Join(root, e.Name()), nil)
			if err != nil {
				continue
			}
			seen[e.Name()] = true
			lines = append(lines, fmt.Sprintf("%-16s %s\n%-16s %s", d.Name, d.Description, "", d.Dir))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return errors.New("no packs found (looked in ./packs, next to the binary, $WARDEN_PACKS)")
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

// githubNote: the github-repo pack's notifier as a hook command. It reads
// the config and the pack's params only (no state lock: it runs inside a
// tick's hook), and the event JSON from stdin.
func githubNote(ctx context.Context, cfg *core.Config, event string) error {
	if event == "" {
		return errors.New("usage: warden github-note --config warden.toml --event Report|Ask|Digest < payload.json")
	}
	d, err := domain.Load(cfg.DomainDir(), cfg.Params)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("github-note: the event JSON on stdin: %w", err)
	}
	out, err := builtin.GitHubNote(ctx, d.Params, event, payload, builtin.DefaultWeb())
	if out != "" {
		fmt.Println(out)
	}
	return err
}

func initCmd(path, pack, dir, name string) error {
	if pack == "" {
		return errors.New("usage: warden init --pack NAME|DIR [--dir D] [--name N] [--config warden.toml]")
	}
	if _, err := os.Stat(path); err == nil {
		return errors.New(path + " exists")
	}
	abs, _ := filepath.Abs(path)
	var ref, ddir string
	if st, err := os.Stat(pack); err == nil && st.IsDir() {
		ddir, _ = filepath.Abs(pack)
		ref = ddir
	} else if ddir = core.FindPack(pack, filepath.Dir(abs)); ddir != "" {
		ref = "pack:" + pack
	} else {
		return fmt.Errorf("no pack %q (see `warden packs`)", pack)
	}
	d, err := domain.Load(ddir, nil)
	if err != nil {
		return err
	}
	if dir != "" {
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
	}
	if name == "" {
		name = d.Name
		if dir != "" {
			name = filepath.Base(dir)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# warden.toml — one warden bound to one domain. See README.md.\nname   = %q\ndomain = %q\n", name, ref)
	var params []string
	for _, k := range sortedKeys(d.Params) {
		v := d.Params[k]
		if k == "root" && dir != "" {
			v = dir
		}
		params = append(params, fmt.Sprintf("%s = %q", k, v))
	}
	fmt.Fprintf(&b, "params = { %s }\n", strings.Join(params, ", "))
	b.WriteString(`mode   = "dry-run"          # act | dry-run: start by watching what it would do
wake   = { every = "6h", listen = false }
sandbox = "auto"            # auto | sandbox-exec | bwrap | none ("none" runs unsandboxed: your choice)
grants = []                 # what you allow beyond the defaults
`)
	var grants, acts []string
	for _, s := range d.Act {
		acts = append(acts, s.Name)
		if s.Floors.Grant != "" {
			grants = append(grants, fmt.Sprintf("%q (%s)", s.Floors.Grant, s.Name))
		}
	}
	if len(grants) > 0 {
		fmt.Fprintf(&b, "                            # this domain's grants: %s\n", strings.Join(grants, ", "))
	}
	b.WriteString("# Your rules: then = auto | ask | deny. Without a rule an act asks; ")
	b.WriteString("3 approvals in a row make it auto, one decline pins it to ask.\n")
	fmt.Fprintf(&b, "rules  = []                 # e.g. [{ act = %q, then = \"auto\" }]  (acts: %s)\n", first(acts), strings.Join(acts, ", "))
	b.WriteString(`
# The brain, only for judgement calls (optional):
# brain = { provider = "anthropic", model = "claude-opus-5-5" }                  # ANTHROPIC_API_KEY
# brain = { provider = "anthropic", base_url = "https://api.z.ai/api/anthropic", auth = "bearer", model = "glm-5.3", api_key_env = "ZAI_API_KEY" }
# brain = { acp = ["npx", "-y", "@agentclientprotocol/claude-agent-acp@0.89.1"] }  # any ACP agent; Claude Code: isolated, your login by reference

# Signals (the local inbox is built in: warden signal TEXT):
# [[signals]]
# name = "news"
# kind = "feed"
# url  = "https://www.cisa.gov/cybersecurity-advisories/all.xml"

# Hooks (Claude Code shape; JSON on stdin; PreAction may block):
# [[hooks]]
# event   = "Ask"
# command = ["sh", "-c", "cat >> ~/warden-asks.jsonl"]

# warden serve: POST /signal and A2A with bearer tokens
# [serve]
# addr   = "127.0.0.1:7709"
# tokens = { "change-me" = "ci" }
`)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	cfg, err := core.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (domain %s, mode dry-run) · state %s\nnext: warden tick --config %s   (and `warden units` to run it on a schedule)\n", path, d.Name, cfg.State, path)
	return nil
}

func first(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
