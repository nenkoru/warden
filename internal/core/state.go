package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// State: everything the warden remembers, in its state folder.
//
//	lock            one tick at a time (flock)
//	journal.jsonl   append-only: every tick, step begin/end (crash recovery)
//	tasks.jsonl     append-only, with params.experiment_log = "on": one record per act on a task (issue, PR,
//	                phase, brain, model, ok, wall time, tokens, failure categories, the act's data) and the
//	                coding brain's idle time (kind "idle"); best effort, 0600 (internal/experiment, tasks.go)
//	idle.json       the previous tick's time, for the idle records
//	seen.json       finding fingerprints: first/last seen, reported or not
//	feeds.json      the last release seen per watched repository
//	pending.json    actions waiting for the owner's yes/no
//	done.json       finished actions by idempotency key
//	signals.json    signals and feed items already read; watch.json: advisories from signals
//	domains.json    domains that ran once (a domain's first run is dry-run unless mode is set)
//	policy.json     the policy engine's state (internal/policy)
//	skills/         the triage skill, with history; evals/: the owner's labels
//	reports/        a report per tick worth one; latest.md always
//	work/           temporary git worktrees; proposals/, prs/: files for the human
type State struct {
	Dir string

	Seen    map[string]*Seen
	Feeds   map[string]string
	Pending map[string]*Pend
	Done    map[string]*Result
	Handled map[string]bool
	Watch   map[string]*Advisory
	Domains map[string]int64
	Pings   Pings // the interrupt backoff (backoff.go)
	// BrainDown: brains whose upstream refused them, until (unix).
	BrainDown map[string]int64
	Digest    DigestState

	lock *os.File
}

type Seen struct {
	First    int64  `json:"first"`
	Last     int64  `json:"last"`
	Reported int64  `json:"reported,omitempty"`
	Title    string `json:"title"`
}

// Pend: an action (or a signal's order) left to the owner.
type Pend struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"` // action | order
	Act      string            `json:"act,omitempty"`
	Target   string            `json:"target,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	What     string            `json:"what"`
	Finding  string            `json:"finding"`
	Reasons  []string          `json:"reasons"`
	Created  int64             `json:"created"`
	Approved bool              `json:"approved,omitempty"` // the next tick runs it
	Told     bool              `json:"told,omitempty"`     // its Ask hook went out (not held by the backoff)
}

// Result: what an action did.
type Result struct {
	Key     string   `json:"key"`
	OK      bool     `json:"ok"`
	Summary string   `json:"summary"`
	Branch  string   `json:"branch,omitempty"`
	File    string   `json:"file,omitempty"` // a PR description or proposal
	Log     []string `json:"log,omitempty"`
	At      int64    `json:"at"`
	Refused bool     `json:"refused,omitempty"` // not run (no sandbox, a hook blocked): not a failure
	// Deferred: nothing happened yet (CI running, a brain off-hours); never stored.
	Deferred bool `json:"-"`
	// Data: the act's task fields for the experiment log (tasks.go); never stored here.
	Data map[string]string `json:"-"`
}

// DigestState: the day's lines for the daily digest (pause.go).
type DigestState struct {
	Day   string   `json:"day,omitempty"`
	Lines []string `json:"lines,omitempty"`
	Spend string   `json:"spend,omitempty"`
	Extra []string `json:"extra,omitempty"` // lines for the digest's own section (AddDigestExtra)
}

// Advisory: "versions below Fixed are vulnerable", from a signal.
type Advisory struct {
	Name, Fixed, Ecosystem, Note, From string
	At                                 int64
}

// OpenState opens the state folder and takes the tick lock.
func OpenState(dir string) (*State, error) {
	for _, d := range []string{"", "reports", "skills/history", "evals", "work", "inbox"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another warden tick is running here")
	}
	s := &State{Dir: dir, lock: f, Seen: map[string]*Seen{}, Feeds: map[string]string{}, Pending: map[string]*Pend{},
		Done: map[string]*Result{}, Handled: map[string]bool{}, Watch: map[string]*Advisory{}, Domains: map[string]int64{}, BrainDown: map[string]int64{}}
	for name, v := range s.files() {
		if err := readJSON(filepath.Join(dir, name), v); err != nil {
			s.Close()
			return nil, err
		}
	}
	if s.BrainDown == nil {
		s.BrainDown = map[string]int64{}
	}
	return s, nil
}

func (s *State) files() map[string]any {
	return map[string]any{"seen.json": &s.Seen, "feeds.json": &s.Feeds, "pending.json": &s.Pending, "done.json": &s.Done,
		"signals.json": &s.Handled, "watch.json": &s.Watch, "domains.json": &s.Domains, "pings.json": &s.Pings,
		"brains.json": &s.BrainDown, "digest.json": &s.Digest}
}

// Save writes every file atomically.
func (s *State) Save() error {
	for name, v := range s.files() {
		if err := writeJSON(filepath.Join(s.Dir, name), v); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) Close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

// Journal appends one event to journal.jsonl.
func (s *State) Journal(ev string, kv map[string]any) {
	m := map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "ev": ev}
	for k, v := range kv {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	f, err := os.OpenFile(filepath.Join(s.Dir, "journal.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n')) // the journal is best effort
}

// Unfinished: action keys that began and never ended (the process died
// mid-action).
func (s *State) Unfinished() []string {
	b, err := os.ReadFile(filepath.Join(s.Dir, "journal.jsonl"))
	if err != nil {
		return nil
	}
	open := map[string]bool{}
	for _, line := range splitLines(b) {
		var m struct{ Ev, Key string }
		if json.Unmarshal(line, &m) != nil || m.Key == "" {
			continue
		}
		switch m.Ev {
		case "action.begin":
			open[m.Key] = true
		case "action.end":
			delete(open, m.Key)
		}
	}
	var out []string
	for k := range open {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
