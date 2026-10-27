package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/box"
	"github.com/nenkoru/warden/internal/brain"
)

// The ACP brain is an outside agent (Claude Code through claude-agent-acp,
// Gemini CLI, ...). It runs like any act: in the sandbox, writing only its
// own folder and the worktree it was given (brain.WithDir), reaching only
// its model's hosts, and every permission it asks for is answered by the
// floors: reads and edits inside that worktree, nothing else (no shell, no
// web). With isolate = "claude" it also gets none of the owner's Claude
// Code setup: its own config home, no settings, MCP servers, plugins,
// hooks, CLAUDE.md or memory of the owner's; only their login, by reference.

// claudeNet: Claude Code's API and login hosts, and npm for npx.
var claudeNet = []string{"api.anthropic.com", "*.anthropic.com", "claude.ai", "*.claude.ai", "*.claude.com", "registry.npmjs.org"}

// passEnv: what of Warden's environment an isolated agent sees.
var passEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TERM", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

func (in *Instance) acpBrain(name string, b BrainConfig, paid bool) brain.Brain {
	work := filepath.Join(in.Cfg.State, "work")
	home := filepath.Join(in.Cfg.State, "agents", "acp")
	if name != "default" {
		home = filepath.Join(in.Cfg.State, "agents", name)
	}
	_ = os.MkdirAll(home, 0o700) // a failure surfaces when the agent starts
	cfg := brain.ACPConfig{Command: b.ACP, Cwd: home, Confine: true, Timeout: 20 * time.Minute,
		// the floors: read and edit inside the worktree (Confine), never anything else
		Allow: func(kind string, paths []string) bool {
			if kind != "read" && kind != "edit" {
				return false
			}
			for _, p := range paths {
				if !strings.HasPrefix(p, work+string(filepath.Separator)) {
					return false
				}
			}
			return true
		}}
	if lf, err := os.OpenFile(filepath.Join(home, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		cfg.Log = lf
		in.cleanups = append(in.cleanups, func() { lf.Close() })
	}
	net := b.Net
	keychain := false
	var loopback []int
	if b.Isolate == "claude" {
		cfg.CleanEnv, cfg.Env = true, claudeEnv(home)
		cfg.SessionMeta = map[string]any{"claudeCode": map[string]any{"options": map[string]any{"settingSources": []string{}, "strictMcpConfig": true}}}
		if paid && in.Ledger != nil { // metered: the agent's API calls go through the meter's local proxy
			addr, port, err := in.meterProxy()
			if err != nil {
				in.note("brain " + name + ": the metering proxy failed (" + err.Error() + "): the agent is refused")
				return failing{fmt.Errorf("brain %s: metering proxy: %w", name, err)}
			}
			cfg.Env = append(cfg.Env, "ANTHROPIC_BASE_URL=http://"+addr)
			loopback = []int{port}
			if net == nil {
				net = meteredNet
			}
		}
		if net == nil {
			net = claudeNet
		}
		// no token in the environment: the owner's login, read from the keychain by reference
		keychain = runtime.GOOS == "darwin" && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == ""
		in.note("brain: " + b.ACP[len(b.ACP)-1] + " isolated (own config home, none of the owner's settings, MCP servers or CLAUDE.md), sandboxed, net: " + strings.Join(net, ", "))
	}
	runner := in.Runner
	deny := in.secretFiles()
	keyFile := b.APIKeyFile
	cfg.Wrap = func(argv, env []string, cwd string) ([]string, []string, func(), error) {
		if keyFile != "" { // read at call time: a rotated secret is picked up
			k, err := os.ReadFile(keyFile)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("api_key_file: %w", errors.Unwrap(err))
			}
			env = append(env, "ANTHROPIC_API_KEY="+strings.TrimSpace(string(k)))
		}
		p := box.Policy{Name: "brain", ReadWrite: []string{home, cwd}, Net: net, Keychain: keychain, Loopback: loopback, Deny: deny}
		if net == nil {
			p.Net = []string{} // the proxy refuses everything: no network at all would also stop the agent's own API
		}
		return box.Command(runner, p, argv, env)
	}
	return authHint{brain.ACP(cfg)}
}

// meteredNet: a metered Claude Code reaches its API through the loopback
// proxy; beyond that only the package registries (npx, Go modules).
var meteredNet = []string{"registry.npmjs.org", "proxy.golang.org", "sum.golang.org"}

// failing: a brain that can't start, with the reason.
type failing struct{ err error }

func (f failing) Do(context.Context, string, string, []brain.Tool) (string, error) { return "", f.err }

// authHint: an agent that is not logged in says so in the report with the
// owner's one-line remedy, not as a bare protocol error.
type authHint struct{ brain.Brain }

func (a authHint) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	out, err := a.Brain.Do(ctx, system, task, tools)
	if err != nil && (strings.Contains(err.Error(), "Authentication required") || strings.Contains(out, "OAuth access token has expired") || strings.Contains(out, "401")) {
		err = fmt.Errorf("%w — the agent is not logged in: run `claude` once to refresh your login, or export CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) for unattended runs", err)
	}
	return out, err
}

// claudeEnv: Claude Code with its own config home. Authentication: a token
// in Warden's environment (ANTHROPIC_API_KEY, or CLAUDE_CODE_OAUTH_TOKEN from
// `claude setup-token`) is used as is; without one, the empty
// CLAUDE_SECURESTORAGE_CONFIG_DIR keeps secure storage (the macOS keychain
// item) where it is while everything else moves: only the credential is
// shared, never settings or MCP config.
func claudeEnv(home string) []string {
	var env []string
	token := false
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if slices.Contains(passEnv, k) || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
			token = token || (k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_OAUTH_TOKEN") && v != ""
		}
	}
	cfgHome := filepath.Join(home, "claude")
	_ = os.MkdirAll(cfgHome, 0o700)
	store := ""
	if token {
		store = cfgHome
	}
	return append(env,
		"CLAUDE_CONFIG_DIR="+cfgHome,
		"CLAUDE_SECURESTORAGE_CONFIG_DIR="+store,
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"npm_config_cache="+filepath.Join(home, ".npm"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
}

// quietBrain: no model call inside the quiet window ([brain] quiet, tz).
type quietBrain struct {
	brain.Brain
	from, to int // minutes of the day
	loc      *time.Location
	now      func() time.Time
}

func newQuietBrain(b brain.Brain, window, tz string, now func() time.Time) (brain.Brain, error) {
	f, t, ok := strings.Cut(window, "-")
	from, err1 := hhmm(f)
	to, err2 := hhmm(t)
	if !ok || err1 != nil || err2 != nil {
		return nil, fmt.Errorf("brain.quiet: %q is not HHMM-HHMM", window)
	}
	loc := time.Local
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("brain.tz: %w", err)
		}
		loc = l
	}
	return quietBrain{b, from, to, loc, now}, nil
}

func hhmm(s string) (int, error) {
	if len(s) != 4 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[2]-'0')*10+int(s[3]-'0')
	if h > 23 || m > 59 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' || s[2] < '0' || s[2] > '9' || s[3] < '0' || s[3] > '9' {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}

func (q quietBrain) quiet() bool {
	t := q.now().In(q.loc)
	m := t.Hour()*60 + t.Minute()
	if q.from <= q.to {
		return m >= q.from && m < q.to
	}
	return m >= q.from || m < q.to // across midnight
}

func (q quietBrain) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	if q.quiet() {
		return "", fmt.Errorf("brain: quiet hours (%02d:%02d–%02d:%02d %s): no model call; the judgement waits", q.from/60, q.from%60, q.to/60, q.to%60, q.loc)
	}
	return q.Brain.Do(ctx, system, task, tools)
}
