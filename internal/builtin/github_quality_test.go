package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// brokenMain: a commit on main as if PR #n (head ref) had been squash-merged
// by mergedBy, CI red on it from a run of event, parentCI on its parent.
func brokenMain(t *testing.T, w *ghWorld, ref, mergedBy, event, parentCI string) (int, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dev")
	run(t, filepath.Dir(dir), "git", "clone", "-q", w.bare, dir)
	parent := run(t, dir, "git", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package proj\n\nfunc Broken() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "-m", "feat: break things (#7)")
	run(t, dir, "git", "push", "-q", "origin", "main")
	sha := run(t, dir, "git", "rev-parse", "HEAD")
	n := w.fake.AddPR("feat: break things @someone", "warden-bot", ref, "head-"+ref, []string{"broken.go"}, "green", "warden")
	w.fake.With(func(f *githubtest.Fake) {
		p := f.PRs[n]
		now := f.Now
		p.State, p.Merged, p.MergedAt, p.MergeCommitSHA = "closed", true, &now, sha
		if mergedBy != "" {
			p.MergedBy = &github.User{Login: mergedBy}
		}
		f.Branches["main"] = sha
		f.Parents[sha] = []string{parent}
		f.Jobs[2] = []githubtest.Job{{ID: 21, Name: "test (ubuntu-latest)", Conclusion: "failure"}, {ID: 22, Name: "lint", Conclusion: "success"}}
	})
	w.fake.AddRun(github.WorkflowRun{ID: 1, Name: "CI", HeadBranch: "main", HeadSHA: parent, Event: "push", Status: "completed", Conclusion: parentCI, WorkflowID: 1})
	w.fake.AddRun(github.WorkflowRun{ID: 2, Name: "CI", HeadBranch: "main", HeadSHA: sha, Event: event, Status: "completed", Conclusion: "failure", WorkflowID: 1})
	return n, sha
}

func redMain(t *testing.T, w *ghWorld) (red, revert bool) {
	t.Helper()
	for _, f := range w.observe("github-main") {
		if strings.HasPrefix(f.Key, "main-red|") {
			red = true
			revert = f.Fix != nil && f.Fix.Act == "revert-pr"
		}
	}
	return red, revert
}

func TestRevertWardensMergeThatTurnedMainRed(t *testing.T) {
	w := newWorld(t)
	n, sha := brokenMain(t, w, "warden/issue-7-break", "warden-bot", "push", "success")
	f := fixOf(w.observe("github-main"), "revert-pr")
	if f == nil || f.Fix.Params["pr"] != itoa(n) || f.Fix.Params["sha"] != sha || f.Key != "main-red|"+sha {
		t.Fatalf("no revert-pr fix (once per commit: keyed on its sha): %+v", f)
	}
	r := Act(context.Background(), "revert-pr", nil, *f, w.env)
	if !r.OK {
		t.Fatalf("revert-pr: %+v", r)
	}
	branch := "warden/revert-" + sha[:7]
	if w.ref(branch) == "" {
		t.Fatalf("%s not pushed", branch)
	}
	if out := run(t, w.root, "git", "--git-dir", w.bare, "ls-tree", "--name-only", branch); strings.Contains(out, "broken.go") || !strings.Contains(out, "hello.go") {
		t.Fatalf("the revert did not undo the merge: %s", out)
	}
	if msg := run(t, w.root, "git", "--git-dir", w.bare, "log", "-1", "--format=%s%n%b", branch); !strings.Contains(msg, "Revert") || !strings.Contains(msg, sha) {
		t.Fatalf("revert commit: %s", msg)
	}
	var rp *githubtest.PR
	w.fake.With(func(f *githubtest.Fake) {
		for _, p := range f.PRs {
			if p.Head.Ref == branch {
				rp = p
			}
		}
	})
	if rp == nil || rp.Draft || rp.Title != `Revert "feat: break things @`+"\u200b"+`someone" (#`+itoa(n)+`)` ||
		!strings.Contains(rp.Body, "Main went red after #"+itoa(n)+" (failed: test (ubuntu-latest)). This reverts it.") ||
		!rp.Has("warden") || !rp.Has("revert") {
		t.Fatalf("revert PR: %+v", rp)
	}
	var comments []string
	w.fake.With(func(f *githubtest.Fake) { comments = f.Comments[n] })
	if len(comments) != 1 || !strings.Contains(comments[0], "@boss") || !strings.Contains(comments[0], "#"+itoa(rp.Number)) {
		t.Fatalf("comment on the original PR: %q", comments)
	}
	// green: merge-pr merges it as an own PR
	w.fake.SetCI(rp.Head.SHA, "green")
	if r := mergeFix(t, w); !r.OK || !merged(w, rp.Number) {
		t.Fatalf("revert PR merge: %+v", r)
	}
	w.noSecret(r)
}

func TestNoRevertUnlessWardensMergeBrokeIt(t *testing.T) {
	cases := []struct {
		name, ref, mergedBy, event, parentCI string
		unlink                               bool
	}{
		{"merged by the owner", "warden/issue-7-break", "boss", "push", "success", false},
		{"merged by nobody known", "warden/issue-7-break", "", "push", "success", false},
		{"a scheduled run went red (a new advisory)", "warden/issue-7-break", "warden-bot", "schedule", "success", false},
		{"main was red before it", "warden/issue-7-break", "warden-bot", "push", "failure", false},
		{"a revert is never reverted", "warden/revert-abc1234", "warden-bot", "push", "success", false},
		{"the owner's push (no PR)", "warden/issue-7-break", "warden-bot", "push", "success", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			n, _ := brokenMain(t, w, c.ref, c.mergedBy, c.event, c.parentCI)
			if c.unlink {
				w.fake.With(func(f *githubtest.Fake) { f.PRs[n].MergeCommitSHA = "elsewhere" })
			}
			if red, revert := redMain(t, w); !red || revert {
				t.Fatalf("red=%v revert=%v", red, revert)
			}
		})
	}
}

func TestRevertChecksAgain(t *testing.T) {
	w := newWorld(t)
	_, sha := brokenMain(t, w, "warden/issue-7-break", "warden-bot", "push", "success")
	f := fixOf(w.observe("github-main"), "revert-pr")
	if f == nil {
		t.Fatal("no revert-pr fix")
	}
	// the owner merged it by hand after all (the fix was proposed on stale data)
	w.fake.With(func(fk *githubtest.Fake) {
		for _, p := range fk.PRs {
			if p.MergeCommitSHA == sha {
				p.MergedBy = &github.User{Login: "boss"}
			}
		}
	})
	if r := Act(context.Background(), "revert-pr", nil, *f, w.env); !r.Refused || w.ref("warden/revert-"+sha[:7]) != "" {
		t.Fatalf("%+v", r)
	}
	// main moved on: nothing to do
	w.fake.With(func(fk *githubtest.Fake) { fk.Branches["main"] = "newer" })
	if r := Act(context.Background(), "revert-pr", nil, *f, w.env); !r.OK || !strings.Contains(r.Summary, "moved on") {
		t.Fatalf("%+v", r)
	}
}

// A revert goes through the same protected-path and kernel check as any
// change Warden pushes: one that touches them is the owner's.
func TestRevertNeverTouchesProtectedPaths(t *testing.T) {
	w := newWorld(t)
	_, sha := brokenMain(t, w, "warden/issue-7-break", "warden-bot", "push", "success")
	f := fixOf(w.observe("github-main"), "revert-pr")
	if f == nil {
		t.Fatal("no revert-pr fix")
	}
	w.env.Params["protected"] = `["broken.go"]`
	if r := Act(context.Background(), "revert-pr", nil, *f, w.env); !r.Refused || !strings.Contains(r.Summary, "broken.go") || w.ref("warden/revert-"+sha[:7]) != "" {
		t.Fatalf("%+v", r)
	}
}

// reviewer: a brain that answers a review with a verdict.
func reviewer(verdict string) *coderBrain {
	return &coderBrain{name: "claude", fn: func(string, []brain.Tool) string {
		return "- looked at the diff\n- @someone said hi\nVERDICT: " + verdict
	}}
}

func TestSecondReviewOffByDefault(t *testing.T) {
	w := newWorld(t)
	never := &coderBrain{name: "local", fn: func(string, []brain.Tool) string { t.Fatal("no brain is asked without review_brain"); return "" }}
	w.env.Brains = brains(never, never)
	n := w.fake.AddPR("fix: a bug (#3)", "warden-bot", "warden/issue-3-a-bug", "own1", []string{"hello.go"}, "green", "warden")
	r := mergeFix(t, w)
	if !r.OK || !merged(w, n) || strings.Contains(r.Summary, "review") || never.calls != 0 {
		t.Fatalf("%+v", r)
	}
	w.fake.With(func(f *githubtest.Fake) {
		if len(f.Comments[n]) != 0 || len(f.Reviews) != 0 {
			t.Fatalf("comments %q reviews %+v", f.Comments[n], f.Reviews)
		}
	})
}

func TestSecondReview(t *testing.T) {
	t.Run("changes-needed: refused, commented, not merged", func(t *testing.T) {
		w := newWorld(t)
		w.env.Params["review_brain"] = "claude"
		claude := reviewer("changes-needed")
		w.env.Brains = brains(nil, claude)
		n := w.fake.AddPR("fix: a bug (#3)", "warden-bot", "warden/issue-3-a-bug", "own1", []string{"hello.go"}, "green", "warden")
		f := fixOf(w.observe("github-prs"), "merge-pr")
		r := Act(context.Background(), "merge-pr", nil, *f, w.env)
		if !r.Refused || merged(w, n) || claude.calls != 1 {
			t.Fatalf("%+v", r)
		}
		if !strings.Contains(claude.tasks[0], "<<<untrusted") || !strings.Contains(claude.sys, "VERDICT") {
			t.Fatalf("the diff must be fenced, the reviewer prompt used: %s", claude.tasks[0])
		}
		var cs []string
		w.fake.With(func(f *githubtest.Fake) { cs = f.Comments[n] })
		if len(cs) != 1 || !strings.Contains(cs[0], "Second review") || !strings.Contains(cs[0], "@boss") || strings.Contains(cs[0], "@someone") {
			t.Fatalf("comment: %q", cs)
		}
		// a looks-good verdict (on a later try): merged
		claude.fn = reviewer("looks-good").fn
		if r := Act(context.Background(), "merge-pr", nil, *f, w.env); !r.OK || !merged(w, n) || !strings.Contains(r.Summary, "second review (claude): looks-good") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the review brain is unavailable: waits", func(t *testing.T) {
		w := newWorld(t)
		w.env.Params["review_brain"] = "nope"
		w.env.Brains = brains(nil, reviewer("looks-good"))
		n := w.fake.AddPR("fix: a bug (#3)", "warden-bot", "warden/issue-3-a-bug", "own1", []string{"hello.go"}, "green", "warden")
		if r := mergeFix(t, w); !r.Deferred || merged(w, n) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("only Warden's own issue PRs", func(t *testing.T) {
		w := newWorld(t)
		w.env.Params["review_brain"] = "claude"
		claude := reviewer("changes-needed")
		w.env.Brains = brains(nil, claude)
		n := w.fake.AddPR(`Revert "x" (#2)`, "warden-bot", "warden/revert-abc1234", "own2", []string{"hello.go"}, "green", "warden", "revert")
		if r := mergeFix(t, w); !r.OK || !merged(w, n) || claude.calls != 0 {
			t.Fatalf("%+v", r)
		}
	})
}
