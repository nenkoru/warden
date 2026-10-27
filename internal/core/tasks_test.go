package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/experiment"
)

// codingOpenAI: an OpenAI-compatible coder: it writes feature.go with a
// tool call, then answers.
func codingOpenAI(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1)%2 == 1 {
			_, _ = io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"feature.go\",\"content\":\"package proj\\n\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"added feature.go"}}],"usage":{"prompt_tokens":150,"completion_tokens":5}}`)
	}))
}

func taskEvents(t *testing.T, m *maint) []experiment.Event {
	t.Helper()
	evs, err := experiment.Load(filepath.Join(m.cfg.State, "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestExperimentRecordsTasksAndIdleTime(t *testing.T) {
	var calls atomic.Int32
	llm := codingOpenAI(&calls)
	defer llm.Close()
	m := newMaint(t, "brain_for = { work-issue = \"local\" }\nwake = { every = \"10m\" }", fmt.Sprintf(`[brains.local]
provider = "openai"
base_url = %q
model = "qwen3.8-27b-local"
free = true
unavailable = ["00:30-08:00", "sat", "sun"]
tz = "Europe/Moscow"
`, llm.URL))
	m.fake.AddIssue("Rewrite the scheduler", "a big feature", "boss", "OWNER", "enhancement")
	in := open(t, m.cfg)
	tick := func(at time.Time) {
		t.Helper()
		in.Now = func() time.Time { return at }
		if _, err := in.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	night := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC) // Tue 06:00 Moscow: the local brain is off-hours
	tick(night)
	tick(night.Add(5 * time.Minute))
	tick(night.Add(90 * time.Minute)) // 07:30 Moscow: a gap longer than the wake interval counts one interval
	if calls.Load() != 0 {
		t.Fatal("the brain was called off-hours")
	}
	evs := taskEvents(t, m)
	if len(evs) != 2 || evs[0].Kind != "idle" || evs[0].Minutes != 5 || evs[0].Reason != "off-hours" || evs[0].Brain != "local" || evs[1].Minutes != 10 {
		t.Fatalf("off-hours must be idle time, not attempts: %+v", evs)
	}

	tick(time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC)) // 09:00 Moscow: it works, the large feature included
	evs = taskEvents(t, m)
	if len(evs) != 3 {
		t.Fatalf("events: %+v", evs)
	}
	e := evs[2]
	if e.Kind != "act" || e.Act != "work-issue" || e.Issue != 1 || e.PR != 2 || e.Phase != "new" || e.Brain != "local" || e.Model != "qwen3.8-27b-local" ||
		!e.OK || e.Calls != 1 || e.TokensIn != 250 || e.TokensOut != 25 || e.WallMS < 0 || len(e.Categories) != 0 ||
		e.Data["files"] != "1" || e.Data["size"] != "S" || e.Data["tool_calls"] != "1" || e.Data["tool_errors"] != "0" || !strings.Contains(e.Summary, "draft PR #2 for issue #1") {
		t.Fatalf("the attempt's record: %+v", e)
	}

	// a revert of the merged PR: one revert record, however many ticks see it
	orig := m.fake.MergedPR("feat: Rewrite the scheduler (#1)", "warden/issue-1-rewrite-the-scheduler", m.fake.Now)
	m.fake.MergedPR(`Revert "feat: Rewrite the scheduler (#1)"`, fmt.Sprintf("revert-%d-warden/issue-1-rewrite-the-scheduler", orig), m.fake.Now)
	tick(time.Date(2026, 10, 27, 6, 10, 0, 0, time.UTC))
	tick(time.Date(2026, 10, 27, 6, 20, 0, 0, time.UTC))
	var reverts []experiment.Event
	for _, e := range taskEvents(t, m) {
		if e.Phase == "revert" {
			reverts = append(reverts, e)
		}
	}
	if len(reverts) != 1 || reverts[0].Issue != 1 || reverts[0].PR != orig {
		t.Fatalf("reverts: %+v", reverts)
	}
	noToken(t, m)
}

func TestExperimentRecordsAFailedAttemptWithItsCategory(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"This model's maximum context length is 32768 tokens","code":"context_length_exceeded"}}`)
	}))
	defer llm.Close()
	m := newMaint(t, `brain_for = { work-issue = "local" }`, fmt.Sprintf("[brains.local]\nprovider = \"openai\"\nbase_url = %q\nmodel = \"qwen3.8-27b-local\"\nfree = true\n", llm.URL))
	m.fake.AddIssue("Add a feature", "x", "boss", "OWNER", "enhancement")
	in := open(t, m.cfg)
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := taskEvents(t, m)
	if len(evs) != 1 || !evs[0].Deferred || evs[0].OK || evs[0].Calls != 1 || len(evs[0].Categories) != 1 || evs[0].Categories[0] != experiment.ContextLimit {
		t.Fatalf("a brain failure is an attempt with its category: %+v", evs)
	}
	// the experiment log off: nothing recorded
	m.cfg.Params["experiment_log"] = "off"
	in.Close()
	in = open(t, m.cfg)
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := taskEvents(t, m); len(evs) != 1 {
		t.Fatalf("experiment_log off still records: %+v", evs)
	}
}
