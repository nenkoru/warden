package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Interrupt backoff: if the owner ignores the pings, Warden pings less.
//
// A ping is a fired Report or Ask hook (whatever the owner wired them to: a
// chat message, a notification, mail). Attention is anything the owner does
// with Warden: a CLI command (status, report, approve, decline, feedback,
// improve, rollback, signal), the same through its local MCP server, or an
// owner-tier signal. Pings sent since the last attention are "ignored".
// After `after` ignored pings, the next one waits at least the wake interval,
// then twice that, and so on up to `max`. Nothing is lost in the pause: the
// report file is still written every tick, the news stays new (not marked
// told) and goes out in the next ping. A critical finding never waits.

// Backoff: [backoff] in warden.toml.
type Backoff struct {
	After int    `toml:"after"` // ignored pings before backing off (default 2; -1: never back off)
	Max   string `toml:"max"`   // the longest pause (default 168h)
}

// Pings: what was sent, for the backoff (pings.json).
type Pings struct {
	Sent []int64 `json:"sent,omitempty"` // the last pings (unix), newest last
	Held int     `json:"held,omitempty"` // ticks with news held back since the last ping
}

const keepPings = 32

func (b *Backoff) normalize() error {
	if b.After == 0 {
		b.After = 2
	}
	if b.Max == "" {
		b.Max = "168h"
	}
	if _, err := time.ParseDuration(b.Max); err != nil {
		return fmt.Errorf("backoff.max: %w", err)
	}
	return nil
}

// Touch records the owner's attention (a command or a signal of theirs): pings
// are welcome again.
func Touch(state string, now time.Time) error {
	return writeFile(filepath.Join(state, "attention"), []byte(strconv.FormatInt(now.Unix(), 10)))
}

func lastTouch(state string) int64 {
	b, _ := os.ReadFile(filepath.Join(state, "attention"))
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// ignored: the pings sent since the owner last paid attention.
func (p Pings) ignored(touch int64) int {
	n := 0
	for _, s := range p.Sent {
		if s > touch {
			n++
		}
	}
	return n
}

// pause: how long after the last ping the next one must wait, and why
// (0, "" when it may go now).
func pause(cfg *Config, p Pings, touch int64) (time.Duration, string) {
	b := cfg.Backoff
	n := p.ignored(touch)
	if b.After < 0 || n < b.After || len(p.Sent) == 0 {
		return 0, ""
	}
	max, _ := time.ParseDuration(b.Max)
	d := cfg.Every()
	for i := b.After; i < n && d < max; i++ {
		d *= 2
	}
	d = min(d, max)
	return d, fmt.Sprintf("%d pings in a row without an answer", n)
}

// nextPing: when the next ping may go (zero time: now).
func nextPing(cfg *Config, p Pings, touch int64) (time.Time, string) {
	d, why := pause(cfg, p, touch)
	if d == 0 {
		return time.Time{}, ""
	}
	return time.Unix(p.Sent[len(p.Sent)-1], 0).Add(d), why
}

// mayPing: whether a ping may go now; urgent (a critical finding) always may.
func (in *Instance) mayPing(now time.Time, urgent bool) (bool, string) {
	if urgent {
		return true, ""
	}
	at, why := nextPing(in.Cfg, in.St.Pings, lastTouch(in.Cfg.State))
	if at.IsZero() || !now.Before(at) {
		return true, ""
	}
	return false, fmt.Sprintf("%s: the next not before %s (any command of yours lifts the pause; critical items go at once)", why, at.Format("2006-01-02 15:04"))
}

func (in *Instance) pinged(now time.Time) {
	p := &in.St.Pings
	if n := len(p.Sent); n > 0 && p.Sent[n-1] == now.Unix() {
		return // one tick, one ping
	}
	p.Sent = append(p.Sent, now.Unix())
	if len(p.Sent) > keepPings {
		p.Sent = p.Sent[len(p.Sent)-keepPings:]
	}
	p.Held = 0
}

// backoffLine: the state of the backoff in words, for `warden status`.
func backoffLine(cfg *Config) string {
	var p Pings
	_ = readJSON(filepath.Join(cfg.State, "pings.json"), &p)
	touch := lastTouch(cfg.State)
	n := p.ignored(touch)
	at, _ := nextPing(cfg, p, touch)
	switch {
	case len(p.Sent) == 0:
		return "pings: none sent yet"
	case at.IsZero():
		return fmt.Sprintf("pings: %d since you last looked; the next goes when there is news", n)
	}
	return fmt.Sprintf("pings: %d since you last looked → backing off: the next not before %s (%d ticks held); any command resets it", n, at.Format("2006-01-02 15:04"), p.Held)
}

// ping fires an interrupting hook (Ask) unless this tick's pings are held;
// it says whether it went out.
func (in *Instance) ping(ctx context.Context, now time.Time, event, subject string, payload map[string]any) bool {
	if in.quiet != "" {
		in.noteOnce("🔕 questions wait in the report, without a ping — " + in.quiet)
		return false
	}
	if !in.hasHook(event) {
		return true // nothing to send: nothing to hold either
	}
	in.fire(ctx, event, subject, payload)
	in.pinged(now)
	return true
}

func (in *Instance) hasHook(event string) bool {
	for _, h := range in.Hooks {
		if h.Event == event {
			return true
		}
	}
	return false
}
