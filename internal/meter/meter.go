// Package meter is a hard model budget for an autonomous agent: a daily cap
// in USD, an alert on every step of spend in a day, and an alert when the
// trailing-hour burn rate runs away. The ledger lives on disk (StateDir), so
// it survives restarts and is shared by every process pointed at the same
// directory. It imports nothing from Warden.
//
// Only model ids, token counts, costs and times are stored: never request
// bodies or headers.
package meter

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Price is USD per 1M tokens.
type Price struct{ Input, Output, CacheRead, CacheWrite float64 }

// Cost of u at p, in USD.
func (p Price) Cost(u Usage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
}

// Prices maps a model id, or a prefix of one, to its price.
type Prices map[string]Price

// DefaultPrices is the Claude price list.
func DefaultPrices() Prices {
	return Prices{
		"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
		"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
		"claude-haiku-5-5":  {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
		"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	}
}

// Lookup returns the price of model: an exact match, else the longest key that
// prefixes it (claude-opus-5-5-20261001, claude-opus-5-5[1m]), else the most
// expensive known price, so an unknown model never meters cheap.
func (ps Prices) Lookup(model string) Price {
	if p, ok := ps[model]; ok {
		return p
	}
	best, bestLen := Price{}, -1
	for k, p := range ps {
		if strings.HasPrefix(model, k) && len(k) > bestLen {
			best, bestLen = p, len(k)
		}
	}
	if bestLen >= 0 {
		return best
	}
	var top Price
	for _, p := range ps {
		if p.Output > top.Output || (p.Output == top.Output && p.Input > top.Input) {
			top = p
		}
	}
	return top
}

// Config of a Ledger. A zero limit disables that limit.
type Config struct {
	PerDayUSD      float64 // hard cap per day (Allow refuses beyond it)
	AlertEveryUSD  float64 // a "threshold" alert each time today's spend crosses a multiple of this
	BurnPerHourUSD float64 // a "burn" alert when the trailing-60-minute spend exceeds this
	Prices         Prices  // default: DefaultPrices()
	StateDir       string  // required: meter.json, meter.jsonl, meter.lock
	Now            func() time.Time
	TZ             *time.Location // where the day starts; default Local
}

// Usage is the token count of one response.
type Usage struct{ Input, Output, CacheRead, CacheWrite int64 }

// Alert is something the owner should hear about.
type Alert struct {
	Kind       string // "threshold" | "burn" | "cap"
	Text       string
	SpentToday float64
	LastHour   float64
	At         time.Time
}

// ErrOverBudget: the daily cap would be exceeded.
var ErrOverBudget = errors.New("meter: daily model budget reached")

const (
	burnWindow = time.Hour
	burnQuiet  = 30 * time.Minute
	eps        = 1e-9
)

// Ledger is the persisted budget. Safe for concurrent use, and across
// processes sharing StateDir: every operation reloads the state under an
// exclusive file lock, so disk is the truth.
type Ledger struct {
	cfg     Config
	mu      sync.Mutex
	st      state // last state seen; used if a reload fails
	pending []Alert
}

type state struct {
	Day      string    `json:"day"`
	Spent    float64   `json:"spent_today"`
	Steps    int       `json:"alerted_steps"` // threshold steps already alerted today
	Capped   bool      `json:"cap_alerted"`
	LastBurn time.Time `json:"last_burn_alert,omitzero"`
	Recent   []spend   `json:"recent,omitempty"` // the trailing burn window
}

type spend struct {
	At  time.Time `json:"at"`
	USD float64   `json:"usd"`
}

type record struct {
	At         time.Time `json:"at"`
	Model      string    `json:"model"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Cost       float64   `json:"cost"`
	SpentToday float64   `json:"spent_today"`
}

// Open loads (or creates) the ledger in cfg.StateDir.
func Open(cfg Config) (*Ledger, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("meter: StateDir is required")
	}
	if cfg.Prices == nil {
		cfg.Prices = DefaultPrices()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TZ == nil {
		cfg.TZ = time.Local
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("meter: %w", err)
	}
	l := &Ledger{cfg: cfg}
	unlock, err := l.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := l.load()
	if err != nil {
		return nil, err
	}
	l.st = st
	return l, nil
}

func (l *Ledger) path(name string) string { return filepath.Join(l.cfg.StateDir, name) }

// lock takes the mutex and the cross-process file lock.
func (l *Ledger) lock() (func(), error) {
	l.mu.Lock()
	f, err := os.OpenFile(l.path("meter.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("meter: %w", err)
	}
	if err := flock(f); err != nil {
		_ = f.Close()
		l.mu.Unlock()
		return nil, fmt.Errorf("meter: lock: %w", err)
	}
	return func() {
		_ = funlock(f)
		_ = f.Close()
		l.mu.Unlock()
	}, nil
}

func (l *Ledger) load() (state, error) {
	bs, err := os.ReadFile(l.path("meter.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("meter: %w", err)
	}
	var st state
	if err := json.Unmarshal(bs, &st); err != nil {
		return state{}, fmt.Errorf("meter: %s: %w", l.path("meter.json"), err)
	}
	return st, nil
}

func (l *Ledger) save(st state) error {
	bs, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(l.cfg.StateDir, "meter.json.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(bs, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), l.path("meter.json"))
}

func (l *Ledger) appendRecord(r record) error {
	bs, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path("meter.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(bs, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// current reloads the state (falling back to the last one seen), rolls the
// day over and trims the burn window. Called under lock.
func (l *Ledger) current(now time.Time) (state, error) {
	st, err := l.load()
	if err != nil {
		st = l.st
	}
	if day := now.In(l.cfg.TZ).Format("2006-01-02"); st.Day != day {
		st.Day, st.Spent, st.Steps, st.Capped = day, 0, 0, false
	}
	keep := st.Recent[:0:0]
	for _, s := range st.Recent {
		if now.Sub(s.At) < burnWindow {
			keep = append(keep, s)
		}
	}
	st.Recent = keep
	return st, err
}

func (st state) lastHour() float64 {
	var sum float64
	for _, s := range st.Recent {
		sum += s.USD
	}
	return sum
}

// Estimate is a rough upper cost of a request: max_tokens at the output
// price plus the request size / 4 as input tokens.
func (l *Ledger) Estimate(model string, maxTokens, inputBytes int) float64 {
	p := l.cfg.Prices.Lookup(model)
	return (float64(maxTokens)*p.Output + float64(inputBytes)/4*p.Input) / 1e6
}

// Allow returns ErrOverBudget if today's spend plus estimateUSD would exceed
// the daily cap (or the cap is already reached). It also fails if the ledger
// can't be read: a budget that can't be checked is not spent.
func (l *Ledger) Allow(estimateUSD float64) error {
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := l.current(l.cfg.Now())
	if err != nil {
		return err
	}
	l.st = st
	if limit := l.cfg.PerDayUSD; limit > 0 && (st.Spent >= limit-eps || st.Spent+estimateUSD > limit+eps) {
		return fmt.Errorf("%w ($%.2f of $%.0f spent today)", ErrOverBudget, st.Spent, limit)
	}
	return nil
}

// Record adds one response's usage and returns its cost and the alerts it
// raised. Every alert is also queued for Alerts, the delivery queue.
func (l *Ledger) Record(model string, u Usage) (float64, []Alert) {
	cost := l.cfg.Prices.Lookup(model).Cost(u)
	now := l.cfg.Now()
	unlock, err := l.lock()
	if err != nil { // can't lock: still count it in this process
		l.mu.Lock()
		defer l.mu.Unlock()
		st := l.st
		alerts := l.apply(&st, now, cost)
		l.st = st
		l.pending = append(l.pending, alerts...)
		return cost, alerts
	}
	defer unlock()
	st, _ := l.current(now)
	alerts := l.apply(&st, now, cost)
	l.st = st
	_ = l.save(st) // a failed write still leaves the in-process state counted
	_ = l.appendRecord(record{At: now, Model: model, Input: u.Input, Output: u.Output,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Cost: cost, SpentToday: st.Spent})
	l.pending = append(l.pending, alerts...)
	return cost, alerts
}

// apply adds cost to st and returns the alerts that raises.
func (l *Ledger) apply(st *state, now time.Time, cost float64) []Alert {
	st.Spent += cost
	st.Recent = append(st.Recent, spend{At: now, USD: cost})
	hour := st.lastHour()
	var out []Alert
	add := func(kind, text string) {
		out = append(out, Alert{Kind: kind, Text: text, SpentToday: st.Spent, LastHour: hour, At: now})
	}
	if every := l.cfg.AlertEveryUSD; every > 0 {
		for step := int(math.Floor(st.Spent/every + eps)); st.Steps < step; {
			st.Steps++
			add("threshold", fmt.Sprintf("warden budget: $%.0f spent on models today ($%.2f; cap $%.0f)",
				float64(st.Steps)*every, st.Spent, l.cfg.PerDayUSD))
		}
	}
	if burn := l.cfg.BurnPerHourUSD; burn > 0 && hour > burn && (st.LastBurn.IsZero() || now.Sub(st.LastBurn) >= burnQuiet) {
		st.LastBurn = now
		add("burn", fmt.Sprintf("warden budget: $%.2f spent on models in the last hour (limit $%.0f/h): a runaway loop?", hour, burn))
	}
	if limit := l.cfg.PerDayUSD; limit > 0 && !st.Capped && st.Spent >= limit-eps {
		st.Capped = true
		add("cap", fmt.Sprintf("warden budget: daily cap $%.0f reached ($%.2f spent); model calls are refused until the day ends", limit, st.Spent))
	}
	return out
}

// Spent returns today's spend and the trailing-60-minute spend, in USD.
func (l *Ledger) Spent() (today, lastHour float64) {
	now := l.cfg.Now()
	unlock, err := l.lock()
	if err != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.st.Spent, l.st.lastHour()
	}
	defer unlock()
	st, _ := l.current(now)
	return st.Spent, st.lastHour()
}

// Alerts returns the alerts raised by this Ledger since the last call, and
// clears them.
func (l *Ledger) Alerts() []Alert {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.pending
	l.pending = nil
	return out
}
