package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/builtin"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/policy"
)

func packs(t *testing.T) string {
	p, err := filepath.Abs("../../packs")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// copyPack copies a pack into a temp dir, rewriting its RSS feeds to url (offline).
func copyPack(t *testing.T, name, rss string) string {
	dst := filepath.Join(t.TempDir(), name)
	src := filepath.Join(packs(t), name)
	_ = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		if rel == "domain.toml" {
			b = regexp.MustCompile(`rss\s*=\s*\[[^\]]*\]`).ReplaceAll(b, []byte(`rss = ["`+rss+`"]`))
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	return dst
}

func writeConfig(t *testing.T, dir, body string) *Config {
	p := filepath.Join(dir, "warden.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func open(t *testing.T, cfg *Config) *Instance {
	in, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	return in
}

func gitRepo(t *testing.T, gomod string) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "init"}} {
		if _, err := git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeWeb: OSV and the Go proxy, offline. example.com/dep below 1.4.0 is vulnerable.
func fakeWeb(t *testing.T) *builtin.Web {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Queries []struct {
				Package struct{ Name string } `json:"package"`
				Version string                `json:"version"`
			} `json:"queries"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		var res []map[string]any
		for _, x := range q.Queries {
			if x.Package.Name == "example.com/dep" && semverLess(x.Version, "1.4.0") {
				res = append(res, map[string]any{"vulns": []map[string]string{{"id": "GO-1"}, {"id": "GHSA-a"}}})
			} else {
				res = append(res, map[string]any{})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
	})
	vuln := func(id, fixed, sev string, aliases ...string) map[string]any {
		return map[string]any{"id": id, "summary": "bad thing " + id, "aliases": aliases, "database_specific": map[string]string{"severity": sev},
			"affected": []map[string]any{{"package": map[string]string{"ecosystem": "Go", "name": "example.com/dep"},
				"ranges": []map[string]any{{"type": "SEMVER", "events": []map[string]string{{"introduced": "0"}, {"fixed": fixed}}}}}}}
	}
	vulns := map[string]map[string]any{"GO-1": vuln("GO-1", "1.4.0", "", "GHSA-a"), "GHSA-a": vuln("GHSA-a", "1.4.0", "HIGH", "GO-1")}
	mux.HandleFunc("/v1/vulns/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(vulns[strings.TrimPrefix(r.URL.Path, "/v1/vulns/")])
	})
	mux.HandleFunc("/example.com/dep/@v/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Time": time.Now().Add(-60 * 24 * time.Hour)})
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<rss><channel><item><guid>a</guid><title>Old news</title></item></channel></rss>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &builtin.Web{OSV: srv.URL, Proxy: srv.URL, GitHub: srv.URL, HTTP: srv.Client()}
}

// fakeRun: what go get would do to go.mod; everything else succeeds.
func fakeRun(ran *[]string) func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
	return func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
		*ran = append(*ran, strings.Join(argv, " "))
		if argv[0] == "go" && argv[1] == "get" {
			mod, to, _ := strings.Cut(argv[2], "@")
			b, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			re := regexp.MustCompile(regexp.QuoteMeta(mod) + ` v[0-9.]+`)
			_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(re.ReplaceAllString(string(b), mod+" "+to)), 0o644)
		}
		return "ok", nil
	}
}

// fakeRunnerSandbox: tests that act through fakeRun don't exercise the sandbox
// (TestSandboxedActRuns and TestNoSandboxFailsClosed do). Where no runner
// exists (Linux without a usable bwrap), say sandbox = "none" so the fake runs
// instead of the act being refused; elsewhere the config is unchanged.
func fakeRunnerSandbox() string {
	if _, err := pickRunner("auto"); err != nil {
		return "sandbox = \"none\"\n"
	}
	return ""
}

func goProject(t *testing.T, extra string) (*Config, string) {
	repo := gitRepo(t, "module x\n\ngo 1.25\n\nrequire example.com/dep v1.0.0\n")
	dir := t.TempDir()
	cfg := writeConfig(t, dir, `name = "proj"
domain = "`+filepath.Join(packs(t), "go-project")+`"
params = { root = "`+repo+`", test = "true" }
mode = "act"
rules = [{ act = "go-bump", then = "auto" }]
`+fakeRunnerSandbox()+extra)
	return cfg, repo
}

func TestGoProjectEndToEnd(t *testing.T) {
	cfg, repo := goProject(t, "")
	in := open(t, cfg)
	in.Web = fakeWeb(t)
	var ran []string
	in.Run = fakeRun(&ran)
	t1, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var bumped *Item
	for _, it := range t1.Items {
		if it.Kind == "vuln" {
			bumped = it
		}
	}
	if bumped == nil || bumped.Result == nil || !bumped.Result.OK || bumped.Result.Branch != "warden/bump-example.com-dep-v1.4.0" {
		t.Fatalf("bump: %+v %v %v", bumped, t1.Errors, t1.Notes)
	}
	if out, _ := git(context.Background(), repo, "show", bumped.Result.Branch+":go.mod"); !strings.Contains(out, "example.com/dep v1.4.0") {
		t.Fatal("branch go.mod:", out)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "go.mod")); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("the owner's checkout must stay untouched")
	}
	if st, _ := git(context.Background(), repo, "status", "--porcelain"); st != "" {
		t.Fatal("dirty checkout:", st)
	}
	if _, err := os.Stat(bumped.Result.File); err != nil {
		t.Fatal("PR description:", err)
	}
	if !strings.Contains(strings.Join(ran, "\n"), "sh -c true") {
		t.Fatalf("checks did not run in the worktree: %v", ran)
	}
	rep, _ := os.ReadFile(t1.Report)
	if !strings.Contains(string(rep), "## Done") || !strings.Contains(string(rep), "sandbox: ") {
		t.Fatal(string(rep))
	}

	t2, _ := in.Tick(context.Background())
	if t2.Report != "" || t2.Worth() {
		t.Fatalf("second tick must be quiet: %+v", t2.Items[0])
	}

	in.St.Journal("action.begin", map[string]any{"key": "go-bump:example.com/other@v2.0.0"})
	_ = os.MkdirAll(filepath.Join(cfg.State, "work", "deadbeef"), 0o700)
	t3, _ := in.Tick(context.Background())
	if len(t3.Notes) == 0 || !strings.Contains(t3.Notes[0], "recovered") {
		t.Fatalf("no recovery: %+v", t3.Notes)
	}
	if len(in.St.Unfinished()) != 0 {
		t.Fatal("still unfinished")
	}
	if _, err := os.Stat(filepath.Join(cfg.State, "work", "deadbeef")); err == nil {
		t.Fatal("the dead worktree was left")
	}
}

func TestPreActionHookBlocks(t *testing.T) {
	cfg, repo := goProject(t, `[[hooks]]
event = "PreAction"
matcher = "go-bump"
command = ["sh", "-c", "echo '{\"decision\":\"block\",\"reason\":\"release freeze\"}'"]
`)
	in := open(t, cfg)
	in.Web = fakeWeb(t)
	var ran []string
	in.Run = fakeRun(&ran)
	t1, _ := in.Tick(context.Background())
	var it *Item
	for _, x := range t1.Items {
		if x.Kind == "vuln" {
			it = x
		}
	}
	if it == nil || it.Result == nil || it.Result.OK || !strings.Contains(it.Result.Summary, "release freeze") {
		t.Fatalf("%+v", it)
	}
	if out, _ := git(context.Background(), repo, "branch", "--list", "warden/*"); out != "" {
		t.Fatal("a blocked action made a branch:", out)
	}
	for _, r := range ran {
		if strings.HasPrefix(r, "go get") {
			t.Fatal("a blocked action ran", r)
		}
	}
}

// k8s-namespace with a fake kubectl printing fixtures.
func TestK8sNamespace(t *testing.T) {
	bin := t.TempDir()
	fx := t.TempDir()
	_ = os.WriteFile(filepath.Join(fx, "events.json"), []byte(`{"items":[
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api","involvedObject":{"kind":"Pod","name":"api-7d9f"}},
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api","involvedObject":{"kind":"Pod","name":"api-7d9f"}},
 {"type":"Normal","reason":"Pulled","message":"ok","involvedObject":{"kind":"Pod","name":"web-1"}}]}`), 0o644)
	_ = os.WriteFile(filepath.Join(fx, "pods.json"), []byte(`{"items":[
 {"metadata":{"name":"api-7d9f"},"spec":{"containers":[{"name":"api","image":"ghcr.io/shop/api:1.4.2"}]},
  "status":{"containerStatuses":[{"name":"api","restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s"}}}]}},
 {"metadata":{"name":"web-1"},"spec":{"containers":[{"name":"web","image":"nginx:1.25.3"}]},
  "status":{"containerStatuses":[{"name":"web","restartCount":0,"state":{"running":{}}}]}}]}`), 0o644)
	script := "#!/bin/sh\ncase \"$*\" in\n*events*) cat " + fx + "/events.json;;\n*pods*) cat " + fx + "/pods.json;;\n*\" ns \"*) echo namespace/shop;;\n*) echo unexpected \"$@\" >&2; exit 1;;\nesac\n"
	_ = os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	web := fakeWeb(t)
	pack := copyPack(t, "k8s-namespace", web.OSV+"/feed.xml")
	cfg := writeConfig(t, t.TempDir(), `domain = "`+pack+`"
params = { namespace = "shop", context = "kind-test" }
mode = "act"
rules = [{ act = "propose", then = "auto" }]
`)
	in := open(t, cfg)
	in.Web = web
	t1, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var incidents, proposed int
	for _, it := range t1.Items {
		if it.Kind == "incident" {
			incidents++
			if it.Result != nil && it.Result.OK && it.Result.File != "" {
				proposed++
			}
			if it.Step == "warnings" && it.Count != 2 {
				t.Fatalf("two BackOff warnings are one finding ×2: %+v", it)
			}
		}
	}
	if incidents != 2 || proposed != 2 || t1.Components != 2 {
		t.Fatalf("incidents %d proposed %d components %d errors %v", incidents, proposed, t1.Components, t1.Errors)
	}
	rep, _ := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
	for _, want := range []string{"act apply: report-only — needs the k8s.apply grant", "mcp server kubernetes: refused — not in warden.lock — run `warden add mcp:io.github.containers/kubernetes-mcp-server@0.0.67`", "CrashLoopBackOff"} {
		if !strings.Contains(string(rep), want) {
			t.Fatalf("report lacks %q:\n%s", want, rep)
		}
	}
}

func TestMacOSDryRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS")
	}
	web := fakeWeb(t)
	pack := copyPack(t, "macos", web.OSV+"/feed.xml")
	cfg := writeConfig(t, t.TempDir(), `domain = "`+pack+`"
params = { skip = "caches,library-caches,trash" }
`)
	in := open(t, cfg)
	in.Web = web
	if !in.DryRun || !in.FirstRun {
		t.Fatal("a domain's first run without mode is dry-run")
	}
	t1, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range t1.Items {
		if it.Result != nil {
			t.Fatalf("dry-run acted: %+v", it)
		}
	}
	rep, _ := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
	if !strings.Contains(string(rep), "dry-run") || !strings.Contains(string(rep), "act tool-clean: report-only — needs the mac.clean grant") {
		t.Fatal(string(rep))
	}
}

// a tiny domain whose act writes a file, sandboxed
func touchDomain(t *testing.T, root string) string {
	d := filepath.Join(t.TempDir(), "touch")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "domain.toml"), []byte(`name = "touch"
[[observe]]
name = "flag"
run = { cmd = ["sh", "-c", "echo '{\"key\":\"flag\",\"kind\":\"health\",\"severity\":\"medium\",\"subject\":\"flag\",\"title\":\"flag missing\"}'"] }
fix = "touch"
[[act]]
name = "touch"
run = { cmd = ["sh", "-c", "echo done > `+root+`/touched"] }
floors = { sandbox = true }
[[check]]
name = "touched"
run = { cmd = ["test", "-f", "`+root+`/touched"] }
floors = { sandbox = true }
`), 0o644)
	return d
}

func TestNoSandboxFailsClosed(t *testing.T) {
	old := pickRunner
	pickRunner = func(string) (string, error) { return "", errors.New("box: no sandbox runner available") }
	defer func() { pickRunner = old }()
	root := t.TempDir()
	cfg := writeConfig(t, root, `domain = "`+touchDomain(t, root)+`"
params = { root = "`+root+`" }
mode = "act"
rules = [{ act = "touch", then = "auto" }]
`)
	in := open(t, cfg)
	t1, _ := in.Tick(context.Background())
	if _, err := os.Stat(filepath.Join(root, "touched")); err == nil {
		t.Fatal("acted without a sandbox")
	}
	rep, _ := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
	for _, want := range []string{"sandbox: unavailable", `act touch refused — no sandbox available: set sandbox = "none" to run unsandboxed — your choice`, "check touched refused"} {
		if !strings.Contains(string(rep), want) {
			t.Fatalf("report lacks %q:\n%s", want, rep)
		}
	}
	if len(t1.Items) != 1 {
		t.Fatalf("observing goes on: %+v", t1.Items)
	}
}

func TestExplicitNoneRuns(t *testing.T) {
	root := t.TempDir()
	cfg := writeConfig(t, root, `domain = "`+touchDomain(t, root)+`"
params = { root = "`+root+`" }
mode = "act"
sandbox = "none"
rules = [{ act = "touch", then = "auto" }]
`)
	in := open(t, cfg)
	t1, _ := in.Tick(context.Background())
	if _, err := os.Stat(filepath.Join(root, "touched")); err != nil {
		t.Fatalf("explicit none must run: %+v %v", t1.Items[0].Result, t1.Notes)
	}
	rep, _ := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
	if !strings.Contains(string(rep), "sandbox: NONE (owner's choice)") || !strings.Contains(string(rep), "SANDBOX OFF") {
		t.Fatal(string(rep))
	}
}

func TestSandboxedActRuns(t *testing.T) {
	if _, err := pickRunner("auto"); err != nil {
		t.Skip("no sandbox here")
	}
	root := t.TempDir()
	cfg := writeConfig(t, root, `domain = "`+touchDomain(t, root)+`"
params = { root = "`+root+`" }
mode = "act"
rules = [{ act = "touch", then = "auto" }]
`)
	in := open(t, cfg)
	t1, _ := in.Tick(context.Background())
	if it := t1.Items[0]; it.Result == nil || !it.Result.OK {
		t.Fatalf("%+v %v", it.Result, t1.Notes)
	}
}

func TestStateLockAndConfig(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(dir); err == nil {
		t.Fatal("two ticks at once")
	}
	s.Close()
	cfg := writeConfig(t, t.TempDir(), "name = \"Mac Book\"\ndomain = \"pack:macos\"\nwake = { every = \"12h\" }\n")
	if cfg.DomainDir() != filepath.Join(packs(t), "macos") {
		t.Fatalf("pack:macos → %s", cfg.DomainDir())
	}
	paths, err := Units(cfg, "/usr/local/bin/warden", false)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(paths[0]); !strings.Contains(string(b), "<integer>43200</integer>") || !strings.Contains(string(b), "dev.warden.mac-book") {
		t.Fatal(string(b))
	}
	if lp, _ := Units(cfg, "/usr/local/bin/warden", true); len(lp) != 2 {
		t.Fatal("listen units", lp)
	}
	_ = os.WriteFile(cfg.Path, []byte("domain = \"x\"\nmode = \"yolo\"\n"), 0o644)
	if _, err := Load(cfg.Path); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestTriageFloorBeatsLearnedRules(t *testing.T) {
	tr := Triage{Version: 2, Rules: []TriageRule{{Kind: "vuln", Then: Ignore}, {Kind: "low_disk", Then: Ignore}, {Then: Ignore}}}
	for _, f := range []domain.Finding{{Kind: "vuln", Severity: "high"}, {Kind: "low_disk", Severity: "medium"}, {Kind: "check", Severity: "high"}} {
		if d, _ := tr.Decide(f); d != Report {
			t.Fatalf("%s must be reported whatever the triage says", f.Kind)
		}
	}
	if d, _ := tr.Decide(domain.Finding{Kind: "incident", Severity: "high"}); d == Ignore {
		t.Fatal("a severe incident is never hidden")
	}
	if d, _ := tr.Decide(domain.Finding{Kind: "cache"}); d != Ignore {
		t.Fatal("a cache finding may be ignored")
	}
}

func TestImproveGate(t *testing.T) {
	sk := OpenTriage(t.TempDir())
	cache := domain.Finding{ID: "c1", Step: "caches", Kind: "cache", Severity: "low", Subject: "~/.npm/_cacache"}
	news := domain.Finding{ID: "r1", Step: "releases", Kind: "news", Severity: "medium", Subject: "owner/noisy@v9.1.0"}
	if _, err := sk.Label(cache, "noise"); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Label(news, "noise"); err != nil {
		t.Fatal(err)
	}
	o, err := sk.Gate(Triage{Rules: []TriageRule{{Kind: "vuln", Then: Ignore}, {Then: Ignore}}}, "test")
	if err != nil || o.Adopted || !strings.Contains(o.Why, "invariant") {
		t.Fatalf("hiding vulnerabilities must be rejected: %+v", o)
	}
	if o, _ = sk.Gate(DefaultTriage(), "test"); o.Adopted {
		t.Fatal("an equal candidate must not be adopted")
	}
	o, _ = sk.Gate(ProposeFromLabels(sk.Triage(), sk.Cases()), "labels")
	if !o.Adopted || o.Version != 2 || o.After.OwnerOK != 2 {
		t.Fatalf("%+v", o)
	}
	if d, _ := sk.Triage().Decide(domain.Finding{Step: "releases", Kind: "news", Severity: "medium", Subject: "owner/noisy@v9.2.0"}); d != Ignore {
		t.Fatal("the next release of the noisy repo should be ignored now")
	}
	if v, err := sk.Rollback(); err != nil || v != 1 || sk.Triage().Version != 1 {
		t.Fatal(v, err)
	}
	if _, err := ParseTriage("version = 1\n[[rules]]\nthen = \"report\"\nfloor = \"off\"\n"); err == nil {
		t.Fatal("unknown fields must be refused")
	}
}
