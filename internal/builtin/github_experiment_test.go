package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/experiment"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

func TestPublishLog(t *testing.T) {
	w := newWorld(t)
	w.env.Params["experiment_log"] = "on"
	w.env.Params["experiment_tz"] = "Europe/Moscow"
	if fs := w.observe("github-experiment"); len(fs) != 0 {
		t.Fatalf("no records, nothing to publish: %+v", fs)
	}
	log := TasksLog(w.env.State)
	evs := []experiment.Event{
		{At: w.fake.Now.Add(-3 * time.Hour), Kind: "idle", Minutes: 60, Reason: "off-hours"},
		{At: w.fake.Now.Add(-2 * time.Hour), Act: "work-issue", Issue: 3, PR: 4, Phase: "new", Brain: "local", OK: true, Summary: "draft PR #4 for issue #3 (brain: local)",
			WallMS: 61_000, TokensIn: 1000, TokensOut: 100, Data: map[string]string{"size": "S"}},
	}
	for _, e := range evs {
		if err := experiment.Append(log, e); err != nil {
			t.Fatal(err)
		}
	}
	f := fixOf(w.observe("github-experiment"), "publish-log")
	if f == nil || f.Fix.Params["tainted"] != "false" || f.Fix.Target != "docs/experiment-27b.md" || f.Key != "experiment|2026-10-27" {
		t.Fatalf("publish-log fix: %+v", f)
	}
	r := Act(context.Background(), "publish-log", nil, *f, w.env)
	branch := "warden/experiment-2026-10-27"
	if !r.OK || r.Branch != branch || w.ref(branch) == "" {
		t.Fatalf("publish-log: %+v", r)
	}
	loaded, _ := experiment.Load(log)
	msk, _ := time.LoadLocation("Europe/Moscow")
	want := experiment.Aggregate(loaded, msk).Markdown(w.fake.Now)
	if got := run(t, w.root, "git", "--git-dir", w.bare, "show", branch+":docs/experiment-27b.md"); got != strings.TrimSpace(want) {
		t.Fatalf("the published log:\n%s", got)
	}
	if files := run(t, w.root, "git", "--git-dir", w.bare, "diff", "--name-only", "main..."+branch); files != "docs/experiment-27b.md" {
		t.Fatalf("the PR must touch only the log: %q", files)
	}
	var pr *githubtest.PR
	w.fake.With(func(f *githubtest.Fake) {
		for _, p := range f.PRs {
			if p.Head.Ref == branch {
				pr = p
			}
		}
	})
	if pr == nil || pr.Draft || pr.Title != "docs: 27B experiment log for 2026-10-27" || !pr.Has("warden") || !pr.Has("experiment") {
		t.Fatalf("PR: %+v", pr)
	}
	// while it is open: no second one
	if fixOf(w.observe("github-experiment"), "publish-log") != nil {
		t.Fatal("a second log PR proposed while one is open")
	}
	// docs/ is not protected: merge-pr merges it on green CI
	if prot := Protected([]string{"docs/experiment-27b.md"}, DefaultProtected, DefaultProtectedNames); len(prot) != 0 {
		t.Fatalf("docs/ protected: %v", prot)
	}
	w.fake.SetCI(pr.Head.SHA, "green")
	if r := mergeFix(t, w); !r.OK || !merged(w, pr.Number) || r.Data != nil {
		t.Fatalf("merge of the log PR: %+v", r)
	}
	// the daily log PR is no reason for a release
	w.fake.With(func(f *githubtest.Fake) {
		tg := github.Tag{Name: "v0.1.0"}
		tg.Commit.SHA = "tag010"
		f.Tags = append(f.Tags, tg)
		f.Dates["tag010"] = w.fake.Now.Add(-48 * time.Hour)
		f.Runs = append(f.Runs, github.WorkflowRun{ID: 1, Name: "CI", HeadBranch: "main", HeadSHA: f.Branches["main"], Event: "push", Status: "completed", Conclusion: "success", WorkflowID: 1})
	})
	if fixOf(w.observe("github-main"), "release") != nil {
		t.Fatal("a release proposed for the experiment log alone")
	}
	// published today: nothing more today
	if fs := w.observe("github-experiment"); len(fs) != 0 {
		t.Fatalf("published today already: %+v", fs)
	}
	// main has the same log: no change, no PR
	run(t, w.root, "git", "--git-dir", w.bare, "branch", "-f", "main", branch)
	_ = os.Remove(filepath.Join(w.env.State, "github", "daily.json"))
	before := 0
	w.fake.With(func(f *githubtest.Fake) { before = len(f.PRs) })
	r = Act(context.Background(), "publish-log", nil, *f, w.env)
	w.fake.With(func(f *githubtest.Fake) {
		if !r.OK || !strings.Contains(r.Summary, "no change") || len(f.PRs) != before {
			t.Fatalf("unchanged log: %+v", r)
		}
	})
	// off: refused
	w.env.Params["experiment_log"] = "off"
	if r := Act(context.Background(), "publish-log", nil, *f, w.env); !r.Refused {
		t.Fatalf("experiment_log off: %+v", r)
	}
	w.noSecret(r)
}
