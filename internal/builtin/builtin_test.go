package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/policy"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLockfiles(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "go.mod"), "module example.com/app\n\ngo 1.22\n\nrequire golang.org/x/text v0.3.7\n\nrequire (\n\tgithub.com/a/b v1.2.3 // indirect\n\tgithub.com/c/d/v2 v2.0.0\n)\n\nreplace (\n\tgithub.com/a/b => ../b\n)\n")
	write(t, filepath.Join(d, "requirements.txt"), "# deps\nrequests==2.31.0\nflask>=2\nuvicorn[standard]==0.23.1 ; python_version > '3.8'\n")
	write(t, filepath.Join(d, "package-lock.json"), `{"packages":{"":{"version":"1.0.0"},"node_modules/lodash":{"version":"4.17.20"},"node_modules/a/node_modules/lodash":{"version":"4.17.21"},"node_modules/@s/x":{"version":"1.0.0"}}}`)
	cs, err := Inventory(context.Background(), "lockfiles", map[string]any{"root": d}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cs {
		got[c.Ecosystem+" "+c.Name+"@"+c.Version] = c.Where
	}
	for _, want := range []string{"Go golang.org/x/text@v0.3.7", "Go github.com/a/b@v1.2.3", "Go github.com/c/d/v2@v2.0.0", "PyPI requests@2.31.0",
		"PyPI uvicorn@0.23.1", "npm lodash@4.17.20", "npm lodash@4.17.21", "npm @s/x@1.0.0"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(cs) != 8 {
		t.Errorf("want 8 components, got %d: %v", len(cs), got)
	}
	if _, err := Inventory(context.Background(), "lockfiles", map[string]any{"root": filepath.Join(d, "nope")}, nil); err == nil {
		t.Error("missing root: want an error")
	}
}

func TestHomebrew(t *testing.T) {
	d := t.TempDir()
	for _, p := range []string{"git/2.40.0", "git/2.41.0_1", "openssl@3/3.1.2", ".hidden/1.0"} {
		if err := os.MkdirAll(filepath.Join(d, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := cellarRoots
	cellarRoots = []string{d, filepath.Join(d, "missing")}
	defer func() { cellarRoots = old }()
	cs, err := Inventory(context.Background(), "homebrew", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Name != "git" || cs[0].Version != "2.41.0_1" || cs[1].Name != "openssl@3" || cs[0].Ecosystem != "Homebrew" {
		t.Fatalf("got %+v", cs)
	}
}

// osvServer: querybatch answers from vulns per package name; vulns by id; the proxy's .info.
func osvServer(t *testing.T, byPkg map[string][]string, vulns map[string]string, published time.Time) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/querybatch":
			var req struct {
				Queries []struct {
					Package struct{ Name string } `json:"package"`
					Version string                `json:"version"`
				} `json:"queries"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var res []map[string]any
			for _, q := range req.Queries {
				var vs []map[string]string
				for _, id := range byPkg[q.Package.Name+"@"+q.Version] {
					vs = append(vs, map[string]string{"id": id})
				}
				res = append(res, map[string]any{"vulns": vs})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
		case strings.HasPrefix(r.URL.Path, "/v1/vulns/"):
			v, ok := vulns[strings.TrimPrefix(r.URL.Path, "/v1/vulns/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, v)
		case strings.HasSuffix(r.URL.Path, ".info"):
			if published.IsZero() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"Version":"x","Time":%q}`, published.Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
}

func vuln(id, sev, fixed string, aliases ...string) string {
	b, _ := json.Marshal(map[string]any{"id": id, "summary": "bug " + id, "aliases": aliases, "database_specific": map[string]string{"severity": sev},
		"affected": []any{map[string]any{"package": map[string]string{"ecosystem": "Go", "name": "golang.org/x/text"},
			"ranges": []any{map[string]any{"type": "SEMVER", "events": []any{map[string]string{"introduced": "0"}, map[string]string{"fixed": fixed}}}}}}})
	return string(b)
}

func TestAdvisories(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	// GHSA-a comes first and does not name GO-1; GO-1 names GHSA-a: one advisory, graded HIGH by the GHSA.
	srv := osvServer(t, map[string][]string{"golang.org/x/text@0.3.7": {"GHSA-a", "GO-1", "GO-2"}},
		map[string]string{"GHSA-a": vuln("GHSA-a", "HIGH", "0.3.8"), "GO-1": vuln("GO-1", "", "0.3.8", "GHSA-a", "CVE-1"), "GO-2": vuln("GO-2", "", "0.3.10")},
		now.Add(-3*24*time.Hour))
	defer srv.Close()
	env := &Env{Web: &Web{OSV: srv.URL, Proxy: srv.URL, HTTP: srv.Client()}, Now: func() time.Time { return now }, Params: map[string]string{}}
	comps := []domain.Component{{Name: "golang.org/x/text", Version: "v0.3.7", Ecosystem: "Go"}, {Name: "git", Version: "2.41.0", Ecosystem: "Homebrew"},
		{Name: "golang.org/x/net", Version: "v0.1.0", Ecosystem: "Go"}}
	fs, err := Advisories(context.Background(), comps, map[string]string{"Go": "go-bump"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %+v", fs)
	}
	f := fs[0]
	if f.Severity != "high" || !strings.Contains(f.Title, ": 2 known vulnerabilities") || f.Kind != "vuln" || f.ID == "" {
		t.Errorf("finding %+v", f)
	}
	if f.Fix == nil || f.Fix.Act != "go-bump" || f.Fix.Target != "golang.org/x/text" {
		t.Fatalf("fix %+v", f.Fix)
	}
	p := f.Fix.Params
	if p["to"] != "v0.3.10" || p["from"] != "v0.3.7" || p["advisories"] != "GHSA-a,GO-2" || p["published"] != "2026-10-24" || p["fresh"] != "true" || p["ecosystem"] != "Go" {
		t.Errorf("params %v", p)
	}
	env.Params["min_age_days"] = "2"
	fs, _ = Advisories(context.Background(), comps, map[string]string{"Go": "go-bump"}, env)
	if fs[0].Fix.Params["fresh"] != "false" {
		t.Errorf("3 days old with min 2: want fresh=false, got %v", fs[0].Fix.Params)
	}
	fs, _ = Advisories(context.Background(), comps, nil, env) // no act for Go: report only
	if fs[0].Fix != nil || fs[0].Data["to"] != "v0.3.10" {
		t.Errorf("no act: %+v", fs[0])
	}
}

func TestReleasesAndFeed(t *testing.T) {
	tag := "v2.0.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases.atom":
			fmt.Fprintf(w, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>tag:github.com,2008:Repository/1/%s</id><title>%s</title><link rel="alternate" href="https://github.com/o/r/releases/tag/%s"/><content type="html">&lt;p&gt;Fixes things&lt;/p&gt;</content></entry></feed>`, tag, tag, tag)
		case "/rss":
			fmt.Fprint(w, `<rss version="2.0"><channel><item><title>A</title><link>https://x/a</link><guid>g1</guid><description>&lt;b&gt;bold&lt;/b&gt; text</description></item><item><title>B</title><link>https://x/b</link></item></channel></rss>`)
		case "/atom":
			fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>e1</id><title>E</title><link href="https://x/e"/><summary>sum</summary></entry></feed>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := &Env{Web: &Web{GitHub: srv.URL, HTTP: srv.Client()}}
	comps := []domain.Component{{Name: "github.com/o/r/v2", Version: "v1.9.0", Ecosystem: "Go"}}
	last := map[string]string{}
	fs, errs := Releases(context.Background(), []string{"o/r", "o/missing"}, last, comps, env)
	if len(fs) != 0 || len(errs) != 1 || last["o/r"] != "v2.0.0" {
		t.Fatalf("baseline: %v %v %v", fs, errs, last)
	}
	last["o/r"] = "v1.9.0"
	fs, _ = Releases(context.Background(), []string{"o/r"}, last, comps, env)
	if len(fs) != 1 || fs[0].Severity != "medium" || fs[0].Kind != "news" || !strings.Contains(fs[0].Detail, "Fixes things") || fs[0].Links[0] == "" {
		t.Fatalf("release: %+v", fs)
	}
	last["o/r"] = "v1.9.0"
	fs, _ = Releases(context.Background(), []string{"o/r"}, last, nil, env)
	if fs[0].Severity != "info" {
		t.Errorf("not in the inventory: want info, got %s", fs[0].Severity)
	}

	items, err := Feed(context.Background(), srv.URL+"/rss", env)
	if err != nil || len(items) != 2 || items[0].ID != "g1" || items[0].Text != "bold text" || items[1].ID != "https://x/b" {
		t.Fatalf("rss: %+v %v", items, err)
	}
	items, err = Feed(context.Background(), srv.URL+"/atom", env)
	if err != nil || len(items) != 1 || items[0].ID != "e1" || items[0].Link != "https://x/e" || items[0].Text != "sum" {
		t.Fatalf("atom: %+v %v", items, err)
	}
}

// ---- go-bump

type fakeBrain func(ctx context.Context, tools []brain.Tool) (string, error)

func (f fakeBrain) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	return f(ctx, tools)
}

func call(t *testing.T, tools []brain.Tool, name string, args map[string]any) (string, bool) {
	for _, tl := range tools {
		if tl.Name == name {
			return tl.Run(context.Background(), args)
		}
	}
	t.Fatalf("no tool %s", name)
	return "", false
}

func gitRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	ctx := context.Background()
	if _, err := git(ctx, d, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(d, "go.mod"), "module example.com/app\n\ngo 1.22\n\nrequire golang.org/x/text v0.3.7\n")
	write(t, filepath.Join(d, "main.go"), "package main\n\nfunc main() {}\n")
	_, _ = git(ctx, d, "add", "-A")
	if _, err := git(ctx, d, "commit", "-q", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	return d
}

func bumpEnv(t *testing.T, checksOK func(dir string) bool) (*Env, *[]string) {
	srv := osvServer(t, nil, nil, time.Time{})
	t.Cleanup(srv.Close)
	var ran []string
	return &Env{
		State: t.TempDir(), Web: &Web{OSV: srv.URL, Proxy: srv.URL, HTTP: srv.Client()},
		Run: func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
			ran = append(ran, strings.Join(argv, " ")+fmt.Sprintf(" sandbox=%v net=%v", f.Sandbox, f.Net))
			if argv[1] == "get" {
				mod, ver, _ := strings.Cut(argv[2], "@")
				b, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
				s := strings.Replace(string(b), mod+" v0.3.7", mod+" "+ver, 1)
				return "", os.WriteFile(filepath.Join(dir, "go.mod"), []byte(s), 0o644)
			}
			return "", nil
		},
		Checks: func(ctx context.Context, dir string) (string, error) {
			if checksOK(dir) {
				return "ok", nil
			}
			return "FAIL TestX", errors.New("exit 1")
		},
	}, &ran
}

func bumpFinding() domain.Finding {
	return domain.Finding{ID: "f1", Title: "golang.org/x/text v0.3.7: 1 known vulnerability", Detail: "GO-1: bug", Links: []string{"https://osv.dev/vulnerability/GO-1"},
		Fix: &domain.Fix{Act: "go-bump", Target: "golang.org/x/text", Params: map[string]string{"ecosystem": "Go", "module": "golang.org/x/text", "from": "v0.3.7", "to": "v0.3.8", "advisories": "GO-1"}}}
}

func TestGoBump(t *testing.T) {
	root := gitRepo(t)
	env, ran := bumpEnv(t, func(string) bool { return true })
	ctx := context.Background()
	res := Act(ctx, "go-bump", map[string]any{"root": root}, bumpFinding(), env)
	if !res.OK || res.Branch != "warden/bump-golang.org-x-text-v0.3.8" {
		t.Fatalf("bump: %+v", res)
	}
	if len(*ran) != 3 || !strings.Contains((*ran)[0], "go get golang.org/x/text@v0.3.8 sandbox=true net=[proxy.golang.org sum.golang.org]") ||
		!strings.Contains((*ran)[2], "go test -count=1 -run=^$ ./... sandbox=true net=[]") {
		t.Errorf("ran %v", *ran)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "go.mod")); !strings.Contains(string(b), "v0.3.7") {
		t.Error("the owner's checkout was touched")
	}
	show, err := git(ctx, root, "show", res.Branch+":go.mod")
	if err != nil || !strings.Contains(show, "golang.org/x/text v0.3.8") {
		t.Fatalf("branch go.mod: %v %s", err, show)
	}
	if author, _ := git(ctx, root, "log", "-1", "--format=%an <%ae>", res.Branch); author != "Warden <warden@localhost>" {
		t.Errorf("author %q", author)
	}
	if b, err := os.ReadFile(res.File); err != nil || !strings.Contains(string(b), "deps: bump golang.org/x/text v0.3.7 → v0.3.8") ||
		res.File != filepath.Join(env.State, "prs", "warden_bump-golang.org-x-text-v0.3.8.md") {
		t.Errorf("PR file %s: %v", res.File, err)
	}
	if ents, _ := os.ReadDir(filepath.Join(env.State, "work")); len(ents) != 0 {
		t.Errorf("worktree left behind: %v", ents)
	}
	if wl, _ := git(ctx, root, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("worktrees: %s", wl)
	}
	again := Act(ctx, "go-bump", map[string]any{"root": root}, bumpFinding(), env)
	if !again.OK || !strings.HasPrefix(again.Summary, "already") {
		t.Errorf("idempotent: %+v", again)
	}
}

func TestGoBumpRed(t *testing.T) {
	root := gitRepo(t)
	ctx := context.Background()
	env, _ := bumpEnv(t, func(string) bool { return false })
	res := Act(ctx, "go-bump", map[string]any{"root": root}, bumpFinding(), env)
	if res.OK || res.Branch != "" || !strings.Contains(strings.Join(res.Log, "\n"), "FAIL TestX") {
		t.Fatalf("red: %+v", res)
	}
	if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/warden/bump-golang.org-x-text-v0.3.8"); err == nil {
		t.Error("the branch was kept")
	}

	// red, then the brain fixes it inside the worktree (and cannot get out)
	env, _ = bumpEnv(t, func(dir string) bool { _, err := os.Stat(filepath.Join(dir, "fixed.go")); return err == nil })
	env.Brain = fakeBrain(func(ctx context.Context, tools []brain.Tool) (string, error) {
		if out, ok := call(t, tools, "read_file", map[string]any{"path": "../../../etc/passwd"}); ok {
			t.Errorf("escaped: %s", out)
		}
		if _, ok := call(t, tools, "write_file", map[string]any{"path": ".git/config", "content": "x"}); ok {
			t.Error("wrote .git")
		}
		if _, ok := call(t, tools, "run_checks", nil); ok {
			t.Error("checks should be red before the fix")
		}
		if out, ok := call(t, tools, "write_file", map[string]any{"path": "fixed.go", "content": "package main\n"}); !ok {
			t.Errorf("write: %s", out)
		}
		if out, ok := call(t, tools, "replace_in_file", map[string]any{"path": "main.go", "old": "func main() {}", "new": "func main() { _ = 1 }"}); !ok {
			t.Errorf("replace: %s", out)
		}
		if out, _ := call(t, tools, "list_files", nil); !strings.Contains(out, "fixed.go") || strings.Contains(out, ".git") {
			t.Errorf("list: %s", out)
		}
		return "added fixed.go", nil
	})
	res = Act(ctx, "go-bump", map[string]any{"root": root}, bumpFinding(), env)
	if !res.OK {
		t.Fatalf("brain fix: %+v", res)
	}
	if show, err := git(ctx, root, "show", res.Branch+":fixed.go"); err != nil || show != "package main" {
		t.Errorf("fixed.go on the branch: %v %q", err, show)
	}
}

// A fixed version that changed its API breaks the build, not only the
// tests: that too goes to the brain, in the worktree it was given.
func TestGoBumpBuildBreakGoesToBrain(t *testing.T) {
	root := gitRepo(t)
	env, ran := bumpEnv(t, func(string) bool { return true })
	run := env.Run
	env.Run = func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
		if argv[1] == "test" {
			if _, err := os.Stat(filepath.Join(dir, "fixed.go")); err != nil {
				return "./main.go:3: undefined: quic.Connection", errors.New("exit 1")
			}
		}
		return run(ctx, dir, argv, f)
	}
	var gotDir string
	env.Brain = fakeBrain(func(ctx context.Context, tools []brain.Tool) (string, error) {
		gotDir = brain.DirFrom(ctx)
		if out, ok := call(t, tools, "write_file", map[string]any{"path": "fixed.go", "content": "package main\n"}); !ok {
			t.Errorf("write: %s", out)
		}
		return "ported to the new API", nil
	})
	res := Act(context.Background(), "go-bump", map[string]any{"root": root}, bumpFinding(), env)
	if !res.OK || !strings.HasPrefix(gotDir, filepath.Join(env.State, "work")+"/") || !strings.Contains(strings.Join(res.Log, "\n"), "build or checks red") {
		t.Fatalf("%+v (ran %v)", res, *ran)
	}
}

func TestGoBumpRefusesBadParams(t *testing.T) {
	env := &Env{State: t.TempDir()}
	f := bumpFinding()
	f.Fix.Params["to"] = "--upgrade"
	if res := Act(context.Background(), "go-bump", nil, f, env); res.OK || !strings.Contains(res.Summary, "refused") {
		t.Errorf("%+v", res)
	}
}

// ---- tool-clean

func TestToolClean(t *testing.T) {
	oldC, oldR := cleaners, runClean
	defer func() { cleaners, runClean = oldC, oldR }()
	cleaners = func() []Cleaner {
		return []Cleaner{{ID: "fake", Argv: []string{"fake-tool", "clean"}}, {ID: "report", Title: "report only"}}
	}
	var ran []string
	runClean = func(ctx context.Context, argv []string) (string, error) {
		ran = append(ran, strings.Join(argv, " "))
		return "done", nil
	}
	fd := func(id, argv string) domain.Finding {
		return domain.Finding{Fix: &domain.Fix{Act: "tool-clean", Target: id, Params: map[string]string{"cleaner": id, "argv": argv}}}
	}
	ctx := context.Background()
	for _, c := range []struct {
		allow string
		f     domain.Finding
	}{
		{"", fd("fake", "fake-tool clean")},
		{"other", fd("fake", "fake-tool clean")},
		{"fake", fd("fake", "rm -rf /")},
		{"report", fd("report", "")},
		{"ghost", fd("ghost", "x")},
	} {
		if res := Act(ctx, "tool-clean", map[string]any{"allow": c.allow}, c.f, &Env{}); res.OK || !strings.HasPrefix(res.Summary, "refused") {
			t.Errorf("allow=%q %v: %+v", c.allow, c.f.Fix.Params, res)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("ran %v", ran)
	}
	res := Act(ctx, "tool-clean", map[string]any{"allow": "npm, fake"}, fd("fake", "fake-tool clean"), &Env{})
	if !res.OK || len(ran) != 1 || ran[0] != "fake-tool clean" {
		t.Errorf("%+v %v", res, ran)
	}
}

// ---- propose

func TestPropose(t *testing.T) {
	env := &Env{State: t.TempDir(), Skills: "- k8s-triage: triage", Brain: fakeBrain(func(ctx context.Context, tools []brain.Tool) (string, error) {
		if len(tools) != 1 || tools[0].Name != "pods_get" {
			t.Errorf("tools %v", tools)
		}
		return "Root cause: OOM. Patch: raise the limit.", nil
	}), Tools: []brain.Tool{{Name: "pods_get"}}}
	f := domain.Finding{ID: "abc123", Kind: "incident", Severity: "high", Subject: "web", Title: "CrashLoopBackOff: web", Detail: "back-off", Data: map[string]string{"restarts": "7"}}
	res := Act(context.Background(), "propose", nil, f, env)
	if !res.OK || res.File != filepath.Join(env.State, "proposals", "abc123.md") {
		t.Fatalf("%+v", res)
	}
	b, _ := os.ReadFile(res.File)
	for _, want := range []string{"# CrashLoopBackOff: web", "back-off", "- restarts: 7", "Root cause: OOM"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("proposal lacks %q:\n%s", want, b)
		}
	}
	if res := Act(context.Background(), "propose", nil, f, &Env{State: t.TempDir()}); !res.OK {
		t.Errorf("without a brain: %+v", res)
	}
}

// ---- machine observers (hooks only: nothing real is measured)

func TestDiskAndCaches(t *testing.T) {
	oldC, oldD, oldF := cleaners, du, diskFree
	defer func() { cleaners, du, diskFree = oldC, oldD, oldF }()
	cleaners = func() []Cleaner {
		return []Cleaner{{ID: "big", Title: "Big cache", Path: func() string { return "/big" }, Argv: []string{"big", "clean"}},
			{ID: "small", Title: "Small", Path: func() string { return "/small" }, Argv: []string{"s"}},
			{ID: "manual", Title: "Manual", Path: func() string { return "/manual" }}}
	}
	du = func(ctx context.Context, p string) int64 {
		return map[string]int64{"/big": 3 << 30, "/small": 1 << 20, "/manual": 1 << 30}[p]
	}
	diskFree = func(string) (int64, int64, error) { return 10 << 30, 500 << 30, nil }
	ctx := context.Background()
	fs, err := Observe(ctx, "disk", map[string]any{"min_free_gb": "25"}, nil)
	if err != nil || len(fs) != 1 || fs[0].Kind != "low_disk" || fs[0].Severity != "high" || !strings.Contains(fs[0].Detail, "Big cache 3.0 GB") {
		t.Fatalf("disk: %+v %v", fs, err)
	}
	if fs, _ := Observe(ctx, "disk", map[string]any{"min_free_gb": float64(15)}, nil); len(fs) != 1 || fs[0].Severity != "medium" {
		t.Errorf("medium: %+v", fs)
	}
	if fs, _ := Observe(ctx, "disk", map[string]any{"min_free_gb": "5"}, nil); len(fs) != 0 {
		t.Errorf("enough space: %+v", fs)
	}
	fs, _ = Observe(ctx, "caches", nil, nil)
	if len(fs) != 2 || fs[0].Fix == nil || fs[0].Fix.Act != "tool-clean" || fs[0].Fix.Target != "big" || fs[0].Fix.Params["argv"] != "big clean" || fs[1].Fix != nil {
		t.Fatalf("caches: %+v", fs)
	}
	if _, err := Observe(ctx, "nope", nil, nil); err == nil {
		t.Error("unknown observer: want an error")
	}
}

func TestHealth(t *testing.T) {
	old := cmdOut
	defer func() { cmdOut = old }()
	cmdOut = func(ctx context.Context, name string, args ...string) string {
		if name == "system_profiler" {
			return "Cycle Count: 812\nCondition: Service Recommended\nMaximum Capacity: 79%"
		}
		return "CPU_Speed_Limit = 70"
	}
	fs, _ := Observe(context.Background(), "health", nil, nil)
	if len(fs) != 2 || fs[0].Severity != "medium" || fs[0].Title != "Battery: Service Recommended, 812 cycles, max capacity 79%" || !strings.Contains(fs[1].Title, "70%") {
		t.Fatalf("%+v", fs)
	}
}
