package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/brain"
)

// rcaBrain: reads main.go in the directory it was given, writes there too
// (which must not reach the owner's checkout), and answers with a FIX line.
type rcaBrain struct{ dir, saw *string }

func (b rcaBrain) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	*b.dir = brain.DirFrom(ctx)
	bs, _ := os.ReadFile(filepath.Join(*b.dir, "main.go"))
	*b.saw = string(bs)
	_ = os.WriteFile(filepath.Join(*b.dir, "main.go"), []byte("scribble"), 0o644)
	return "cause: Pipe gets a nil conn\nFIX: return early when the dial fails", nil
}

// An incident's RCA reads the project's code in a throwaway detached worktree.
func TestIncidentRCAReadsTheCode(t *testing.T) {
	inbox := t.TempDir()
	cfg, repo := goProject(t, `[[signals]]
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
	_ = os.WriteFile(filepath.Join(inbox, "1.txt"), []byte("panic: runtime error: nil pointer dereference in relay.Pipe"), 0o644)
	tk, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(cfg.State, "work")+"/") || !strings.Contains(saw, "package main") {
		t.Fatalf("the RCA got dir %q, read %q", dir, saw)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the RCA worktree was left behind")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "main.go")); string(b) == "scribble" {
		t.Error("the owner's checkout was written")
	}
	asked := false
	for _, it := range tk.Items {
		asked = asked || it.Kind == "incident" && it.Pending != "" && strings.Contains(it.Detail, "Pipe gets a nil conn")
	}
	if !asked {
		t.Fatalf("no prepared fix with the RCA: %+v", tk.Items)
	}
	if rep, _ := os.ReadFile(tk.Report); !strings.Contains(string(rep), "Pipe gets a nil conn") {
		t.Error("the RCA is not next to the question in the report")
	}
}
