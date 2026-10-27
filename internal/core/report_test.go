package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A vuln told and bumped on one tick comes back on a later (incident) tick
// as one line with its branch; the new incident keeps its full detail.
func TestReportToldItemsOneLine(t *testing.T) {
	inbox := t.TempDir()
	cfg, _ := goProject(t, `[[signals]]
name = "app"
kind = "dir"
dir = "`+inbox+`"
trust = "trusted"
`)
	in := open(t, cfg)
	in.Web = fakeWeb(t)
	var ran []string
	in.Run = fakeRun(&ran)
	var dir, saw string
	in.Brain = rcaBrain{&dir, &saw}
	t1, err := in.Tick(context.Background())
	if err != nil || t1.Report == "" {
		t.Fatalf("bump tick: %v %+v", err, t1)
	}

	_ = os.WriteFile(filepath.Join(inbox, "1.txt"), []byte("panic: runtime error: nil pointer dereference in relay.Pipe"), 0o644)
	t2, err := in.Tick(context.Background())
	if err != nil || t2.Report == "" {
		t.Fatalf("incident tick: %v %+v", err, t2)
	}
	b, _ := os.ReadFile(t2.Report)
	rep := string(b)

	var vuln string
	for _, l := range strings.Split(rep, "\n") {
		if strings.Contains(l, "example.com/dep") && strings.HasPrefix(l, "- **") {
			vuln = l
		}
	}
	if !strings.Contains(vuln, "done earlier: branch warden/bump-example.com-dep-v1.4.0") || strings.Contains(vuln, "checks pass") {
		t.Fatalf("the told vuln is not one line with its branch: %q\n%s", vuln, rep)
	}
	if !strings.Contains(rep, "Already told (1):") {
		t.Fatalf("no told group:\n%s", rep)
	}
	for _, noise := range []string{"osv.dev", "bad thing"} {
		if strings.Contains(rep, noise) {
			t.Fatalf("the told vuln repeats its advisories (%q):\n%s", noise, rep)
		}
	}
	if !strings.Contains(rep, "Pipe gets a nil conn") || !strings.Contains(rep, "warden approve ") {
		t.Fatalf("the new incident lost its detail:\n%s", rep)
	}
}
