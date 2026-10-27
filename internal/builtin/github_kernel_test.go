package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// prLabels: a PR's labels in the fake.
func (w *ghWorld) prLabels(n int) []string {
	var out []string
	w.fake.With(func(f *githubtest.Fake) {
		if p := f.PRs[n]; p != nil {
			out = github.LabelNames(p.Labels)
		}
	})
	return out
}

func (w *ghWorld) comments(n int) []string {
	var out []string
	w.fake.With(func(f *githubtest.Fake) { out = append(out, f.Comments[n]...) })
	return out
}

func held(labels []string) bool {
	return strings.Contains(","+strings.Join(labels, ",")+",", ",needs-human,") && strings.Contains(","+strings.Join(labels, ",")+",", ",kernel,")
}

func TestKernelMergeRefused(t *testing.T) {
	cases := []struct {
		name, title, author string
		files               []string
	}{
		{"own", "fix: tidy the policy", "warden-bot", []string{"hello.go", "internal/policy/policy.go"}},
		{"dependabot actions bump", "build(deps): bump actions/checkout from 4.1.0 to 4.2.0", "dependabot[bot]", []string{".github/workflows/ci.yml"}},
		{"trusted", "fix: raise the cap", "ops-agent", []string{"internal/meter/meter.go"}},
		{"self-protection: the kernel list itself", "chore: drop a kernel pattern", "warden-bot", []string{"internal/github/kernel.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.env.Params["trusted_authors"] = `["ops-agent"]`
			n := w.fake.AddPR(tc.title, tc.author, "warden/issue-9-x", "sha1", tc.files, "green")
			r := mergeFix(t, w)
			if r.OK || !r.Refused || merged(w, n) || !strings.Contains(r.Summary, "human-only kernel") {
				t.Fatalf("%+v", r)
			}
			if !held(w.prLabels(n)) {
				t.Fatalf("labels %v", w.prLabels(n))
			}
			cs := w.comments(n)
			if len(cs) != 1 || !strings.HasPrefix(cs[0], "@boss — human-only kernel:") || !strings.Contains(cs[0], tc.files[len(tc.files)-1]) {
				t.Fatalf("comments %q", cs)
			}
			// labeled: no longer proposed for a merge
			if fixOf(w.observe("github-prs"), "merge-pr") != nil {
				t.Fatal("a kernel PR proposed for merge again")
			}
		})
	}
}

func TestKernelIsAFloor(t *testing.T) {
	w := newWorld(t)
	w.env.Params["protected"] = "[]"
	w.env.Params["protected_names"] = ""
	files := []string{".github/workflows/ci.yml", "SECURITY.md", "internal/policy/policy.go", "internal/github/kernel.go", "internal/box/box.go", "my_token.txt", "hello.go"}
	if got := protectedFor(w.env, files); strings.Join(got, ",") != strings.Join(github.KernelFiles(files), ",") || len(got) != 6 {
		t.Fatalf("protected = [] removed kernel paths: %v", got)
	}
	w.env.Params["protected"] = `["docs/**"]`
	if got := protectedFor(w.env, []string{"docs/x.md", "internal/meter/meter.go", "hello.go"}); strings.Join(got, ",") != "docs/x.md,internal/meter/meter.go" {
		t.Fatalf("params add to the kernel: %v", got)
	}
	// and through merge-pr
	w.env.Params["protected"] = "[]"
	n := w.fake.AddPR("fix: x", "warden-bot", "warden/issue-3-x", "s3", []string{"internal/policy/policy.go"}, "green")
	if r := mergeFix(t, w); !r.Refused || merged(w, n) || !held(w.prLabels(n)) {
		t.Fatalf("%+v", r)
	}
}

func TestMergeAllowed(t *testing.T) {
	w := newWorld(t)
	c, err := ghClient(w.env)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ok := w.fake.AddPR("fix: a", "warden-bot", "warden/issue-1-a", "a1", []string{"hello.go"}, "green")
	k := w.fake.AddPR("fix: b", "warden-bot", "warden/issue-2-b", "b1", []string{"hello.go"}, "green")
	pr, _ := c.PR(ctx, k)
	// the files change after merge-pr's first look: the check right before the merge sees them
	w.fake.With(func(f *githubtest.Fake) { f.PRs[k].Files = []string{"hello.go", ".goreleaser.yaml"} })
	if r, allowed := mergeAllowed(ctx, c, w.env, pr); allowed || !r.Refused || !held(w.prLabels(k)) {
		t.Fatalf("%+v", r)
	}
	if pok, _ := c.PR(ctx, ok); func() bool { _, a := mergeAllowed(ctx, c, w.env, pok); return !a }() {
		t.Fatal("a PR outside the kernel refused")
	}
	// and the client refuses on its own
	if _, err := c.Merge(ctx, k, "b1", "b"); err == nil || merged(w, k) {
		t.Fatal("the client merged a kernel PR")
	}
	// one comment per head commit
	markKernel(ctx, c, w.env, k, "b1", []string{".goreleaser.yaml"})
	if cs := w.comments(k); len(cs) != 1 {
		t.Fatalf("comments %q", cs)
	}
	markKernel(ctx, c, w.env, k, "b2", []string{".goreleaser.yaml"})
	if cs := w.comments(k); len(cs) != 2 || !strings.Contains(cs[1], "<!-- warden:kernel b2 -->") {
		t.Fatalf("comments %q", cs)
	}
}

func TestWorkIssueKernel(t *testing.T) {
	t.Run("the owner's issue: pushed, a ready PR labeled kernel, never merged", func(t *testing.T) {
		w := newWorld(t)
		w.fake.AddIssue("Lower the budget floor", "x", "boss", "OWNER", "warden", "small")
		w.env.Brains = brains(&coderBrain{fn: writer("internal/policy/floor.go", "package policy\n")}, nil)
		f := fixOf(w.observe("github-issues"), "work-issue")
		r := Act(context.Background(), "work-issue", nil, *f, w.env)
		if !r.OK || !strings.Contains(r.Summary, "human-only kernel") {
			t.Fatalf("%+v", r)
		}
		var pr *githubtest.PR
		w.fake.With(func(f *githubtest.Fake) {
			for _, p := range f.PRs {
				pr = p
			}
		})
		if pr == nil || pr.Draft || !held(w.prLabels(pr.Number)) || !strings.Contains(pr.Body, "human-only kernel: internal/policy/floor.go") {
			t.Fatalf("PR %+v labels %v", pr, w.prLabels(pr.Number))
		}
		if cs := w.comments(pr.Number); len(cs) != 1 || !strings.HasPrefix(cs[0], "@boss") {
			t.Fatalf("comments %q", cs)
		}
		if mr := Act(context.Background(), "merge-pr", nil, *mergeFinding(pr), w.env); mr.OK || merged(w, pr.Number) {
			t.Fatalf("merged: %+v", mr)
		}
	})
	t.Run("someone else's issue: nothing pushed", func(t *testing.T) {
		w := newWorld(t)
		w.fake.AddIssue("Tweak the sandbox", "x", "stranger", "NONE", "warden", "small")
		w.env.Brains = brains(&coderBrain{fn: writer("internal/box/box.go", "package box\n")}, nil)
		f := fixOf(w.observe("github-issues"), "work-issue")
		r := Act(context.Background(), "work-issue", nil, *f, w.env)
		if r.OK || !r.Refused || !strings.Contains(r.Summary, "human-only kernel") || w.ref("warden/issue-1-tweak-the-sandbox") != "" {
			t.Fatalf("%+v", r)
		}
		w.fake.With(func(f *githubtest.Fake) {
			if !f.Issues[1].Has("needs-human") || len(f.PRs) != 0 {
				t.Fatal("the issue must be labeled needs-human and no PR opened")
			}
		})
	})
}

func mergeFinding(p *githubtest.PR) *domain.Finding {
	return &domain.Finding{Fix: &domain.Fix{Act: "merge-pr", Params: map[string]string{"number": itoa(p.Number), "sha": p.Head.SHA}}}
}

func TestFixRoundKernel(t *testing.T) {
	for _, owner := range []bool{true, false} {
		w := newWorld(t)
		author, assoc := "boss", "OWNER"
		if !owner {
			author, assoc = "stranger", "NONE"
		}
		w.fake.AddIssue("small fix", "x", author, assoc, "warden", "small")
		w.env.Brains = brains(&coderBrain{fn: writer("a.go", "package proj\nvar A = 1\n")}, &coderBrain{fn: writer("internal/meter/cap.go", "package meter\n")})
		f := fixOf(w.observe("github-issues"), "work-issue")
		if r := Act(context.Background(), "work-issue", nil, *f, w.env); !r.OK {
			t.Fatal(r)
		}
		var prn int
		var sha string
		w.fake.With(func(f *githubtest.Fake) {
			for _, p := range f.PRs {
				prn, sha = p.Number, p.Head.SHA
			}
			f.Checks[sha] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
		})
		fx := fixOf(w.observe("github-prs"), "work-issue")
		if fx == nil || fx.Fix.Params["phase"] != "fix" {
			t.Fatal("no fix round")
		}
		r := Act(context.Background(), "work-issue", nil, *fx, w.env)
		pushed := w.ref("warden/issue-1-small-fix") != sha
		if owner {
			if !r.OK || !pushed || !held(w.prLabels(prn)) || len(w.comments(prn)) != 2 {
				t.Fatalf("owner: %+v pushed=%v labels=%v comments=%q", r, pushed, w.prLabels(prn), w.comments(prn))
			}
			continue
		}
		if r.OK || !r.Refused || pushed || held(w.prLabels(prn)) {
			t.Fatalf("stranger: %+v pushed=%v", r, pushed)
		}
	}
}

func TestPickupKernel(t *testing.T) {
	w := newWorld(t)
	b := "claude/ci"
	run(t, w.root, "git", "checkout", "-q", "-b", b, "origin/main")
	_ = os.MkdirAll(filepath.Join(w.root, ".github", "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(w.root, ".github", "workflows", "ci.yml"), []byte("name: CI2\n"), 0o644)
	run(t, w.root, "git", "commit", "-q", "-am", "ci: faster CI")
	run(t, w.root, "git", "push", "-q", "origin", b)
	run(t, w.root, "git", "checkout", "-q", "main")
	sha := w.ref(b)
	w.fake.With(func(f *githubtest.Fake) { f.Branches[b] = sha })
	pr := w.pickup(b)
	if !held(w.prLabels(pr.Number)) || len(w.comments(pr.Number)) != 1 {
		t.Fatalf("labels %v comments %q", w.prLabels(pr.Number), w.comments(pr.Number))
	}
}

func TestKernelPRsInTheDigest(t *testing.T) {
	w := newWorld(t)
	k := w.fake.AddPR("ci: bump for @alice", "dependabot[bot]", "dependabot/x", "d1", []string{"hello.go", ".github/workflows/ci.yml"}, "green", "needs-human", "kernel")
	w.fake.AddPR("fix: other", "warden-bot", "warden/issue-5-o", "o1", []string{"hello.go"}, "green", "needs-human")
	c, err := ghClient(w.env)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := KernelPRs(context.Background(), c)
	want := "kernel PR #" + itoa(k) + ": ci: bump for @\u200balice — human-only: .github/workflows/ci.yml"
	if err != nil || len(lines) != 1 || lines[0] != want {
		t.Fatalf("%q %v", lines, err)
	}
	if _, err := GitHubNote(context.Background(), w.env.Params, "Digest", map[string]any{"day": "2026-10-27", "lines": []any{"✅ merged PR #3"}}, nil); err != nil {
		t.Fatal(err)
	}
	var digest string
	w.fake.With(func(f *githubtest.Fake) {
		for n, i := range f.Issues {
			if i.Has("warden-log") && len(f.Comments[n]) > 0 {
				digest = f.Comments[n][0]
			}
		}
	})
	if !strings.Contains(digest, "- "+want+"\n- ✅ merged PR #3") || strings.Contains(digest, "@alice") {
		t.Fatalf("digest: %s", digest)
	}
}
