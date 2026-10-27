// Package experiment is the 27B experiment's log: what Warden's coding
// model did on each task (an issue worked by work-issue across ticks: the
// first attempt, fix rounds, the give-up, the merge, a revert), recorded as
// append-only JSONL in the state (tasks.jsonl), aggregated per day and ISO
// week, and rendered as Markdown for the owner and Claude to review every
// few days (docs/experiment-27b.md, published by the publish-log act).
package experiment

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Event: one line of tasks.jsonl. Kind "act" is one act on a task; kind
// "idle" is time the coding brain could not work (off-hours, unavailable,
// over budget): logged apart, never a failure.
type Event struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"` // act | idle
	Act        string    `json:"act,omitempty"`
	Key        string    `json:"key,omitempty"`
	Issue      int       `json:"issue,omitempty"`
	PR         int       `json:"pr,omitempty"`
	Phase      string    `json:"phase,omitempty"` // new | fix | giveup | merge | revert
	Brain      string    `json:"brain,omitempty"`
	Model      string    `json:"model,omitempty"`
	OK         bool      `json:"ok,omitempty"`
	Refused    bool      `json:"refused,omitempty"`
	Deferred   bool      `json:"deferred,omitempty"` // the brain failed before any change: tried again later
	Summary    string    `json:"summary,omitempty"`
	WallMS     int64     `json:"wall_ms,omitempty"`
	TokensIn   int64     `json:"tokens_in,omitempty"`
	TokensOut  int64     `json:"tokens_out,omitempty"`
	Calls      int       `json:"calls,omitempty"`
	Categories []string  `json:"categories,omitempty"`
	Minutes    float64   `json:"minutes,omitempty"` // idle
	Reason     string    `json:"reason,omitempty"`  // idle: off-hours | unavailable | budget
	// Data: the act's own fields (size, files, lines_added, tool_errors,
	// ci_first_push, …), flattened into the line.
	Data map[string]string `json:"-"`
}

type plain Event

// known: the JSON names of Event's own fields (Data never overrides them).
var known = func() map[string]bool {
	m := map[string]bool{}
	t := reflect.TypeFor[plain]()
	for i := range t.NumField() {
		if n, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); n != "" && n != "-" {
			m[n] = true
		}
	}
	return m
}()

func (e Event) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(plain(e))
	if err != nil || len(e.Data) == 0 {
		return b, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range e.Data {
		if !known[k] {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func (e *Event) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, (*plain)(e)); err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k, raw := range m {
		var s string
		if known[k] || json.Unmarshal(raw, &s) != nil {
			continue
		}
		if e.Data == nil {
			e.Data = map[string]string{}
		}
		e.Data[k] = s
	}
	return nil
}

// Append adds one event to the JSONL file at path (0600, parents made).
func Append(path string, e Event) error {
	if e.Kind == "" {
		e.Kind = "act"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Load reads the events at path; a missing file is no events, a line that
// does not parse is skipped.
func Load(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && !e.At.IsZero() {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
