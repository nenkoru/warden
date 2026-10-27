package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/warden/internal/a2a"
	"github.com/nenkoru/warden/internal/mcpx"
)

// Control: the instance as other harnesses see it (warden mcp, warden serve).
// Reads don't take the tick lock; answers do, briefly.
type Control struct {
	Cfg   *Config
	Local bool          // signals come from the owner (stdio MCP on the owner's machine)
	Wake  chan struct{} // nudged after a signal (serve/listen tick soon); may be nil
}

var _ mcpx.Control = (*Control)(nil)

func (c *Control) pending() map[string]*Pend {
	m := map[string]*Pend{}
	_ = readJSON(filepath.Join(c.Cfg.State, "pending.json"), &m)
	return m
}

func (c *Control) Status() string { c.looked(); return StatusText(c.Cfg) }

// looked: the owner (through a local MCP client) paid attention: the backoff resets.
func (c *Control) looked() {
	if c.Local {
		_ = Touch(c.Cfg.State, time.Now())
	}
}

func (c *Control) Report() string {
	c.looked()
	b, err := os.ReadFile(filepath.Join(c.Cfg.State, "reports", "latest.md"))
	if err != nil {
		return "no report yet (run `warden tick`)"
	}
	return string(b)
}

func (c *Control) Pending() []mcpx.PendingItem {
	var out []mcpx.PendingItem
	for _, p := range sortedPend(c.pending()) {
		if !p.Approved {
			out = append(out, mcpx.PendingItem{ID: p.ID, What: p.What, Why: strings.Join(p.Reasons, "; ")})
		}
	}
	return out
}

func (c *Control) Approve(id string, yes bool) (string, error) {
	in, err := Open(c.Cfg)
	if err != nil {
		return "", err
	}
	defer in.Close()
	pd, err := in.Approve(id, yes)
	if err != nil {
		return "", err
	}
	c.looked()
	c.nudge()
	if yes {
		return "approved: " + pd.What + " (runs on the next tick)", nil
	}
	return "declined: " + pd.What + " (this is always asked from now on)", nil
}

func (c *Control) Signal(from, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty signal")
	}
	if from == "" {
		from = "owner"
	}
	if err := Enqueue(c.Cfg, from, text, c.Local); err != nil {
		return "", err
	}
	c.looked()
	c.nudge()
	return "queued for the next tick", nil
}

func (c *Control) nudge() {
	if c.Wake != nil {
		select {
		case c.Wake <- struct{}{}:
		default:
		}
	}
}

func sortedPend(m map[string]*Pend) []*Pend {
	out := make([]*Pend, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created < out[j].Created || out[i].Created == out[j].Created && out[i].ID < out[j].ID
	})
	return out
}

// StatusText: pending decisions, triage, policy and the last report, read
// without the lock.
func StatusText(cfg *Config) string {
	var b strings.Builder
	t := OpenTriage(cfg.State).Triage()
	fmt.Fprintf(&b, "warden %s · domain %s · mode %s · sandbox %s · state %s\n", cfg.Name, cfg.Domain, or(cfg.Mode, "unset (first run dry-run)"), cfg.Sandbox, cfg.State)
	fmt.Fprintf(&b, "triage v%d (%d rules) %s\n", t.Version, len(t.Rules), t.Note)
	var pol struct {
		Learned map[string]struct {
			Approvals int
			Pinned    bool
		} `json:"learned"`
		Total int `json:"total"`
	}
	_ = readJSON(filepath.Join(cfg.State, "policy.json"), &pol)
	for _, r := range cfg.Rules {
		fmt.Fprintf(&b, "rule: %s %s → %s\n", or(r.Act, "*"), r.Target, r.Then)
	}
	for k, l := range pol.Learned {
		fmt.Fprintf(&b, "learned: %s — %d approvals in a row, pinned %v\n", k, l.Approvals, l.Pinned)
	}
	c := &Control{Cfg: cfg}
	ps := sortedPend(c.pending())
	if len(ps) == 0 {
		b.WriteString("pending: none\n")
	}
	for _, p := range ps {
		state := "`warden approve " + p.ID + "` · `warden decline " + p.ID + "`"
		if p.Approved {
			state = "approved: runs on the next tick"
		}
		fmt.Fprintf(&b, "pending %s: %s\n  why: %s\n  %s\n", p.ID, p.What, strings.Join(p.Reasons, "; "), state)
	}
	fmt.Fprintf(&b, "%s\n", backoffLine(cfg))
	fmt.Fprintf(&b, "%s\n", sourcesLine(OpenTriage(cfg.State).Cases()))
	fmt.Fprintf(&b, "latest report: %s\n", filepath.Join(cfg.State, "reports", "latest.md"))
	return b.String()
}

// Handler: warden serve. POST /signal (bearer token → sender), A2A at /a2a
// (same tokens), GET /report (bearer).
func (c *Control) Handler(baseURL string) http.Handler {
	mux := http.NewServeMux()
	sender := func(r *http.Request) (string, bool) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			return "", false
		}
		for k, v := range c.Cfg.Serve.Tokens {
			if subtle.ConstantTimeCompare([]byte(k), []byte(tok)) == 1 {
				return v, true
			}
		}
		return "", false
	}
	mux.HandleFunc("POST /signal", func(w http.ResponseWriter, r *http.Request) {
		from, ok := sender(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		text := string(body)
		var j struct{ Text string }
		if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal(body, &j) == nil && j.Text != "" {
			text = j.Text
		}
		out, err := c.Signal(from, text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, out+"\n")
	})
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := sender(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, c.Report())
	})
	card := a2a.NewCard("warden-"+c.Cfg.Name, "Warden for "+c.Cfg.Domain+": send it news, alerts or requests; it answers through its tick and report.", strings.TrimRight(baseURL, "/")+"/a2a")
	h := a2a.Handler(card, c.Cfg.Serve.Tokens, func(_ context.Context, from, text string) (string, error) {
		return c.Signal(from, text)
	})
	mux.Handle("/a2a", h)
	mux.Handle("/.well-known/agent-card.json", h)
	return mux
}

// Serve runs the HTTP endpoint and ticks on its timer and after signals.
func Serve(ctx context.Context, cfg *Config, tick func(context.Context), log io.Writer) error {
	wake := make(chan struct{}, 1)
	c := &Control{Cfg: cfg, Wake: wake}
	srv := &http.Server{Addr: cfg.Serve.Addr, Handler: c.Handler("http://" + cfg.Serve.Addr), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(log, "warden serve: http://%s (POST /signal, /a2a, GET /report)\n", cfg.Serve.Addr)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); Listen(ctx, cfg, wake, tick, log) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	wg.Wait()
	return nil
}
