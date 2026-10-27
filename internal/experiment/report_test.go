package experiment

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// sample: two weeks of a task's life, with idle nights.
func sample() []Event {
	msk, _ := time.LoadLocation("Europe/Moscow")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, msk) }
	return []Event{
		{At: at(23, 1, 0), Kind: "idle", Minutes: 420, Reason: "off-hours"},
		{At: at(23, 9, 10), Kind: "act", Act: "work-issue", Issue: 3, PR: 4, Phase: "new", Brain: "local", Model: "qwen3.8-27b-local", OK: true,
			Summary: "draft PR #4 for issue #3 (brain: local)", WallMS: 250_000, TokensIn: 120_000, TokensOut: 8_000, Calls: 1,
			Data: map[string]string{"size": "S", "files": "2", "lines_added": "30", "lines_deleted": "4", "steps": "12", "tool_calls": "11"}},
		{At: at(23, 10, 0), Kind: "act", Act: "merge-pr", Issue: 3, PR: 4, Phase: "merge", OK: true, Summary: "merged PR #4 (own, squash) → m4",
			Data: map[string]string{"ci_first_push": "green", "review_comments": "1"}},
		{At: at(26, 9, 5), Kind: "act", Act: "work-issue", Issue: 5, PR: 6, Phase: "new", Brain: "local", Model: "qwen3.8-27b-local", OK: true,
			Summary: "draft PR #6 for issue #5 (brain: local)", WallMS: 600_000, TokensIn: 300_000, TokensOut: 20_000, Calls: 1, Data: map[string]string{"size": "L", "steps": "30", "tool_calls": "29", "unknown_tool": "1", "bad_args": "2"}},
		{At: at(26, 9, 40), Kind: "act", Act: "work-issue", Issue: 5, PR: 6, Phase: "fix", Brain: "local", Model: "qwen3.8-27b-local", OK: true,
			Summary: "PR #6: fix round pushed (brain: local)", WallMS: 200_000, TokensIn: 90_000, TokensOut: 5_000, Calls: 1, Categories: []string{WrongEdit},
			Data: map[string]string{"ci_first_push": "red", "fix_round": "1", "steps": "8", "tool_calls": "7"}},
		{At: at(26, 10, 30), Kind: "act", Act: "merge-pr", Issue: 5, PR: 6, Phase: "merge", OK: true, Summary: "merged PR #6 (own, squash) → m6",
			Data: map[string]string{"ci_first_push": "red", "review_comments": "0"}},
		{At: at(27, 0, 40), Kind: "idle", Minutes: 300, Reason: "off-hours"},
		{At: at(27, 9, 0), Kind: "act", Act: "work-issue", Issue: 7, Phase: "new", Brain: "local", Model: "qwen3.8-27b-local", Deferred: true,
			Summary: "issue #7: the brain local failed (http://llm: 400: maximum context length is 32768 tokens); tried again next tick", WallMS: 30_000,
			TokensIn: 32_000, Calls: 1, Categories: []string{ContextLimit}},
		{At: at(27, 9, 30), Kind: "act", Act: "work-issue", Issue: 7, Phase: "new", Brain: "local", Model: "qwen3.8-27b-local",
			Summary: "issue #7: the brain (local) made no change | @someone", WallMS: 900_000, TokensIn: 400_000, TokensOut: 30_000, Calls: 1,
			Categories: []string{WrongEdit, Loop}, Data: map[string]string{"repeats": "6", "steps": "40", "tool_calls": "44", "bad_args": "4"}},
		{At: at(27, 11, 0), Kind: "idle", Minutes: 60, Reason: "unavailable"},
		{At: at(28, 12, 0), Kind: "act", Act: "github-main", Issue: 3, PR: 4, Phase: "revert", OK: true, Summary: "PR #9 reverted Warden's PR #4 (issue #3)",
			Data: map[string]string{"revert_pr": "9"}},
	}
}

func TestAggregate(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	r := Aggregate(sample(), msk)
	if len(r.Weeks) != 2 || r.Weeks[0].Name != "2026-W44" || r.Weeks[1].Name != "2026-W43" {
		t.Fatalf("weeks: %+v", r.Weeks)
	}
	if len(r.Days) != 4 || r.Days[0].Name != "2026-10-28" || r.Days[3].Name != "2026-10-23" {
		t.Fatalf("days: %+v", r.Days)
	}
	w := r.Weeks[0].Summary() // 26–28 Oct: #5 (fix round, merged), #7 (failed twice); #3's revert belongs to week 43
	if w.Tasks != 2 || w.Merged != 1 || w.Reverted != 0 || w.Pushed != 1 || w.Green != 0 || w.AvgAttempts != 2 || w.AvgFix != 0.5 ||
		w.TopFailures != "wrong-edit 2, context-limit 1, loop 1" || w.TokensIn != 822_000 || w.ToolCalls != 80 || w.CallErrors != 7 {
		t.Fatalf("week 44: %+v", w)
	}
	if w.MedianWallMS != 865_000 { // #5: 800s, #7: 930s
		t.Fatalf("median: %d", w.MedianWallMS)
	}
	if h := r.Weeks[0].IdleHours(); h != 6 {
		t.Fatalf("idle: %v", h)
	}
	old := r.Weeks[1].Summary()
	if old.Tasks != 1 || old.Merged != 1 || old.Green != 1 || old.Pushed != 1 || old.Reverted != 1 {
		t.Fatalf("week 43: %+v", old)
	}
	if rev := r.Days[0]; len(rev.Tasks) != 0 || len(rev.Reverts) != 1 || rev.Reverts[0].Task != "#3" || rev.Reverts[0].First != "2026-10-23" {
		t.Fatalf("the revert's day: a note, no row: %+v", rev)
	}
	if three := r.Days[3].Tasks[0]; three.Issue != 3 || !three.Reverted || !three.Merged || three.Attempts != 1 {
		t.Fatalf("#3 is marked reverted on its own row: %+v", three)
	}
	five := r.Days[2].Tasks[0]
	if five.Issue != 5 || five.Size != "L" || five.FirstPush != "red" || five.FixRounds != 1 || five.Attempts != 2 || !five.Merged || five.WallMS != 800_000 ||
		five.Steps != 38 || five.ToolCalls != 36 || five.ToolCallErrors != 3 {
		t.Fatalf("#5: %+v", five)
	}
}

func TestMarkdownGolden(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, msk)
	got := Aggregate(sample(), msk).Markdown(now)
	if again := Aggregate(sample(), msk).Markdown(now.Add(time.Hour)); again != got {
		t.Fatal("the same events on the same day must render the same")
	}
	golden := filepath.Join("testdata", "experiment.md")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("Markdown differs from %s (go test ./internal/experiment -update):\n%s", golden, got)
	}
	if strings.Contains(got, "http://") || strings.Contains(got, "@someone") {
		t.Fatal("a public page names no endpoint and pings no one")
	}
	if empty := Aggregate(nil, msk).Markdown(now); !strings.Contains(empty, "Nothing recorded yet.") {
		t.Fatal(empty)
	}
}
