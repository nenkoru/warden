// Package policy decides whether Warden may act alone: floors, then the
// human's rules, then learning, then budgets, then dry-run.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Floors are an action's hard limits, enforced here and by the executor.
type Floors struct {
	Reversible bool     `toml:"reversible"`
	AlwaysAsk  bool     `toml:"always_ask"`
	Grant      string   `toml:"grant"`       // needs this in Config.Grants, else report-only
	MaxPerDay  int      `toml:"max_per_day"` // 0 = default 20
	DryRunOnly bool     `toml:"dry_run_only"`
	Where      string   `toml:"where"` // "", "worktree"
	Sandbox    bool     `toml:"sandbox"`
	Net        []string `toml:"net"` // egress allowlist when sandboxed
}

// Rule is one of the human's rules. Act and Target are globs ("" = any);
// Then is auto, ask or deny.
type Rule struct {
	Act    string `toml:"act"`
	Target string `toml:"target"`
	Then   string `toml:"then"`
}

type Config struct {
	DryRun     bool
	Grants     []string
	Rules      []Rule
	LearnAfter int // default 3, min 2
	DailyAuto  int // default 50
}

type Action struct {
	Act, Target, What string
	Params            map[string]string
	Floors            Floors
}

type Verdict struct {
	Outcome string // auto|ask|deny|report
	Would   string // dry-run: what it would do
	DryRun  bool
	Floor   string // the floor that decided, if any
	Reasons []string
}

// Learned is the approval history of one act+target key.
type Learned struct {
	Approvals int  `json:"approvals"` // in a row
	Pinned    bool `json:"pinned"`    // declined once: always ask
}

type Status struct {
	Rules      []Rule
	Learned    map[string]Learned // key: "act target"
	Day        string
	Today      int            // auto actions today
	TodayByAct map[string]int // auto actions today per act
	DryRun     bool
	Err        string // last state write error, if any
}

const (
	defaultMaxPerDay  = 20
	defaultDailyAuto  = 50
	defaultLearnAfter = 3
)

type state struct {
	Learned map[string]Learned `json:"learned"`
	Day     string             `json:"day"`
	ByAct   map[string]int     `json:"by_act"`
	Total   int                `json:"total"`
}

type Engine struct {
	cfg  Config
	dir  string
	now  func() time.Time
	mu   sync.Mutex
	st   state
	werr error
}

// Open loads (or starts) the policy state in stateDir.
func Open(cfg Config, stateDir string) (*Engine, error) {
	if cfg.LearnAfter == 0 {
		cfg.LearnAfter = defaultLearnAfter
	}
	cfg.LearnAfter = max(cfg.LearnAfter, 2)
	if cfg.DailyAuto <= 0 {
		cfg.DailyAuto = defaultDailyAuto
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("policy: state dir: %w", err)
	}
	e := &Engine{cfg: cfg, dir: stateDir, now: time.Now}
	b, err := os.ReadFile(e.path("policy.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("policy: read state: %w", err)
	default:
		if err := json.Unmarshal(b, &e.st); err != nil {
			return nil, fmt.Errorf("policy: parse %s: %w", e.path("policy.json"), err)
		}
	}
	if e.st.Learned == nil {
		e.st.Learned = map[string]Learned{}
	}
	if e.st.ByAct == nil {
		e.st.ByAct = map[string]int{}
	}
	return e, nil
}

func (e *Engine) path(name string) string { return filepath.Join(e.dir, name) }

func key(a Action) string { return a.Act + " " + a.Target }

// Decide returns the verdict for a and logs it. An auto verdict counts
// against today's budgets; if that count cannot be saved, it becomes ask.
func (e *Engine) Decide(a Action) Verdict {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollDay()
	v := e.decide(a)
	if e.cfg.DryRun {
		v.DryRun, v.Would = true, v.Outcome
		if v.Outcome == "auto" {
			v.Outcome = "ask"
			v.Reasons = append(v.Reasons, "dry-run: would act alone")
		}
	}
	if v.Outcome == "auto" {
		e.st.ByAct[a.Act]++
		e.st.Total++
		if err := e.save(); err != nil {
			v.Outcome, v.Floor = "ask", ""
			v.Reasons = append(v.Reasons, "state not saved: "+err.Error())
		}
	}
	e.log(map[string]any{"kind": "decide", "act": a.Act, "target": a.Target, "what": a.What, "verdict": v})
	return v
}

func (e *Engine) decide(a Action) Verdict {
	f := a.Floors
	var deny, ask, auto *Rule
	for i := range e.cfg.Rules {
		r := &e.cfg.Rules[i]
		if !glob(r.Act, a.Act) || !glob(r.Target, a.Target) {
			continue
		}
		switch r.Then {
		case "deny":
			deny = first(deny, r)
		case "ask":
			ask = first(ask, r)
		case "auto":
			auto = first(auto, r)
		}
	}
	if deny != nil {
		return Verdict{Outcome: "deny", Reasons: []string{"rule: " + ruleText(deny)}}
	}
	if f.DryRunOnly {
		return Verdict{Outcome: "report", Floor: "dry_run_only", Reasons: []string{"floor: dry-run only"}}
	}
	if f.Grant != "" && !slices.Contains(e.cfg.Grants, f.Grant) {
		return Verdict{Outcome: "report", Floor: "grant", Reasons: []string{"floor: grant " + f.Grant + " missing"}}
	}
	if f.AlwaysAsk {
		return Verdict{Outcome: "ask", Floor: "always_ask", Reasons: []string{"floor: always ask"}}
	}
	l := e.st.Learned[key(a)]
	if ask != nil {
		return Verdict{Outcome: "ask", Reasons: []string{"rule: " + ruleText(ask)}}
	}
	if l.Pinned {
		return Verdict{Outcome: "ask", Reasons: []string{"pinned: declined before"}}
	}
	var why string
	switch {
	case auto != nil:
		why = "rule: " + ruleText(auto)
	case l.Approvals >= e.cfg.LearnAfter:
		why = fmt.Sprintf("learned: %d approvals in a row", l.Approvals)
	default:
		return Verdict{Outcome: "ask", Reasons: []string{fmt.Sprintf("learning: %d of %d", l.Approvals, e.cfg.LearnAfter)}}
	}
	perDay := f.MaxPerDay
	if perDay <= 0 {
		perDay = defaultMaxPerDay
	}
	if n := e.st.ByAct[a.Act]; n >= perDay {
		return Verdict{Outcome: "ask", Floor: "max_per_day", Reasons: []string{why, fmt.Sprintf("velocity: %d of %d today for %s", n, perDay, a.Act)}}
	}
	if e.st.Total >= e.cfg.DailyAuto {
		return Verdict{Outcome: "ask", Reasons: []string{why, fmt.Sprintf("budget: %d of %d auto actions today", e.st.Total, e.cfg.DailyAuto)}}
	}
	return Verdict{Outcome: "auto", Reasons: []string{why}}
}

// first keeps the first matching rule of each kind.
func first(have, r *Rule) *Rule {
	if have != nil {
		return have
	}
	return r
}

func ruleText(r *Rule) string {
	s := orAny(r.Act)
	if r.Target != "" {
		s += " " + r.Target
	}
	return s + " → " + r.Then
}

func orAny(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// Learn records the human's answer: LearnAfter approvals in a row of the
// same act+target make it auto; one decline pins it to ask.
func (e *Engine) Learn(a Action, approved bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := key(a)
	l := e.st.Learned[k]
	if approved {
		l.Approvals++
	} else {
		l.Approvals, l.Pinned = 0, true
	}
	e.st.Learned[k] = l
	e.werr = e.save()
	e.log(map[string]any{"kind": "learn", "act": a.Act, "target": a.Target, "approved": approved, "learned": l})
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollDay()
	s := Status{
		Rules:      slices.Clone(e.cfg.Rules),
		Learned:    make(map[string]Learned, len(e.st.Learned)),
		Day:        e.st.Day,
		Today:      e.st.Total,
		TodayByAct: make(map[string]int, len(e.st.ByAct)),
		DryRun:     e.cfg.DryRun,
	}
	for k, v := range e.st.Learned {
		s.Learned[k] = v
	}
	for k, v := range e.st.ByAct {
		s.TodayByAct[k] = v
	}
	if e.werr != nil {
		s.Err = e.werr.Error()
	}
	return s
}

func (e *Engine) rollDay() {
	if d := e.now().Format(time.DateOnly); d != e.st.Day {
		e.st.Day, e.st.Total, e.st.ByAct = d, 0, map[string]int{}
	}
}

func (e *Engine) save() error {
	b, err := json.MarshalIndent(e.st, "", "  ")
	if err != nil {
		return fmt.Errorf("policy: encode state: %w", err)
	}
	if err := writeAtomic(e.path("policy.json"), b); err != nil {
		return fmt.Errorf("policy: write state: %w", err)
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// log appends one JSON line to policy.jsonl; a failed log never blocks a decision.
func (e *Engine) log(rec map[string]any) {
	rec["at"] = e.now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(e.path("policy.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		e.werr = err
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		e.werr = err
	}
}

var globCache sync.Map // pattern → *regexp.Regexp

// glob matches s against a pattern where * is any run of characters
// (slashes included) and ? one character; "" and "*" match anything.
func glob(pat, s string) bool {
	if pat == "" || pat == "*" {
		return true
	}
	if !strings.ContainsAny(pat, "*?") {
		return pat == s
	}
	re, ok := globCache.Load(pat)
	if !ok {
		var b strings.Builder
		b.WriteString("^")
		for _, r := range pat {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		re, _ = globCache.LoadOrStore(pat, regexp.MustCompile(b.String()))
	}
	return re.(*regexp.Regexp).MatchString(s)
}
