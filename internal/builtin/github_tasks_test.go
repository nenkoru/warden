package builtin

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/experiment"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// only: every task routed to one brain (brain_for = { work-issue = "local" }).
func only(name string, b brain.Brain) *Brains {
	return &Brains{Route: func(string) string { return name }, Auto: []string{"local", "claude"},
		Get: func(n string) (NamedBrain, error) {
			if n == name {
				return NamedBrain{Name: name, Brain: b}, nil
			}
			return NamedBrain{Name: n}, os.ErrNotExist
		}}
}

func TestLargeFeatureGoesToTheConfiguredBrain(t *testing.T) {
	w := newWorld(t)
	local := &coderBrain{fn: func(_ string, tools []brain.Tool) string {
		var b strings.Builder
		for i := range 60 {
			b.WriteString("// line " + itoa(i) + "\n")
		}
		use(tools, "write_file", map[string]any{"path": "scheduler.go", "content": "package proj\n\n" + b.String()})
		return "rewrote the scheduler"
	}}
	w.env.Brains = only("local", local)
	// a delegate is configured, and still a large feature goes to the local brain: never delegated for its size
	w.env.Params["delegate_cmd"] = `["true"]`
	w.env.Params["delegate_default"] = "Coder"
	w.fake.AddIssue("Rewrite the scheduler", "a big feature", "boss", "OWNER", "enhancement")
	if r := PickBrains(w.env, "work-issue", false); r.Hard || len(r.Brains) != 1 || r.Brains[0].Name != "local" {
		t.Fatalf("route: %+v", r)
	}
	fs := w.observe("github-issues")
	if fixOf(fs, "delegate") != nil {
		t.Fatal("a large issue was delegated though a brain is configured for work-issue")
	}
	f := fixOf(fs, "work-issue")
	if f == nil {
		t.Fatal("no work-issue for a large enhancement")
	}
	r := Act(context.Background(), "work-issue", nil, *f, w.env)
	if !r.OK || local.calls != 1 || !strings.Contains(r.Summary, "brain: local") {
		t.Fatalf("%+v calls=%d", r, local.calls)
	}
	d := r.Data
	if d["issue"] != "1" || d["pr"] != "2" || d["phase"] != "new" || d["brain"] != "local" || d["files"] != "1" || d["lines_added"] != "62" ||
		d["lines_deleted"] != "0" || d["size"] != "M" || d["tool_calls"] != "1" || d["tool_errors"] != "0" {
		t.Fatalf("data: %v", d)
	}
}

func TestTaskDataAcrossFixGiveUpAndMerge(t *testing.T) {
	w := newWorld(t)
	w.fake.AddIssue("small fix", "x", "boss", "OWNER", "warden", "size:XL")
	// a coder that fails a tool three times the same way before writing
	coder := &coderBrain{fn: func(_ string, tools []brain.Tool) string {
		for range 3 {
			use(tools, "read_file", map[string]any{"path": "missing.go"})
		}
		use(tools, "write_file", map[string]any{"path": "a.go", "content": "package proj\nvar A = 1\n"})
		return "done"
	}}
	w.env.Brains = brains(coder, nil)
	f := fixOf(w.observe("github-issues"), "work-issue")
	r := Act(context.Background(), "work-issue", nil, *f, w.env)
	if !r.OK || r.Data["size"] != "XL" || r.Data["tool_calls"] != "4" || r.Data["tool_errors"] != "3" || r.Data["repeats"] != "2" || r.Data["steps"] != "1" ||
		r.Data["unknown_tool"] != "0" || r.Data["context_tokens"] != "262144" {
		t.Fatalf("new: %+v", r)
	}
	var prn int
	var sha string
	w.fake.With(func(f *githubtest.Fake) {
		for _, p := range f.PRs {
			prn, sha = p.Number, p.Head.SHA
		}
		f.Checks[sha] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
	})
	coder.fn = writer("a.go", "package proj\n\nvar A = 2\n")
	fx := fixOf(w.observe("github-prs"), "work-issue")
	r = Act(context.Background(), "work-issue", nil, *fx, w.env)
	d := r.Data
	if !r.OK || d["phase"] != "fix" || d["issue"] != "1" || d["pr"] != itoa(prn) || d["ci_first_push"] != "red" || d["fix_round"] != "1" || d["files"] != "1" {
		t.Fatalf("fix: %+v", r)
	}
	w.fake.With(func(f *githubtest.Fake) {
		p := f.PRs[prn]
		p.Head.SHA = w.ref("warden/issue-1-small-fix")
		f.Checks[p.Head.SHA] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
	})
	g := fixOf(w.observe("github-prs"), "work-issue")
	r = Act(context.Background(), "work-issue", nil, *g, w.env)
	if !r.Refused || r.Data["phase"] != "giveup" || r.Data["issue"] != "1" || r.Data["pr"] != itoa(prn) {
		t.Fatalf("giveup: %+v", r)
	}
	if cs := experiment.Categories("giveup", r.OK, r.Summary, r.Data); len(cs) != 1 || cs[0] != experiment.GaveUp {
		t.Fatalf("giveup categories: %v", cs)
	}

	// merged green on the first push: people's comments and reviews counted, Warden's own not
	w2 := newWorld(t)
	n := w2.fake.AddPR("feat: x (#5)", "warden-bot", "warden/issue-5-x", "own5", []string{"hello.go"}, "green", "warden")
	w2.fake.AddComment(n, "boss", "nice")
	w2.fake.AddComment(n, "warden-bot", "Warden pushed one fix round")
	w2.fake.With(func(f *githubtest.Fake) {
		f.Reviews = append(f.Reviews, githubtest.Review{N: n, Body: "lgtm", User: "boss"}, githubtest.Review{N: n, Body: "mine"}, githubtest.Review{N: 99, User: "x"})
	})
	r = mergeFix(t, w2)
	d = r.Data
	if !r.OK || d["phase"] != "merge" || d["issue"] != "5" || d["pr"] != itoa(n) || d["ci_first_push"] != "green" || d["review_comments"] != "2" || d["files"] != "1" {
		t.Fatalf("merge: %+v", r)
	}
	// a PR that is not a warden/issue one is no task
	w2.fake.AddPR("build(deps): bump x from 1.2.0 to 1.3.0", "dependabot[bot]", "dependabot/x", "dep1", []string{"go.mod"}, "green")
	if r := mergeFix(t, w2); !r.OK || r.Data != nil {
		t.Fatalf("dependabot merge: %+v", r)
	}
}

func TestIssueSize(t *testing.T) {
	cases := []struct {
		labels []string
		data   map[string]string
		want   string
	}{
		{[]string{"small"}, nil, "S"},
		{[]string{"bug", "Size:L"}, map[string]string{"lines_added": "3", "lines_deleted": "0"}, "L"},
		{nil, map[string]string{"lines_added": "40", "lines_deleted": "9"}, "S"},
		{nil, map[string]string{"lines_added": "40", "lines_deleted": "10"}, "M"},
		{nil, map[string]string{"lines_added": "700", "lines_deleted": "99"}, "L"},
		{nil, map[string]string{"lines_added": "800", "lines_deleted": "0"}, "XL"},
		{[]string{"size:huge"}, nil, ""},
	}
	for _, c := range cases {
		if got := issueSize(c.labels, c.data); got != c.want {
			t.Errorf("%v %v: %q, want %q", c.labels, c.data, got, c.want)
		}
	}
}

func TestRevertOfWardensPRIsATaskEvent(t *testing.T) {
	w := newWorld(t)
	w.env.Params["release"] = "off"
	at := w.fake.Now.Add(-2 * time.Hour)
	orig := w.fake.MergedPR("feat: add x (#7)", "warden/issue-7-add-x", at)
	w.fake.MergedPR("fix: unrelated", "fix/y", at)
	byButton := w.fake.MergedPR(`Revert "feat: add x (#7)"`, "revert-"+itoa(orig)+"-warden/issue-7-add-x", at.Add(time.Hour))
	w.fake.MergedPR(`Revert "fix: unrelated"`, "revert-9-fix/y", at.Add(time.Hour))
	if fs := w.observe("github-main"); len(fs) != 0 {
		t.Fatalf("experiment_log off: no revert findings: %+v", fs)
	}
	w.env.Params["experiment_log"] = "on"
	var got []string
	for _, f := range w.observe("github-main") {
		if f.Kind == "revert" {
			got = append(got, f.Key+" "+f.Data["task_event"]+" issue="+f.Data["issue"]+" pr="+f.Data["pr"]+" by="+f.Data["revert_pr"])
		}
	}
	if len(got) != 1 || got[0] != "revert|"+itoa(byButton)+" revert issue=7 pr="+itoa(orig)+" by="+itoa(byButton) {
		t.Fatalf("reverts: %v", got)
	}
	// a revert of the squash commit by hand: the original's title and number quoted
	w2 := newWorld(t)
	w2.env.Params["release"], w2.env.Params["experiment_log"] = "off", "on"
	o2 := w2.fake.MergedPR("fix: y (#3)", "warden/issue-3-y", at)
	w2.fake.MergedPR(`Revert "fix: y (#3) (#`+itoa(o2)+`)"`, "owner/revert-y", at.Add(time.Hour))
	fs := w2.observe("github-main")
	if len(fs) != 1 || fs[0].Data["pr"] != itoa(o2) || fs[0].Data["issue"] != "3" {
		t.Fatalf("revert by hand: %+v", fs)
	}
}
