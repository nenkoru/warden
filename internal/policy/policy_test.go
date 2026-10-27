package policy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, cfg Config, dir string) *Engine {
	t.Helper()
	e, err := Open(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func want(t *testing.T, v Verdict, outcome string) {
	t.Helper()
	if v.Outcome != outcome {
		t.Fatalf("outcome = %q, want %q (%+v)", v.Outcome, outcome, v)
	}
}

func TestDenyRuleWins(t *testing.T) {
	e := open(t, Config{Rules: []Rule{
		{Act: "bump", Then: "auto"},
		{Act: "bump", Target: "github.com/*", Then: "deny"},
	}}, t.TempDir())
	want(t, e.Decide(Action{Act: "bump", Target: "github.com/x/y", Floors: Floors{AlwaysAsk: true}}), "deny")
	want(t, e.Decide(Action{Act: "bump", Target: "golang.org/x/net"}), "auto")
}

func TestGrantMissingReports(t *testing.T) {
	a := Action{Act: "apply", Floors: Floors{Grant: "k8s.apply"}}
	e := open(t, Config{Rules: []Rule{{Then: "auto"}}}, t.TempDir())
	v := e.Decide(a)
	want(t, v, "report")
	if v.Floor != "grant" {
		t.Fatalf("floor = %q", v.Floor)
	}
	e = open(t, Config{Grants: []string{"k8s.apply"}, Rules: []Rule{{Then: "auto"}}}, t.TempDir())
	want(t, e.Decide(a), "auto")
}

func TestFloors(t *testing.T) {
	e := open(t, Config{Rules: []Rule{{Then: "auto"}}}, t.TempDir())
	v := e.Decide(Action{Act: "apply", Floors: Floors{AlwaysAsk: true}})
	want(t, v, "ask")
	if v.Floor != "always_ask" {
		t.Fatalf("floor = %q", v.Floor)
	}
	want(t, e.Decide(Action{Act: "apply", Floors: Floors{DryRunOnly: true}}), "report")
}

func TestRules(t *testing.T) {
	e := open(t, Config{Rules: []Rule{{Act: "clean", Then: "auto"}, {Act: "apply", Then: "ask"}}}, t.TempDir())
	want(t, e.Decide(Action{Act: "clean", Target: "go-build"}), "auto")
	want(t, e.Decide(Action{Act: "apply"}), "ask")
	v := e.Decide(Action{Act: "other"})
	want(t, v, "ask")
	if !strings.HasPrefix(v.Reasons[0], "learning: 0 of 3") {
		t.Fatalf("reasons = %v", v.Reasons)
	}
}

func TestLearning(t *testing.T) {
	e := open(t, Config{}, t.TempDir())
	a := Action{Act: "bump", Target: "x"}
	for range 2 {
		e.Learn(a, true)
	}
	want(t, e.Decide(a), "ask")
	e.Learn(a, true)
	want(t, e.Decide(a), "auto")
	want(t, e.Decide(Action{Act: "bump", Target: "y"}), "ask")

	e.Learn(a, false)
	want(t, e.Decide(a), "ask")
	for range 5 {
		e.Learn(a, true)
	}
	v := e.Decide(a)
	want(t, v, "ask")
	if !strings.HasPrefix(v.Reasons[0], "pinned") {
		t.Fatalf("reasons = %v", v.Reasons)
	}
}

func TestVelocity(t *testing.T) {
	e := open(t, Config{Rules: []Rule{{Then: "auto"}}, DailyAuto: 3}, t.TempDir())
	a := Action{Act: "clean", Floors: Floors{MaxPerDay: 2}}
	want(t, e.Decide(a), "auto")
	want(t, e.Decide(a), "auto")
	v := e.Decide(a)
	want(t, v, "ask")
	if v.Floor != "max_per_day" {
		t.Fatalf("floor = %q", v.Floor)
	}
	want(t, e.Decide(Action{Act: "other"}), "auto")
	want(t, e.Decide(Action{Act: "third"}), "ask") // daily budget of 3 spent

	// a new day resets the counts
	e.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	want(t, e.Decide(a), "auto")
}

func TestDryRun(t *testing.T) {
	e := open(t, Config{DryRun: true, Rules: []Rule{{Then: "auto"}}}, t.TempDir())
	v := e.Decide(Action{Act: "clean"})
	if v.Outcome != "ask" || v.Would != "auto" || !v.DryRun {
		t.Fatalf("verdict = %+v", v)
	}
	if e.Status().Today != 0 {
		t.Fatal("dry-run counted against the budget")
	}
	v = e.Decide(Action{Act: "clean", Floors: Floors{Grant: "g"}})
	if v.Outcome != "report" || v.Would != "report" || !v.DryRun {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	e := open(t, Config{}, dir)
	a := Action{Act: "bump", Target: "x"}
	for range 3 {
		e.Learn(a, true)
	}
	e.Learn(Action{Act: "bump", Target: "z"}, false)
	want(t, e.Decide(a), "auto")

	e2 := open(t, Config{}, dir)
	want(t, e2.Decide(a), "auto")
	want(t, e2.Decide(Action{Act: "bump", Target: "z"}), "ask")
	s := e2.Status()
	if s.Today != 2 || s.TodayByAct["bump"] != 2 || !s.Learned["bump z"].Pinned || s.Learned["bump x"].Approvals != 3 {
		t.Fatalf("status = %+v", s)
	}
	if s.Err != "" {
		t.Fatal(s.Err)
	}

	f, err := os.Open(filepath.Join(dir, "policy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
	}
	if n != 7 {
		t.Fatalf("log lines = %d, want 7", n)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".policy.json.tmp*")); len(m) > 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestOpenCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{}, dir); err == nil {
		t.Fatal("want error on corrupt state")
	}
}

func TestLearnAfterMin(t *testing.T) {
	e := open(t, Config{LearnAfter: 1}, t.TempDir())
	a := Action{Act: "x"}
	e.Learn(a, true)
	want(t, e.Decide(a), "ask")
	e.Learn(a, true)
	want(t, e.Decide(a), "auto")
}
