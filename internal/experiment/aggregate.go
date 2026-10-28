package experiment

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Task: one task's numbers over a period (a day or a week).
type Task struct {
	Issue, PR      int
	Size           string // S | M | L | XL ("" = unknown)
	Brain, Model   string // the latest
	Attempts       int    // work-issue runs: the first attempt and fix rounds
	FixRounds      int
	Steps          int // model turns
	ToolCalls      int
	ToolCallErrors int // unknown tools and arguments that did not parse
	WallMS         int64
	TokensIn       int64
	TokensOut      int64
	FirstPush      string // CI on the first push: green | red | "" (not known yet)
	Merged         bool
	Reverted       bool
	ReviewComments int
	Categories     []string
	First          time.Time
}

// Name: "#N" for an issue, "PR #N" for a PR without one.
func (t Task) Name() string {
	if t.Issue > 0 {
		return "#" + strconv.Itoa(t.Issue)
	}
	return "PR #" + strconv.Itoa(t.PR)
}

// Failure: one act that failed, with its one-line reason.
type Failure struct {
	At         time.Time
	Task       string
	Phase      string
	Categories []string
	Reason     string
}

// Period: a day ("2026-10-28") or an ISO week ("2026-W44").
type Period struct {
	Name     string
	Tasks    []Task
	Failures []Failure
	Reverts  []Revert           // reverts that happened in the period (their task's row is where it began)
	Idle     map[string]float64 // minutes by reason
}

// Revert: a task undone, the day it happened.
type Revert struct {
	At    time.Time
	Task  string
	PR    int
	First string // the day the task began ("" = before the log)
}

// IdleHours: every reason together.
func (p Period) IdleHours() float64 {
	m := 0.0
	for _, v := range p.Idle {
		m += v
	}
	return m / 60
}

// Report: the log, newest first.
type Report struct {
	Loc   *time.Location
	Weeks []Period
	Days  []Period
}

func dayOf(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.DateOnly) }

func weekOf(t time.Time, loc *time.Location) string {
	y, w := t.In(loc).ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// Aggregate groups the events by day and ISO week in loc (nil: UTC).
func Aggregate(evs []Event, loc *time.Location) Report {
	if loc == nil {
		loc = time.UTC
	}
	evs = attachReverts(evs, loc)
	return Report{Loc: loc, Weeks: periods(evs, func(t time.Time) string { return weekOf(t, loc) }), Days: periods(evs, func(t time.Time) string { return dayOf(t, loc) })}
}

// attachReverts: a revert belongs to the task it undoes: it marks the row of
// the day and week of the task's first event (no row, no task of its own
// where it happened), and leaves a note on the day it happened. A revert of
// a task from before the log is only that note.
func attachReverts(evs []Event, loc *time.Location) []Event {
	evs = append([]Event(nil), evs...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	first := map[string]time.Time{}
	for _, e := range evs {
		if k := taskKey(e); k != "" && e.Kind != "idle" && e.Phase != "revert" {
			if _, ok := first[k]; !ok {
				first[k] = e.At
			}
		}
	}
	out := make([]Event, 0, len(evs))
	for _, e := range evs {
		if e.Phase != "revert" || e.Kind == "idle" {
			out = append(out, e)
			continue
		}
		note := Event{At: e.At, Kind: "revert", Issue: e.Issue, PR: e.PR, Summary: e.Summary}
		if at, ok := first[taskKey(e)]; ok {
			note.Data = map[string]string{"first": dayOf(at, loc)}
			e.At = at
			out = append(out, e)
		}
		out = append(out, note)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func periods(evs []Event, name func(time.Time) string) []Period {
	by := map[string][]Event{}
	var names []string
	for _, e := range evs {
		n := name(e.At)
		if by[n] == nil {
			names = append(names, n)
		}
		by[n] = append(by[n], e)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := make([]Period, 0, len(names))
	for _, n := range names {
		out = append(out, period(n, by[n]))
	}
	return out
}

func taskKey(e Event) string {
	switch {
	case e.Issue > 0:
		return "#" + strconv.Itoa(e.Issue)
	case e.PR > 0:
		return "PR #" + strconv.Itoa(e.PR)
	}
	return ""
}

func period(name string, evs []Event) Period {
	p := Period{Name: name, Idle: map[string]float64{}}
	tasks := map[string]*Task{}
	var order []string
	for _, e := range evs {
		switch e.Kind {
		case "idle":
			p.Idle[or(e.Reason, "off-hours")] += e.Minutes
			continue
		case "revert":
			p.Reverts = append(p.Reverts, Revert{At: e.At, Task: taskKey(e), PR: e.PR, First: e.Data["first"]})
			continue
		}
		k := taskKey(e)
		if k == "" {
			continue
		}
		t := tasks[k]
		if t == nil {
			t = &Task{First: e.At}
			tasks[k] = t
			order = append(order, k)
		}
		add(t, e)
		if len(e.Categories) > 0 {
			p.Failures = append(p.Failures, Failure{At: e.At, Task: k, Phase: e.Phase, Categories: e.Categories, Reason: e.Summary})
		}
	}
	for _, k := range order {
		p.Tasks = append(p.Tasks, *tasks[k])
	}
	return p
}

func add(t *Task, e Event) {
	t.Issue, t.PR = max(t.Issue, e.Issue), max(t.PR, e.PR)
	if s := e.Data["size"]; s != "" && (t.Size == "" || e.Phase == "new") {
		t.Size = s
	}
	if e.Brain != "" {
		t.Brain = e.Brain
	}
	if e.Model != "" {
		t.Model = e.Model
	}
	n := func(k string) int { v, _ := strconv.Atoi(e.Data[k]); return v }
	t.Steps += n("steps")
	t.ToolCalls += n("tool_calls")
	t.ToolCallErrors += n("unknown_tool") + n("bad_args")
	t.WallMS += e.WallMS
	t.TokensIn += e.TokensIn
	t.TokensOut += e.TokensOut
	switch e.Phase {
	case "new":
		t.Attempts++
	case "fix":
		t.Attempts++
		t.FixRounds++
	case "merge":
		t.Merged = t.Merged || e.OK
		if n, err := strconv.Atoi(e.Data["review_comments"]); err == nil {
			t.ReviewComments = max(t.ReviewComments, n)
		}
	case "revert":
		t.Reverted = true
	}
	switch fp := e.Data["ci_first_push"]; {
	case fp == "red" || e.Phase == "fix":
		t.FirstPush = "red"
	case fp == "green" && t.FirstPush == "":
		t.FirstPush = "green"
	}
	for _, c := range e.Categories {
		if !contains(t.Categories, c) {
			t.Categories = append(t.Categories, c)
		}
	}
	sort.Slice(t.Categories, func(i, j int) bool { return rank(t.Categories[i]) < rank(t.Categories[j]) })
}

func rank(c string) int {
	for i, o := range Order {
		if o == c {
			return i
		}
	}
	return len(Order)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func or(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// ---- a period's summary numbers

// Summary: the weekly table's numbers for a period.
type Summary struct {
	Tasks, Merged, Reverted int
	Green, Pushed           int     // tasks green on the first push, of those whose first push is known
	AvgAttempts, AvgFix     float64 // over tasks with an attempt in the period
	ToolCalls, CallErrors   int     // every tool call; unknown tools and bad arguments
	MedianWallMS            int64   // over tasks with wall time in the period
	TokensIn, TokensOut     int64
	TopFailures             string // "wrong-edit 2, loop 1"
}

func (p Period) Summary() Summary {
	var s Summary
	var walls []int64
	worked, attempts, fixes := 0, 0, 0
	cats := map[string]int{}
	for _, t := range p.Tasks {
		s.Tasks++
		if t.Merged {
			s.Merged++
		}
		if t.Reverted {
			s.Reverted++
		}
		if t.FirstPush != "" {
			s.Pushed++
			if t.FirstPush == "green" {
				s.Green++
			}
		}
		if t.Attempts > 0 {
			worked++
			attempts += t.Attempts
			fixes += t.FixRounds
		}
		if t.WallMS > 0 {
			walls = append(walls, t.WallMS)
		}
		s.ToolCalls += t.ToolCalls
		s.CallErrors += t.ToolCallErrors
		s.TokensIn += t.TokensIn
		s.TokensOut += t.TokensOut
	}
	for _, f := range p.Failures {
		for _, c := range f.Categories {
			cats[c]++
		}
	}
	if worked > 0 {
		s.AvgAttempts, s.AvgFix = float64(attempts)/float64(worked), float64(fixes)/float64(worked)
	}
	if len(walls) > 0 {
		sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
		if n := len(walls); n%2 == 1 {
			s.MedianWallMS = walls[n/2]
		} else {
			s.MedianWallMS = (walls[n/2-1] + walls[n/2]) / 2
		}
	}
	names := make([]string, 0, len(cats))
	for c := range cats {
		names = append(names, c)
	}
	sort.Slice(names, func(i, j int) bool {
		if cats[names[i]] != cats[names[j]] {
			return cats[names[i]] > cats[names[j]]
		}
		return rank(names[i]) < rank(names[j])
	})
	var top []string
	for i, c := range names {
		if i == 3 {
			break
		}
		top = append(top, fmt.Sprintf("%s %d", c, cats[c]))
	}
	s.TopFailures = strings.Join(top, ", ")
	return s
}
