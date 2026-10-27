// Package core is the Warden instance: warden.toml, its state on disk, and
// the tick over a domain: signals → observe → feeds → triage → decide → act →
// check → report.
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	Name   string `toml:"name"`
	Domain string `toml:"domain"` // a folder, or "pack:NAME" (packs/NAME)
	// Params: strings for the pack's {placeholders}; arrays and tables
	// (protected = [...], delegate_labels = { infra = "Ops" }) arrive as JSON.
	Params    map[string]string `toml:"-"`
	RawParams map[string]any    `toml:"params"`
	Mode      string            `toml:"mode"` // act | dry-run; unset: a domain's first run is dry-run
	Wake      Wake              `toml:"wake"`
	Grants    []string          `toml:"grants"`
	Rules     []policy.Rule     `toml:"rules"`
	Brain     BrainConfig       `toml:"brain"`
	// Brains: named brains (a local model, Claude …) and which does what:
	// brain_for = { review-pr = "local", work-issue = "auto" }; "auto" climbs
	// brain_auto (free first). [brain] alone means one brain for everything.
	Brains    map[string]BrainConfig `toml:"brains"`
	BrainFor  map[string]string      `toml:"brain_for"`
	BrainAuto []string               `toml:"brain_auto"`
	// Budget: a daily model-spend ledger; every paid brain call is metered.
	Budget BudgetConfig `toml:"budget"`
	// Digest: "daily" fires a Digest hook once a day with the day's summary.
	Digest  string `toml:"digest"`
	Sandbox string `toml:"sandbox"` // auto | sandbox-exec | bwrap | none (explicit: runs unsandboxed)
	State   string `toml:"state"`   // default .warden next to warden.toml
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
	// APIKeyFile: the key read from this file at call time (a mounted
	// secret), never logged; instead of api_key_env.
	APIKeyFile string `toml:"api_key_file"`
	// Free: costs nothing (a local model): not metered against the budget,
	// though its calls are counted. Default: a base_url on loopback.
	Free *bool `toml:"free"`
	// Unavailable: when this brain must not be called: "HH:MM-HH:MM"
	// windows and weekday names ("sat", "sun"), in TZ. The router skips it.
	Unavailable []string `toml:"unavailable"`
}

// BudgetConfig: [budget] — USD per day; alerts become immediate pings.
type BudgetConfig struct {
	PerDayUSD      float64            `toml:"per_day_usd"`
	AlertEveryUSD  float64            `toml:"alert_every_usd"`
	BurnPerHourUSD float64            `toml:"burn_per_hour_usd"`
	Prices         map[string]PriceUS `toml:"prices"`
	TZ             string             `toml:"tz"`
	// Upstream: the Anthropic API the metering proxy forwards to.
	Upstream string `toml:"upstream"`
	// UpstreamProxy: an HTTPS proxy on the way to Upstream ("http://host:port").
	UpstreamProxy string `toml:"upstream_proxy"`
	// FreeModels: models that cost $0 whatever the price table says.
	FreeModels []string `toml:"free_models"`
}

// PriceUS: USD per million tokens.
type PriceUS struct {
	Input      float64 `toml:"input"`
	Output     float64 `toml:"output"`
	CacheRead  float64 `toml:"cache_read"`
	CacheWrite float64 `toml:"cache_write"`
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
	c.Params = map[string]string{}
	for k, v := range c.RawParams {
		c.Params[k] = paramString(v)
	}
	for k, v := range c.Params { // files and roots: relative to warden.toml
		if v != "" && (strings.HasSuffix(k, "_file") || k == "root") {
			c.Params[k] = c.abs(v, ".")
		}
	}
	if err := c.Brain.normalize("brain", c); err != nil {
		return err
	}
	for name, b := range c.Brains {
		if name == "" || name == "auto" {
			return fmt.Errorf("brains: %q is not a usable name", name)
		}
		if b.Provider == "" && len(b.ACP) == 0 {
			return fmt.Errorf("brains.%s: a provider (anthropic, openai) or acp = [argv]", name)
		}
		if err := b.normalize("brains."+name, c); err != nil {
			return err
		}
		c.Brains[name] = b
	}
	for task, name := range c.BrainFor {
		if _, ok := c.Brains[name]; !ok && name != "auto" && (name != "default" || c.Brain.Provider == "") {
			return fmt.Errorf("brain_for.%s: no brain %q in [brains]", task, name)
		}
	}
	for _, name := range c.BrainAuto {
		if _, ok := c.Brains[name]; !ok {
			return fmt.Errorf("brain_auto: no brain %q in [brains]", name)
		}
	}
	switch c.Digest {
	case "", "daily", "off":
	default:
		return fmt.Errorf("digest must be daily or off, not %q", c.Digest)
	}
	if c.Budget.PerDayUSD < 0 || c.Budget.AlertEveryUSD < 0 || c.Budget.BurnPerHourUSD < 0 {
		return fmt.Errorf("budget: amounts must not be negative")
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

// paramString: a param value as the pack sees it (arrays and tables as JSON).
func paramString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case int64, float64, bool:
		return fmt.Sprint(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// normalize: one brain's provider, defaults and placeholders ({param} in
// base_url, model and api_key_file come from params).
func (b *BrainConfig) normalize(where string, c *Config) error {
	ex := func(s string) string { return expandParams(s, c.Params) }
	b.BaseURL, b.Model, b.APIKeyFile = ex(b.BaseURL), ex(b.Model), ex(b.APIKeyFile)
	if b.APIKeyFile != "" {
		b.APIKeyFile = c.abs(b.APIKeyFile, ".")
	}
	if b.Provider == "" && len(b.ACP) > 0 {
		b.Provider = "acp"
	}
	switch b.Provider {
	case "", "anthropic", "openai":
	case "acp":
		if len(b.ACP) == 0 {
			return fmt.Errorf("%s: provider acp needs acp = [argv]", where)
		}
		if b.Isolate == "" {
			b.Isolate = "none"
			if slices.ContainsFunc(b.ACP, func(a string) bool { return strings.Contains(a, "claude") }) {
				b.Isolate = "claude"
			}
		}
		if b.Isolate != "claude" && b.Isolate != "none" {
			return fmt.Errorf("%s: isolate must be claude or none, not %q", where, b.Isolate)
		}
	default:
		return fmt.Errorf("%s: provider must be anthropic, openai or acp, not %q", where, b.Provider)
	}
	if _, err := parseWindows(b.Unavailable, b.TZ); err != nil {
		return fmt.Errorf("%s.unavailable: %w", where, err)
	}
	return nil
}

var reParam = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

func expandParams(s string, params map[string]string) string {
	return reParam.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := params[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
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
