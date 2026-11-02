package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/experiment"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// The DCO: params.signoff, commit_name and commit_email on every commit
// Warden makes and on the squash message merge-pr sends.

type idCase struct {
	name   string
	params map[string]string
	author string // "name <email>", the commit's author and committer
	signed string // the message's last line; "": no Signed-off-by at all
}

var idCases = []idCase{
	{"off", nil, "Warden <warden@localhost>", ""},
	{"identity only", map[string]string{"commit_name": "Ruslan", "commit_email": "r@example.com"}, "Ruslan <r@example.com>", ""},
	{"on", map[string]string{"signoff": "on", "commit_name": "Ruslan", "commit_email": "r@example.com"}, "Ruslan <r@example.com>", "Signed-off-by: Ruslan <r@example.com>"},
	{"on, Warden's identity", map[string]string{"signoff": "on"}, "Warden <warden@localhost>", "Signed-off-by: Warden <warden@localhost>"},
}

// checkHead: the head commit of branch in the repository at gitDir.
func checkHead(t *testing.T, dir, gitDir, branch string, c idCase) {
	t.Helper()
	out := run(t, dir, "git", "--git-dir", gitDir, "log", "-1", "--format=%an <%ae>%n%cn <%ce>%n%B", branch)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 || lines[0] != c.author || lines[1] != c.author {
		t.Fatalf("author/committer of %s, want %s:\n%s", branch, c.author, out)
	}
	msg := strings.TrimSpace(strings.Join(lines[2:], "\n"))
	switch {
	case c.signed == "" && strings.Contains(msg, "Signed-off-by"):
		t.Fatalf("signed off with signoff unset:\n%s", msg)
	case c.signed != "" && (!strings.HasSuffix(msg, "\n"+c.signed) || strings.Count(msg, "Signed-off-by") != 1):
		t.Fatalf("want the message to end with %q:\n%s", c.signed, msg)
	}
}

func setParams(w *ghWorld, params map[string]string) {
	for k, v := range params {
		w.env.Params[k] = v
	}
}

func mustAct(t *testing.T, w *ghWorld, observer, act string) Result {
	t.Helper()
	f := fixOf(w.observe(observer), act)
	if f == nil {
		t.Fatalf("no %s fix from %s", act, observer)
	}
	r := Act(context.Background(), act, nil, *f, w.env)
	if !r.OK {
		t.Fatalf("%s: %+v", act, r)
	}
	return r
}

// Each act that commits, run with the params to its pushed branch.
var commitPaths = []struct {
	name string
	run  func(t *testing.T, params map[string]string) (*ghWorld, string)
}{
	{"work-issue", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w := newWorld(t)
		setParams(w, params)
		w.fake.AddIssue("Add a Goodbye function", "Please add Goodbye().", "boss", "OWNER", "warden", "small")
		w.env.Brains = brains(&coderBrain{fn: writer("goodbye.go", "package proj\n\nfunc Goodbye() string { return \"bye\" }\n")}, nil)
		mustAct(t, w, "github-issues", "work-issue")
		return w, "warden/issue-1-add-a-goodbye-function"
	}},
	{"fix round", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w := newWorld(t)
		setParams(w, params)
		w.fake.AddIssue("small fix", "x", "boss", "OWNER", "warden", "small")
		w.env.Brains = brains(&coderBrain{fn: writer("a.go", "package proj\nvar A = 1\n")}, &coderBrain{fn: writer("a.go", "package proj\n\nvar A = 2\n")})
		mustAct(t, w, "github-issues", "work-issue")
		w.fake.With(func(f *githubtest.Fake) {
			for _, p := range f.PRs {
				f.Checks[p.Head.SHA] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
			}
		})
		branch := "warden/issue-1-small-fix"
		before := w.ref(branch)
		if r := mustAct(t, w, "github-prs", "work-issue"); !strings.Contains(r.Summary, "fix round pushed") || w.ref(branch) == before {
			t.Fatalf("fix round: %+v", r)
		}
		return w, branch
	}},
	{"release", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w := newWorld(t)
		setParams(w, params)
		t0 := w.fake.Now.Add(-48 * time.Hour)
		w.fake.With(func(f *githubtest.Fake) {
			tg := github.Tag{Name: "v0.1.0"}
			tg.Commit.SHA = "tag010"
			f.Tags = append(f.Tags, tg)
			f.Dates["tag010"] = t0
		})
		w.fake.MergedPR("fix: a typo", "fix/typo", t0.Add(time.Hour))
		mainSHA := ""
		w.fake.With(func(f *githubtest.Fake) { mainSHA = f.Branches["main"] })
		w.fake.AddRun(github.WorkflowRun{Name: "CI", HeadBranch: "main", HeadSHA: mainSHA, Event: "push", Status: "completed", Conclusion: "success", WorkflowID: 1})
		mustAct(t, w, "github-main", "release")
		return w, "warden/release-v0.1.1"
	}},
	{"publish-log", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w := newWorld(t)
		setParams(w, params)
		w.env.Params["experiment_log"] = "on"
		if err := experiment.Append(TasksLog(w.env.State), experiment.Event{At: w.fake.Now.Add(-time.Hour), Act: "work-issue", Issue: 3, PR: 4, Phase: "new", Brain: "local", OK: true}); err != nil {
			t.Fatal(err)
		}
		r := mustAct(t, w, "github-experiment", "publish-log")
		return w, r.Branch
	}},
	{"revert", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w := newWorld(t)
		setParams(w, params)
		_, sha := brokenMain(t, w, "warden/issue-7-break", "warden-bot", "push", "success")
		mustAct(t, w, "github-main", "revert-pr")
		return w, "warden/revert-" + sha[:7]
	}},
	{"roadmap", func(t *testing.T, params map[string]string) (*ghWorld, string) {
		w, _ := roadmapWorld(t)
		setParams(w, params)
		mustAct(t, w, "github-roadmap", "roadmap")
		return w, "warden/roadmap-2026-W44"
	}},
}

func TestSignoffOnEveryCommit(t *testing.T) {
	for _, p := range commitPaths {
		for _, c := range idCases {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				w, branch := p.run(t, c.params)
				if w.ref(branch) == "" {
					t.Fatalf("%s not pushed", branch)
				}
				checkHead(t, w.root, w.bare, branch, c)
			})
		}
	}
}

func TestSignoffOnBump(t *testing.T) {
	for _, c := range idCases {
		t.Run(c.name, func(t *testing.T) {
			root := repoWith(t, map[string]string{"package.json": `{"name":"app","dependencies":{"a":"^1.0.0"}}`, "package-lock.json": lock})
			env, _ := bumpEnv(t, func(string) bool { return true })
			var ran []string
			env.Run, env.Params = fakeNpm(&ran), c.params
			res := Act(context.Background(), "npm-bump", map[string]any{"root": root}, pkgFinding("npm", "a", "1.0.0", "1.1.0"), env)
			if !res.OK {
				t.Fatalf("%+v", res)
			}
			checkHead(t, root, filepath.Join(root, ".git"), res.Branch, c)
		})
	}
}

// mergeBody: the request body merge-pr sent to merge PR n.
func mergeBody(t *testing.T, w *ghWorld, n int) map[string]string {
	t.Helper()
	var in map[string]string
	w.fake.With(func(f *githubtest.Fake) {
		for i, r := range f.Requests {
			if r == "PUT /repos/acme/proj/pulls/"+itoa(n)+"/merge" || strings.HasSuffix(r, " /pulls/"+itoa(n)+"/merge") {
				_ = json.Unmarshal([]byte(f.Bodies[i]), &in)
			}
		}
	})
	if in == nil {
		t.Fatalf("no merge request for #%d", n)
	}
	return in
}

// pushBranch: commits on a new branch of the remote, one per message; its head.
func pushBranch(t *testing.T, w *ghWorld, branch string, msgs ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dev")
	run(t, filepath.Dir(dir), "git", "clone", "-q", w.bare, dir)
	run(t, dir, "git", "checkout", "-q", "-b", branch)
	for i, m := range msgs {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)+".go"), []byte("package proj\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, dir, "git", "add", ".")
		run(t, dir, "git", "commit", "-q", "-m", m)
	}
	run(t, dir, "git", "push", "-q", "origin", branch)
	return run(t, dir, "git", "rev-parse", "HEAD")
}

func TestSquashMessageSignoff(t *testing.T) {
	signed := []string{"fix: one\n\nSigned-off-by: Dev <dev@example.com>\nCo-authored-by: Pal <pal@example.com>", "fix: two\n\nSigned-off-by: Dev <dev@example.com>"}
	for _, c := range []struct {
		name    string
		params  map[string]string
		compare bool
		want    string // commit_message; "": none sent (GitHub's default)
	}{
		{"off", nil, true, ""},
		{"off, identity only", map[string]string{"commit_name": "Ruslan", "commit_email": "r@example.com"}, true, ""},
		{"on: the commits' trailers, then Warden's", map[string]string{"signoff": "on", "commit_name": "Ruslan", "commit_email": "r@example.com"}, true,
			"Signed-off-by: Dev <dev@example.com>\nCo-authored-by: Pal <pal@example.com>\nSigned-off-by: Ruslan <r@example.com>"},
		{"on, the comparison fails: Warden's alone", map[string]string{"signoff": "on"}, false, "Signed-off-by: Warden <warden@localhost>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			setParams(w, c.params)
			branch := "warden/issue-3-a-bug"
			sha := pushBranch(t, w, branch, signed...)
			if !c.compare {
				w.fake.With(func(f *githubtest.Fake) { f.CommitsOf = nil })
			}
			n := w.fake.AddPR("fix: a bug (#3)", "warden-bot", branch, sha, []string{"f0.go", "f1.go"}, "green", "warden")
			if r := mergeFix(t, w); !r.OK || !merged(w, n) {
				t.Fatalf("merge: %+v", r)
			}
			in := mergeBody(t, w, n)
			if got, ok := in["commit_message"]; got != c.want || ok != (c.want != "") {
				t.Fatalf("commit_message %q (sent: %v), want %q", got, ok, c.want)
			}
			if in["commit_title"] != "fix: a bug (#3) (#"+itoa(n)+")" || in["merge_method"] != "squash" {
				t.Fatalf("merge body: %v", in)
			}
		})
	}
}

func TestCoderRules(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "CONTRIBUTING.md"), []byte("# Contributing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ rules, want string }{
		{"", ""},
		{"CONTRIBUTING.md", "- Before you change anything, read CONTRIBUTING.md (read_file) and follow its rules for contributors.\n"},
		{"Use table-driven tests.\nNo new dependencies.", "- The owner's rules for this repository:\nUse table-driven tests.\nNo new dependencies.\n"},
		{"MISSING.md", "- The owner's rules for this repository:\nMISSING.md\n"}, // not a file here: the text itself
		{"../CONTRIBUTING.md", "- The owner's rules for this repository:\n../CONTRIBUTING.md\n"},
	} {
		env := &Env{Params: map[string]string{"repo": "acme/proj", "coder_rules": c.rules}}
		if got := coderRules(env, wt); got != c.want {
			t.Errorf("coder_rules %q: %q, want %q", c.rules, got, c.want)
		}
		if p := coderPrompt(env, wt); !strings.Contains(p, c.want) || !strings.HasSuffix(p, "for the pull request description.") {
			t.Errorf("coder_rules %q not in the prompt:\n%s", c.rules, p)
		}
	}
}
