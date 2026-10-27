package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// canaryWorld: v0.1.0 tagged, a feat merged since, main green, the canary
// gate on for "buvg01"; the release PR for v0.2.0 opened and merged.
func canaryWorld(t *testing.T) (*ghWorld, int) {
	t.Helper()
	w := newWorld(t)
	w.env.Params["canary"] = "buvg01"
	t0 := w.fake.Now.Add(-48 * time.Hour)
	w.fake.With(func(f *githubtest.Fake) {
		tg := github.Tag{Name: "v0.1.0"}
		tg.Commit.SHA = "tag010"
		f.Tags = append(f.Tags, tg)
		f.Dates["tag010"] = t0
	})
	w.fake.MergedPR("feat: add a goodbye (#7)", "warden/issue-7-x", t0.Add(time.Hour))
	mainSHA := ""
	w.fake.With(func(f *githubtest.Fake) { mainSHA = f.Branches["main"] })
	w.fake.AddRun(github.WorkflowRun{Name: "CI", HeadBranch: "main", HeadSHA: mainSHA, Event: "push", Status: "completed", Conclusion: "success", WorkflowID: 1})
	rel := releaseAndMerge(t, w, "")
	return w, rel
}

// releaseAndMerge: the release PR from the observer's fix, merged under
// merge-pr's rules; main moves to the release branch (the fake's merge does
// not touch git) and stays green.
func releaseAndMerge(t *testing.T, w *ghWorld, after string) int {
	t.Helper()
	var f *domain.Finding
	fs := w.observe("github-main")
	for i := range fs {
		if fs[i].Fix != nil && fs[i].Fix.Act == "release" && fs[i].Fix.Params["phase"] == "pr" {
			f = &fs[i]
		}
	}
	if f == nil || f.Fix.Params["after"] != after {
		t.Fatalf("release PR fix: %+v", f)
	}
	if r := Act(context.Background(), "release", nil, *f, w.env); !r.OK {
		t.Fatalf("release PR: %+v", r)
	}
	branch := "warden/release-" + f.Fix.Params["version"]
	var rel int
	w.fake.With(func(f *githubtest.Fake) {
		for _, p := range f.PRs {
			if p.Head.Ref == branch && p.State == "open" {
				rel = p.Number
				f.Checks[p.Head.SHA] = []github.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}
			}
		}
	})
	if r := mergeFix(t, w); !r.OK || !merged(w, rel) {
		t.Fatalf("release PR merge: %+v", r)
	}
	run(t, w.root, "git", "--git-dir", w.bare, "update-ref", "refs/heads/main", "refs/heads/"+branch)
	w.fake.With(func(f *githubtest.Fake) { f.Runs[0].HeadSHA = f.Branches["main"] })
	return rel
}

func beaconComment(name, version, rolledBack string, healthySince time.Time, ticks int) string {
	hs := ""
	if !healthySince.IsZero() {
		hs = fmt.Sprintf(`,"healthy_since":%q`, healthySince.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("**%s** · %s\n\n<!-- warden-fleet {\"name\":%q,\"version\":%q,\"channel\":\"canary\"%s,\"ticks_ok\":%d,\"rolled_back\":%q} -->\n",
		name, version, name, version, hs, ticks, rolledBack)
}

func (w *ghWorld) at(d time.Duration) {
	w.fake.With(func(f *githubtest.Fake) { f.Now = f.Now.Add(d) })
}

func (w *ghWorld) refs() map[string]string {
	out := map[string]string{}
	w.fake.With(func(f *githubtest.Fake) {
		for k, v := range f.Refs {
			out[k] = v
		}
	})
	return out
}

func findingKey(fs []domain.Finding, prefix string) *domain.Finding {
	for i := range fs {
		if strings.HasPrefix(fs[i].Key, prefix) {
			return &fs[i]
		}
	}
	return nil
}

// The release candidate first; the final tag on the same commit only once
// the canary ran it healthy for canary_hours and canary_min_ticks ticks.
func TestCanaryGate(t *testing.T) {
	w, rel := canaryWorld(t)
	mc := "m" + itoa(rel)
	f := fixOf(w.observe("github-main"), "release")
	if f == nil || f.Fix.Params["phase"] != "rc" || f.Fix.Params["rc"] != "v0.2.0-rc.1" || f.Fix.Params["sha"] != mc {
		t.Fatalf("rc fix: %+v", f)
	}
	// the plain tag phase is refused while the gate is on
	tagFix := *f.Fix
	tagFix.Params = map[string]string{"phase": "tag", "version": "v0.2.0", "sha": mc}
	if r := Act(context.Background(), "release", nil, domain.Finding{Fix: &tagFix}, w.env); !r.Refused {
		t.Fatalf("phase tag with the gate on: %+v", r)
	}
	if r := Act(context.Background(), "release", nil, *f, w.env); !r.OK || !strings.Contains(r.Summary, "v0.2.0-rc.1") {
		t.Fatalf("rc tag: %+v", r)
	}
	if refs := w.refs(); refs["refs/tags/v0.2.0-rc.1"] != mc || refs["refs/tags/v0.2.0"] != "" {
		t.Fatalf("refs: %v", refs)
	}
	// an rc is not a release: the latest is still v0.1.0, and while the rc
	// waits on the canary no second release PR is proposed
	w.fake.MergedPR("fix: later", "fix/later", w.fake.Now.Add(time.Minute))
	ri, err := releaseState(context.Background(), mustClient(t, w), w.env)
	if err != nil || ri.Tag != "v0.1.0" || ri.RC != "v0.2.0-rc.1" || len(ri.Fresh) != 1 {
		t.Fatalf("releaseState: %+v %v", ri, err)
	}
	fs := w.observe("github-main")
	if fixOf(fs, "release") != nil {
		t.Fatalf("a release act while the rc waits: %+v", fixOf(fs, "release"))
	}
	if s := findingKey(fs, "release-canary|"); s == nil || !strings.Contains(s.Title, "no beacon from the canary «buvg01»") {
		t.Fatalf("status: %+v", fs)
	}

	// beacons by untrusted authors (anyone may comment; a collaborator is not the owner) count for nothing
	fleet := w.fake.AddIssue("Warden fleet", "", "warden-bot", "OWNER", "warden-fleet")
	w.fake.AddComment(fleet, "mallory", beaconComment("buvg01", "v0.2.0-rc.1", "", w.fake.Now.Add(-72*time.Hour), 99))
	id := w.fake.AddComment(fleet, "carol", beaconComment("buvg01", "v0.2.0-rc.1", "", w.fake.Now.Add(-72*time.Hour), 99))
	w.fake.With(func(f *githubtest.Fake) { f.CommentAssoc[id] = "COLLABORATOR" })
	if fixOf(w.observe("github-main"), "release") != nil {
		t.Fatal("an untrusted beacon moved the release")
	}
	// the canary's own beacon (the owner's token): 2 h healthy, 3 ticks
	own := w.fake.AddComment(fleet, "boss", beaconComment("buvg01", "v0.2.0-rc.1", "", w.fake.Now.Add(-2*time.Hour), 3))
	w.fake.With(func(f *githubtest.Fake) { f.CommentAssoc[own] = "OWNER" })
	fs = w.observe("github-main")
	s := findingKey(fs, "release-canary|")
	if fixOf(fs, "release") != nil || s == nil || !strings.Contains(s.Title, "release v0.2.0: rc.1 on the canary «buvg01» since 2026-10-27 10:00 UTC, healthy 2 h of 24") {
		t.Fatalf("status after 2h: %+v", fs)
	}
	w.at(21 * time.Hour) // 23 h
	if fixOf(w.observe("github-main"), "release") != nil {
		t.Fatal("final tag before canary_hours")
	}
	w.at(2 * time.Hour) // 25 h, but only 3 ticks
	if fixOf(w.observe("github-main"), "release") != nil {
		t.Fatal("final tag before canary_min_ticks")
	}
	w.fake.With(func(f *githubtest.Fake) {
		f.Comments[fleet][2] = beaconComment("buvg01", "v0.2.0-rc.1", "", f.Now.Add(-25*time.Hour), 8)
	})
	// healthy enough, but main is red: waiting
	w.fake.With(func(f *githubtest.Fake) { f.Runs[0].Conclusion = "failure" })
	fs = w.observe("github-main")
	if fixOf(fs, "release") != nil || findingKey(fs, "release-final|") == nil || !strings.Contains(findingKey(fs, "release-final|").Title, "waits for green main") {
		t.Fatalf("final tag on red main: %+v", fs)
	}
	w.fake.With(func(f *githubtest.Fake) { f.Runs[0].Conclusion = "success" })
	f = fixOf(w.observe("github-main"), "release")
	if f == nil || f.Fix.Params["phase"] != "final" || f.Fix.Params["sha"] != mc || f.Fix.Params["version"] != "v0.2.0" {
		t.Fatalf("final fix: %+v", f)
	}
	r := Act(context.Background(), "release", nil, *f, w.env)
	if !r.OK || !strings.Contains(r.Summary, "tagged v0.2.0") {
		t.Fatalf("final tag: %+v", r)
	}
	if refs := w.refs(); refs["refs/tags/v0.2.0"] != mc || refs["refs/tags/v0.2.0-rc.1"] != mc {
		t.Fatalf("the final tag is not on the rc's commit: %v", refs)
	}
	w.noSecret(r)
}

// A rollback of the rc on the canary: no final tag, the owner told once,
// and the next attempt waits for newly merged PRs (a new rc on a new merge).
func TestCanaryRollback(t *testing.T) {
	w, rel := canaryWorld(t)
	if r := Act(context.Background(), "release", nil, *fixOf(w.observe("github-main"), "release"), w.env); !r.OK {
		t.Fatalf("rc: %+v", r)
	}
	fleet := w.fake.AddIssue("Warden fleet", "", "warden-bot", "OWNER", "warden-fleet")
	w.fake.AddComment(fleet, "warden-bot", beaconComment("buvg01", "v0.1.0", "v0.2.0-rc.1", w.fake.Now, 1))
	w.at(30 * time.Hour)
	fs := w.observe("github-main")
	f := fixOf(fs, "release")
	if f == nil || f.Kind != "rollback" || f.Severity != "high" || f.Fix.Params["phase"] != "failed" || !strings.Contains(f.Title, "release v0.2.0-rc.1 failed on the canary") {
		t.Fatalf("failed finding: %+v", fs)
	}
	if len(fs) != 1 {
		t.Fatalf("only the failure, no release PR without new merges: %+v", fs)
	}
	r := Act(context.Background(), "release", nil, *f, w.env)
	if !r.OK || !strings.Contains(r.Summary, "issue #") {
		t.Fatalf("failed act: %+v", r)
	}
	issues := func() []github.Issue {
		var out []github.Issue
		w.fake.With(func(f *githubtest.Fake) {
			for _, i := range f.Issues {
				if i.Title == "release v0.2.0-rc.1 failed on the canary" {
					out = append(out, *i)
				}
			}
		})
		return out
	}
	is := issues()
	if len(is) != 1 || !strings.Contains(is[0].Body, "@boss") || !is[0].Has("needs-human") {
		t.Fatalf("needs-human issue: %+v", is)
	}
	// once: not again, not even after the owner closed it
	w.fake.With(func(f *githubtest.Fake) { f.Issues[is[0].Number].State = "closed" })
	if r := Act(context.Background(), "release", nil, *f, w.env); !r.OK || len(issues()) != 1 {
		t.Fatalf("told twice: %+v", r)
	}
	if refs := w.refs(); refs["refs/tags/v0.2.0"] != "" {
		t.Fatalf("a final tag after a rollback: %v", refs)
	}
	// a final-tag fix made before the rollback is refused at act time
	final := domain.Finding{Fix: &domain.Fix{Act: "release", Params: map[string]string{"phase": "final", "version": "v0.2.0", "rc": "v0.2.0-rc.1", "sha": "m" + itoa(rel)}}}
	if r := Act(context.Background(), "release", nil, final, w.env); !r.Refused {
		t.Fatalf("final after a rollback: %+v", r)
	}
	// a fix merged: a new release PR (its own key), the failed section folded in
	w.fake.MergedPR("fix: the crash", "fix/crash", w.fake.Now.Add(-time.Minute))
	rel2 := releaseAndMerge(t, w, "v0.2.0-rc.1")
	cl := run(t, w.root, "git", "--git-dir", w.bare, "show", "main:CHANGELOG.md")
	if strings.Count(cl, "## [0.2.0]") != 1 || strings.Count(cl, "[0.2.0]: ") != 1 || !strings.Contains(cl, "add a goodbye") || !strings.Contains(cl, "the crash") {
		t.Fatalf("CHANGELOG of the second attempt:\n%s", cl)
	}
	f = fixOf(w.observe("github-main"), "release")
	if f == nil || f.Fix.Params["phase"] != "rc" || f.Fix.Params["rc"] != "v0.2.0-rc.2" || f.Fix.Params["sha"] != "m"+itoa(rel2) {
		t.Fatalf("rc.2 fix: %+v", f)
	}
}

// An open warden-rollback issue naming the rc stops it too.
func TestCanaryRollbackIssue(t *testing.T) {
	w, _ := canaryWorld(t)
	if r := Act(context.Background(), "release", nil, *fixOf(w.observe("github-main"), "release"), w.env); !r.OK {
		t.Fatalf("rc: %+v", r)
	}
	w.fake.AddIssue("release v0.2.0-rc.10 rolled back on x", "", "warden-bot", "OWNER", "warden-rollback") // another rc
	if f := fixOf(w.observe("github-main"), "release"); f != nil {
		t.Fatalf("rc.10 named rc.1: %+v", f)
	}
	w.fake.AddIssue("release v0.2.0-rc.1 rolled back on buvg01", "", "warden-bot", "OWNER", "warden-rollback")
	f := fixOf(w.observe("github-main"), "release")
	if f == nil || f.Kind != "rollback" || !strings.Contains(f.Title, "warden-rollback issue") {
		t.Fatalf("rollback issue: %+v", f)
	}
}

func mustClient(t *testing.T, w *ghWorld) *github.Client {
	t.Helper()
	c, err := ghClient(w.env)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRCOf(t *testing.T) {
	tag := func(name, sha string) github.Tag { g := github.Tag{Name: name}; g.Commit.SHA = sha; return g }
	tags := []github.Tag{tag("v0.1.0", "a"), tag("v0.2.0-rc.1", "b"), tag("v0.2.0-rc.2", "c"), tag("v0.3.0-rc.7", "c"), tag("v0.2.0-beta.9", "c")}
	for _, tc := range []struct{ v, sha, on, next string }{
		{"v0.2.0", "b", "v0.2.0-rc.1", "v0.2.0-rc.3"},
		{"v0.2.0", "c", "v0.2.0-rc.2", "v0.2.0-rc.3"},
		{"v0.2.0", "d", "", "v0.2.0-rc.3"},
		{"v0.4.0", "c", "", "v0.4.0-rc.1"},
	} {
		if on, next := rcOf(tags, tc.v, tc.sha); on != tc.on || next != tc.next {
			t.Errorf("rcOf(%s, %s) = %q, %q", tc.v, tc.sha, on, next)
		}
	}
	if lt, _ := latestTag(tags); lt.Name != "v0.1.0" {
		t.Errorf("latestTag took an rc: %s", lt.Name)
	}
}

func TestUnrelease(t *testing.T) {
	cl := "# Changelog\n\n## [Unreleased]\n\n## [0.2.0] - 2026-10-27\n\n### Added\n\n- a\n\n## [0.1.0] - 2026-10-01\n\n- first\n\n" +
		"[Unreleased]: https://x/compare/v0.2.0...HEAD\n[0.2.0]: https://x/compare/v0.1.0...v0.2.0\n[0.1.0]: https://x/releases/tag/v0.1.0\n"
	got := unrelease(cl, "v0.2.0")
	if strings.Contains(got, "## [0.2.0]") || strings.Contains(got, "[0.2.0]: ") || !strings.Contains(got, "### Added\n\n- a") || !strings.Contains(got, "## [0.1.0]") {
		t.Fatalf("unrelease:\n%s", got)
	}
	if unrelease(cl, "v0.1.0") != cl {
		t.Fatal("unrelease dropped a section that is not the latest")
	}
	out := Changelog(got, "v0.1.0", "v0.2.0", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), "acme/proj", []github.PR{{Number: 9, Title: "fix: b"}})
	if strings.Count(out, "## [0.2.0] - 2026-10-29") != 1 || !strings.Contains(out, "- a") || !strings.Contains(out, "- b ([#9]") || strings.Count(out, "[0.2.0]: ") != 1 {
		t.Fatalf("Changelog after unrelease:\n%s", out)
	}
}
