// Package core is the Warden instance: warden.toml, its state on disk, and
// the tick over a domain: signals → observe → feeds → triage → decide → act →
// check → report.
package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // brain.tz and quiet hours must work where the OS has no zoneinfo (e.g. a minimal container)

	"github.com/pelletier/go-toml/v2"

	"github.com/nenkoru/warden/internal/hooks"
	"github.com/nenkoru/warden/internal/policy"
)

// Config is warden.toml.
type Config struct {
	Name    string            `toml:"name"`
	Domain  string            `toml:"domain"` // a folder, or "pack:NAME" (packs/NAME)
	Params  map[string]string `toml:"params"`
	Mode    string            `toml:"mode"` // act | dry-run; unset: a domain's first run is dry-run
	Wake    Wake              `toml:"wake"`
	Grants  []string          `toml:"grants"`
	Rules   []policy.Rule     `toml:"rules"`
	Brain   BrainConfig       `toml:"brain"`
	Sandbox string            `toml:"sandbox"` // auto | sandbox-exec | bwrap | none (explicit: runs unsandboxed)
	State   string            `toml:"state"`   // default .warden next to warden.toml
	// Replies on the owner's behalf (signal reply commands) are dry-run until true.
	Replies     bool           `toml:"replies"`
	Signals     []SignalSource `toml:"signals"`
	Hooks       []hooks.Hook   `toml:"hooks"`
	Extensions  []string       `toml:"extensions"`   // registry refs, pinned in warden.lock
	EnableHooks []string       `toml:"enable_hooks"` // extensions whose hooks may run
	Serve       ServeConfig    `toml:"serve"`
	// Backoff: ping less when the owner ignores the pings (backoff.go).
	Backoff Backoff `toml:"backoff"`

	Path string `toml:"-"` // warden.toml, absolute
	Dir  string `toml:"-"` // its folder
}

type Wake struct {
	Every  string `toml:"every"`  // between ticks (default 6h)
	Listen bool   `toml:"listen"` // stay awake on stream signals
}

// BrainConfig: an Anthropic/OpenAI-compatible endpoint, or an ACP agent argv.
type BrainConfig struct {
	Provider  string   `toml:"provider"`
	BaseURL   string   `toml:"base_url"`
	Model     string   `toml:"model"`
	APIKeyEnv string   `toml:"api_key_env"`
	Auth      string   `toml:"auth"`
	ACP       []string `toml:"acp"`
	// Isolate (acp): "claude" (default when the argv names Claude Code) gives
	// the agent its own config home, none of the owner's settings, MCP
	// servers, CLAUDE.md or memory, and the owner's login only by reference;
	// "none" passes Warden's whole environment.
	Isolate string `toml:"isolate"`
	// Quiet: "HHMM-HHMM" (in TZ, default local) when the model is not called
	// at all — e.g. a shared GPU's night window. Findings still come; the
	// judgement (RCA, classification, fixes) waits.
	Quiet string `toml:"quiet"`
	TZ    string `toml:"tz"`
	// Net (acp): the hosts the agent may reach (default for claude: the
	// Anthropic API and login hosts, and the npm registry for npx).
	Net []string `toml:"net"`
}

type ServeConfig struct {
	Addr    string            `toml:"addr"`    // default 127.0.0.1:7709
	Tokens  map[string]string `toml:"tokens"`  // bearer token → sender
	Owners  []string          `toml:"owners"`  // senders whose orders count
	Trusted []string          `toml:"trusted"` // senders whose word counts as advice
}

// Load reads warden.toml and fills the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path, _ = filepath.Abs(path)
	c.Dir = filepath.Dir(c.Path)
	return &c, c.normalize()
}

func (c *Config) normalize() error {
	if c.Domain == "" {
		return fmt.Errorf("warden.toml: domain (a folder or pack:NAME)")
	}
	if c.Name == "" {
		c.Name = filepath.Base(c.DomainDir())
	}
	switch c.Mode {
	case "", "act", "dry-run":
	default:
		return fmt.Errorf("mode must be act or dry-run, not %q", c.Mode)
	}
	if c.Wake.Every == "" {
		c.Wake.Every = "6h"
	}
	if d, err := time.ParseDuration(c.Wake.Every); err != nil || d < time.Minute {
		return fmt.Errorf("wake.every: %q (a Go duration ≥ 1m)", c.Wake.Every)
	}
	switch c.Sandbox {
	case "":
		c.Sandbox = "auto"
	case "auto", "sandbox-exec", "bwrap", "none":
	default:
		return fmt.Errorf("sandbox must be auto, sandbox-exec, bwrap or none, not %q", c.Sandbox)
	}
	for _, r := range c.Rules {
		if r.Then != "auto" && r.Then != "ask" && r.Then != "deny" {
			return fmt.Errorf("rule %+v: then must be auto, ask or deny", r)
		}
	}
	if c.Brain.Provider == "" && len(c.Brain.ACP) > 0 {
		c.Brain.Provider = "acp"
	}
	switch c.Brain.Provider {
	case "", "anthropic", "openai":
	case "acp":
		if len(c.Brain.ACP) == 0 {
			return fmt.Errorf("brain: provider acp needs acp = [argv]")
		}
		if c.Brain.Isolate == "" {
			c.Brain.Isolate = "none"
			if slices.ContainsFunc(c.Brain.ACP, func(a string) bool { return strings.Contains(a, "claude") }) {
				c.Brain.Isolate = "claude"
			}
		}
		if c.Brain.Isolate != "claude" && c.Brain.Isolate != "none" {
			return fmt.Errorf("brain: isolate must be claude or none, not %q", c.Brain.Isolate)
		}
	default:
		return fmt.Errorf("brain: provider must be anthropic, openai or acp, not %q", c.Brain.Provider)
	}
	if err := c.Backoff.normalize(); err != nil {
		return err
	}
	c.State = c.abs(c.State, ".warden")
	if c.Serve.Addr == "" {
		c.Serve.Addr = "127.0.0.1:7709"
	}
	for i := range c.Signals {
		if err := c.Signals[i].normalize(c); err != nil {
			return err
		}
	}
	return nil
}

// DomainDir: the pack folder. "pack:NAME" looks in ./packs, next to the
// binary, and in WARDEN_PACKS.
func (c *Config) DomainDir() string {
	if name, ok := strings.CutPrefix(c.Domain, "pack:"); ok {
		if p := FindPack(name, c.Dir); p != "" {
			return p
		}
		return filepath.Join(c.Dir, "packs", name)
	}
	return c.abs(c.Domain, ".")
}

// PackRoots: where packs are looked up, first wins.
func PackRoots(near string) []string {
	var roots []string
	if e := os.Getenv("WARDEN_PACKS"); e != "" {
		roots = append(roots, filepath.SplitList(e)...)
	}
	up := func(d string) {
		for ; d != ""; d = filepath.Dir(d) {
			roots = append(roots, filepath.Join(d, "packs"))
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	up(near)
	if wd, err := os.Getwd(); err == nil {
		up(wd)
	}
	if exe, err := os.Executable(); err == nil {
		for d := filepath.Dir(exe); ; d = filepath.Dir(d) {
			roots = append(roots, filepath.Join(d, "packs"))
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return roots
}

func FindPack(name, near string) string {
	for _, r := range PackRoots(near) {
		if _, err := os.Stat(filepath.Join(r, name, "domain.toml")); err == nil {
			p, _ := filepath.Abs(filepath.Join(r, name))
			return p
		}
	}
	return ""
}

// abs: ~ and paths relative to warden.toml.
func (c *Config) abs(p, def string) string {
	if p == "" {
		p = def
	}
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.Dir, p)
	}
	return filepath.Clean(p)
}

func (c *Config) Every() time.Duration { d, _ := time.ParseDuration(c.Wake.Every); return d }
