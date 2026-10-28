package experiment

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCategories(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		ok    bool
		text  string
		data  map[string]string
		want  []string
	}{
		{"success", "new", true, "draft PR #3 for issue #2 (brain: local)", nil, nil},
		{"merge", "merge", true, "merged PR #3 (own, squash) → abc1234", nil, nil},
		{"fix round pushed: CI was red", "fix", true, "PR #3: fix round pushed (brain: local)", nil, []string{WrongEdit}},
		{"fix round changed nothing", "fix", false, "PR #3: the fix round (brain: local) changed nothing", nil, []string{WrongEdit}},
		{"give-up", "giveup", false, "PR #3 stayed red after a fix round: labeled needs-human", nil, []string{GaveUp}},
		{"no change", "new", false, "issue #2: the brain (local) made no change", nil, []string{WrongEdit}},
		{"local checks red", "new", false, "issue #2: checks red after one fix attempt (brain: local); commented on the issue, no PR", nil, []string{WrongEdit}},
		{"protected path", "new", false, "issue #2: refused — the change touches protected paths (.github/x.yml); labeled needs-human, nothing pushed", nil, []string{WrongEdit}},
		{"max steps", "new", false, "issue #2: the brain local failed (stopped after 40 steps (max_steps)); tried again next tick", nil, []string{Loop}},
		{"repeated calls", "new", false, "issue #2: the brain (local) made no change", map[string]string{"repeats": "2"}, []string{WrongEdit, Loop}},
		{"one repeat is not a loop", "new", false, "issue #2: the brain (local) made no change", map[string]string{"repeats": "1"}, []string{WrongEdit}},
		{"max steps from the counts", "new", false, "issue #2: x", map[string]string{"finish_reason": "max_steps"}, []string{Loop}},
		{"prompt near the context", "new", false, "issue #2: the brain (local) made no change", map[string]string{"max_prompt_tokens": "240000"}, []string{ContextLimit, WrongEdit}},
		{"prompt below 90%", "new", false, "issue #2: the brain (local) made no change", map[string]string{"max_prompt_tokens": "235000"}, []string{WrongEdit}},
		{"a smaller context", "new", false, "issue #2: x", map[string]string{"max_prompt_tokens": "29500", "context_tokens": "32768"}, []string{ContextLimit}},
		{"unknown tool counted", "new", false, "issue #2: x", map[string]string{"unknown_tool": "1"}, []string{ToolCall}},
		{"bad arguments counted", "new", false, "issue #2: x", map[string]string{"bad_args": "2", "tool_errors": "5"}, []string{ToolCall}},
		{"context", "new", false, "brain local failed: http://llm/v1/chat/completions: 400 Bad Request: This model's maximum context length is 32768 tokens", nil, []string{ContextLimit}},
		{"prompt too long", "fix", false, "prompt is too long: 210000 tokens > 200000 maximum", nil, []string{ContextLimit, WrongEdit}},
		{"timeout", "new", false, "brain local failed: context deadline exceeded", nil, []string{Timeout}},
		{"unknown tool in the log", "new", false, "warden brain: run_shell → error: unknown tool run_shell", nil, []string{ToolCall}},
		{"bad arguments", "new", false, "error: arguments are not a JSON object: invalid character", nil, []string{ToolCall}},
		{"a tool that ran and failed is no tool-call error", "new", false, "issue #2: the brain (local) made no change", map[string]string{"tool_errors": "3"}, []string{WrongEdit}},
		{"tool-call errors on a success don't count", "new", true, "draft PR #3", map[string]string{"unknown_tool": "3", "max_prompt_tokens": "260000"}, nil},
		{"the model says it can't", "new", false, "brain local: I cannot do this without network access", nil, []string{GaveUp}},
		{"unexplained", "merge", false, "merge-pr: 502 Bad Gateway", nil, []string{Other}},
	}
	for _, c := range cases {
		if got := Categories(c.phase, c.ok, c.text, c.data); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAppendLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "tasks.jsonl")
	at := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	e := Event{At: at, Act: "work-issue", Issue: 7, Phase: "new", Brain: "local", OK: true, TokensIn: 10,
		Data: map[string]string{"size": "M", "files": "2", "issue": "999"}}
	if err := Append(p, e); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, Event{At: at, Kind: "idle", Minutes: 10, Reason: "off-hours"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"size":"M"`) || !strings.Contains(string(b), `"issue":7`) || strings.Contains(string(b), `"999"`) {
		t.Fatalf("data must be flattened, never over the record's own fields:\n%s", b)
	}
	_ = os.WriteFile(p, append(b, []byte("not json\n")...), 0o600)
	evs, err := Load(p)
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v %+v", err, evs)
	}
	if evs[0].Kind != "act" || evs[0].Issue != 7 || evs[0].Data["size"] != "M" || evs[0].Data["files"] != "2" || evs[1].Kind != "idle" || evs[1].Minutes != 10 {
		t.Fatalf("%+v", evs)
	}
	if evs, err := Load(filepath.Join(t.TempDir(), "none")); err != nil || evs != nil {
		t.Fatal("a missing log is no events")
	}
}
