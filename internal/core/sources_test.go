package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/brain"
)

// countBrain: answers "none" and counts the calls that mention a word.
type countBrain struct {
	word  string
	calls *int
}

func (b countBrain) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	if strings.Contains(task, b.word) {
		*b.calls++
	}
	return `{"verb":"none"}`, nil
}

// The flywheel: three "noise" labels on a blog's items make its next items
// quieter and free of model calls; three "important" labels on ops make its
// digest lines reports. A failing check is never made quieter.
func TestSourceScores(t *testing.T) {
	d := filepath.Join(t.TempDir(), "quiet")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "domain.toml"), []byte("name = \"quiet\"\n[[observe]]\nname = \"none\"\nrun = { cmd = [\"true\"] }\n"), 0o644)
	blog, ops := t.TempDir(), t.TempDir()
	cfg := writeConfig(t, t.TempDir(), `name = "s"
domain = "`+d+`"
mode = "act"
[[signals]]
name = "blog"
kind = "dir"
dir = "`+blog+`"
trust = "trusted"
[[signals]]
name = "ops"
kind = "dir"
dir = "`+ops+`"
trust = "trusted"
`)
	in := open(t, cfg)
	calls := 0
	in.Brain = countBrain{word: "blog", calls: &calls}
	drop := func(dir, name, text string) { _ = os.WriteFile(filepath.Join(dir, name+".txt"), []byte(text), 0o644) }
	for i := range 3 {
		drop(blog, fmt.Sprint("b", i), fmt.Sprintf("ten tips for a cleaner desk, part %d", i))
		drop(ops, fmt.Sprint("o", i), fmt.Sprintf("the rota for week %d is out", i))
	}
	t1, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	labeled := 0
	for _, it := range t1.Items {
		f, err := in.LastFinding(it.ID[:8]) // signal items are in last.json now
		if err != nil {
			t.Fatal(err)
		}
		label := map[string]string{"blog": "noise", "ops": "important"}[f.Source]
		if _, err := in.Triage.Label(f, label); err != nil {
			t.Fatal(err)
		}
		labeled++
	}
	if labeled != 6 || calls != 3 {
		t.Fatalf("labeled %d, brain calls on blog %d", labeled, calls)
	}

	calls = 0
	drop(blog, "b9", "seven habits of tidy people")
	drop(blog, "b10", "panic: the newsletter render failed")
	drop(ops, "o9", "the rota for week 9 is out")
	t2, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Disposition{}
	for _, it := range t2.Items {
		got[it.Source+"/"+it.Kind] = it.Disp
	}
	if got["blog/message"] != Ignore || got["blog/incident"] != Digest || got["ops/message"] != Report {
		t.Fatalf("dispositions %v", got)
	}
	if calls != 0 {
		t.Fatalf("the brain was asked about the noisy blog %d times", calls)
	}
	if !strings.Contains(strings.Join(t2.Notes, "\n"), "source blog") {
		t.Errorf("notes: %v", t2.Notes)
	}
	if st := sourcesLine(in.Triage.Cases()); !strings.Contains(st, "blog 0.20 (0↑ 0· 3↓") || !strings.Contains(st, "ops 0.80") {
		t.Error(st)
	}

	// the floors: a check never goes quieter
	it := &Item{Disp: Report}
	it.Source, it.Kind = "blog", "check"
	in.bySource(it, nil)
	if it.Disp != Report {
		t.Error("a failing check was made quieter")
	}
}
