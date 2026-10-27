package core

// The experiment log (internal/experiment). With params.experiment_log =
// "on", every act on a task — an act in params.experiment_acts (default
// work-issue, merge-pr, pickup-branch) whose result names an issue or a PR —
// appends one record to <state>/tasks.jsonl: the act's own data, its wall
// time, the tokens its brains used and its failure categories. An act
// deferred before any model call (CI running, the brain off-hours) is not an
// attempt and is not recorded; the time the coding brain could not work is
// recorded per tick as kind "idle" instead, never as a failure.

import (
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/builtin"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/experiment"
)

func (in *Instance) experimentOn() bool { return in.Dom.Params["experiment_log"] == "on" }

// taskActs: the acts whose results are task records.
func (in *Instance) taskActs() []string {
	var acts []string
	for _, a := range strings.Split(in.Dom.Params["experiment_acts"], ",") {
		if a = strings.TrimSpace(a); a != "" {
			acts = append(acts, a)
		}
	}
	if len(acts) == 0 {
		return []string{"work-issue", "merge-pr", "pickup-branch"}
	}
	return acts
}

// usageSnap: every brain's calls and tokens so far this tick.
type usageSnap struct {
	calls   int
	in, out int64
}

func (in *Instance) usageTotal() usageSnap {
	in.mu.Lock()
	defer in.mu.Unlock()
	var u usageSnap
	for _, b := range in.usage {
		u.calls += b.Calls
		u.in += b.In
		u.out += b.Out
	}
	return u
}

// recordTask appends an act's task record (acts run one at a time, so the
// usage difference is this act's).
func (in *Instance) recordTask(act, key string, r *Result, wall time.Duration, before usageSnap) {
	if !in.experimentOn() || !slices.Contains(in.taskActs(), act) || r.Data["issue"] == "" && r.Data["pr"] == "" {
		return
	}
	after := in.usageTotal()
	calls := after.calls - before.calls
	if r.Deferred && calls == 0 {
		return
	}
	data := maps.Clone(r.Data)
	e := experiment.Event{At: in.Now().UTC(), Kind: "act", Act: act, Key: key, Phase: data["phase"], Brain: data["brain"], OK: r.OK, Refused: r.Refused,
		Deferred: r.Deferred, Summary: short(r.Summary, 300), WallMS: wall.Milliseconds(), TokensIn: after.in - before.in, TokensOut: after.out - before.out, Calls: calls}
	e.Issue, _ = strconv.Atoi(data["issue"])
	e.PR, _ = strconv.Atoi(data["pr"])
	for _, k := range []string{"issue", "pr", "phase", "brain"} {
		delete(data, k)
	}
	if s := in.slots[e.Brain]; s != nil {
		e.Model = s.cfg.Model
	}
	e.Categories = experiment.Categories(e.Phase, r.OK, r.Summary+"\n"+strings.Join(r.Log, "\n"), data)
	e.Data = data
	_ = experiment.Append(builtin.TasksLog(in.Cfg.State), e) // best effort, like the journal
}

// taskEvent records a task event an observer reported (data task_event:
// "revert"), the first tick the finding is seen.
func (in *Instance) taskEvent(f domain.Finding) {
	phase := f.Data["task_event"]
	if phase == "" || !in.experimentOn() {
		return
	}
	data := maps.Clone(f.Data)
	e := experiment.Event{At: in.Now().UTC(), Kind: "act", Act: f.Step, Key: f.Key, Phase: phase, OK: true, Summary: short(f.Title, 300)}
	e.Issue, _ = strconv.Atoi(data["issue"])
	e.PR, _ = strconv.Atoi(data["pr"])
	for _, k := range []string{"task_event", "issue", "pr"} {
		delete(data, k)
	}
	e.Data = data
	_ = experiment.Append(builtin.TasksLog(in.Cfg.State), e)
}

// idleTick: when the work-issue brain cannot work now (off-hours, its
// upstream unavailable, the budget spent), the minutes since the previous
// tick (at most one wake interval) are idle time, logged apart from failures.
func (in *Instance) idleTick(now time.Time) {
	if !in.experimentOn() {
		return
	}
	if _, ok := in.Dom.ActStep("work-issue"); !ok {
		return
	}
	p := filepath.Join(in.Cfg.State, "idle.json")
	var prev struct {
		Tick int64 `json:"tick"`
	}
	_ = readJSON(p, &prev)
	cur := prev
	cur.Tick = now.Unix()
	_ = writeJSON(p, cur)
	if prev.Tick == 0 || now.Unix() <= prev.Tick {
		return
	}
	reason, name := in.idleReason("work-issue", now)
	if reason == "" {
		return
	}
	gap := min(now.Sub(time.Unix(prev.Tick, 0)), in.Cfg.Every())
	_ = experiment.Append(builtin.TasksLog(in.Cfg.State), experiment.Event{At: now.UTC(), Kind: "idle", Brain: name, Minutes: gap.Minutes(), Reason: reason})
}

// idleReason: why no brain routed to task may work now (off-hours,
// unavailable, budget), and the first such brain; "" when one may.
func (in *Instance) idleReason(task string, now time.Time) (string, string) {
	var names []string
	switch r := in.route(task); r {
	case "auto":
		names = in.ladder()
	case "":
		names = []string{in.defaultBrain}
	default:
		names = []string{r}
	}
	reason, who := "", ""
	for _, n := range names {
		s := in.slots[n]
		if s == nil {
			continue
		}
		in.mu.Lock()
		down := in.St.BrainDown[n] > now.Unix()
		in.mu.Unlock()
		why := ""
		switch {
		case down:
			why = "unavailable"
		case s.win.off(now):
			why = "off-hours"
		case s.paid && in.Ledger != nil && in.Ledger.Allow(0) != nil:
			why = "budget"
		default:
			return "", ""
		}
		if reason == "" {
			reason, who = why, n
		}
	}
	return reason, who
}
