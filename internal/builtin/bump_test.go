package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/policy"
)

func repoWith(t *testing.T, files map[string]string) string {
	d := gitRepo(t)
	for name, body := range files {
		write(t, filepath.Join(d, name), body)
	}
	ctx := context.Background()
	_, _ = git(ctx, d, "add", "-A")
	if _, err := git(ctx, d, "commit", "-q", "-m", "deps"); err != nil {
		t.Fatal(err)
	}
	return d
}

func pkgFinding(eco, name, from, to string) domain.Finding {
	return domain.Finding{ID: "f", Title: name + " " + from + ": 1 known vulnerability", Detail: "GHSA-x: bad",
		Fix: &domain.Fix{Act: map[string]string{"npm": "npm-bump", "PyPI": "pip-bump"}[eco], Target: name,
			Params: map[string]string{"ecosystem": eco, "module": name, "from": from, "to": to, "advisories": "GHSA-x"}}}
}

const lock = `{"name":"app","lockfileVersion":3,"packages":{"":{"name":"app"},"node_modules/a":{"version":"1.0.0"},"node_modules/b":{"version":"1.0.0"},"node_modules/a/node_modules/b":{"version":"1.0.1"}}}`

// fakeNpm: what npm would do to package-lock.json (and package.json).
func fakeNpm(ran *[]string) func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
	return func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
		*ran = append(*ran, strings.Join(argv, " ")+fmt.Sprintf(" sandbox=%v net=%v", f.Sandbox, f.Net))
		lp := filepath.Join(dir, "package-lock.json")
		b, _ := os.ReadFile(lp)
		s := string(b)
		switch argv[1] {
		case "pkg": // npm pkg set overrides[b]=1.2.0
			m := regexp.MustCompile(`^overrides\[(.+)\]=(.+)$`).FindStringSubmatch(argv[3])
			pj, _ := os.ReadFile(filepath.Join(dir, "package.json"))
			_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(strings.TrimSuffix(strings.TrimSpace(string(pj)), "}")+`,"overrides":{"`+m[1]+`":"`+m[2]+`"}}`), 0o644)
		case "install":
			if at := argv[len(argv)-1]; strings.Contains(at, "@") && !strings.HasPrefix(at, "-") { // a direct dependency
				name, ver, _ := strings.Cut(at, "@")
				s = strings.Replace(s, `"node_modules/`+name+`":{"version":"1.0.0"}`, `"node_modules/`+name+`":{"version":"`+ver+`"}`, 1)
			} else if pj, _ := os.ReadFile(filepath.Join(dir, "package.json")); strings.Contains(string(pj), `"overrides":{"b":"1.2.0"}`) {
				s = regexp.MustCompile(`("node_modules/(?:a/node_modules/)?b":\{"version":")[0-9.]+`).ReplaceAllString(s, "${1}1.2.0")
			}
			_ = os.WriteFile(lp, []byte(s), 0o644)
		}
		return "ok", nil
	}
}

func TestNpmBump(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, pkg, to string
		want          []string // in what ran
	}{
		{"direct", "a", "1.1.0", []string{"npm install --ignore-scripts --no-audit --no-fund --cache"}},
		{"transitive", "b", "1.2.0", []string{"npm pkg set overrides[b]=1.2.0 sandbox=true net=[]", "npm install --ignore-scripts"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := repoWith(t, map[string]string{"package.json": `{"name":"app","dependencies":{"a":"^1.0.0"}}`, "package-lock.json": lock})
			env, _ := bumpEnv(t, func(string) bool { return true })
			var ran []string
			env.Run = fakeNpm(&ran)
			res := Act(ctx, "npm-bump", map[string]any{"root": root}, pkgFinding("npm", c.pkg, "1.0.0", c.to), env)
			if !res.OK || res.Branch != "warden/bump-"+c.pkg+"-"+c.to {
				t.Fatalf("%+v\nran %v", res, ran)
			}
			all := strings.Join(ran, "\n")
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Errorf("ran %v, want %q", ran, w)
				}
			}
			if !strings.Contains(all, "net=[registry.npmjs.org]") || strings.Contains(all, "go ") {
				t.Errorf("ran %v", ran)
			}
			show, _ := git(ctx, root, "show", res.Branch+":package-lock.json")
			if strings.Contains(show, `"version":"1.0.1"`) && c.pkg == "b" || !strings.Contains(show, c.to) {
				t.Errorf("lock on the branch: %s", show)
			}
			if b, _ := os.ReadFile(filepath.Join(root, "package-lock.json")); string(b) != lock {
				t.Error("the owner's checkout was touched")
			}
		})
	}
	// an override that leaves an old copy behind does not pass
	root := repoWith(t, map[string]string{"package.json": `{"name":"app"}`, "package-lock.json": lock})
	env, _ := bumpEnv(t, func(string) bool { return true })
	env.Run = func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
		return "ok", nil
	}
	if res := Act(ctx, "npm-bump", map[string]any{"root": root}, pkgFinding("npm", "b", "1.0.0", "1.2.0"), env); res.OK || !strings.Contains(res.Summary, "verify") {
		t.Errorf("%+v", res)
	}
	if res := Act(ctx, "npm-bump", map[string]any{"root": root}, pkgFinding("npm", "--registry=evil", "1.0.0", "1.2.0"), env); res.OK || !strings.Contains(res.Summary, "refused") {
		t.Errorf("%+v", res)
	}
}

func TestPipBump(t *testing.T) {
	ctx := context.Background()
	reqs := "# app\nflask==3.0.0\nRequests[socks]==2.19.0 ; python_version >= \"3.8\"\n"
	root := repoWith(t, map[string]string{"requirements.txt": reqs, ".gitignore": ""})
	env, _ := bumpEnv(t, func(dir string) bool {
		b, _ := os.ReadFile(filepath.Join(dir, "requirements.txt"))
		return strings.Contains(string(b), "==2.32.4")
	})
	var ran []string
	env.Run = func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error) {
		ran = append(ran, strings.Join(argv, " ")+fmt.Sprintf(" net=%v", f.Net))
		if argv[0] == "python3" {
			_ = os.MkdirAll(filepath.Join(dir, ".warden-venv", "bin"), 0o755)
		}
		return "ok", nil
	}
	res := Act(ctx, "pip-bump", map[string]any{"root": root}, pkgFinding("PyPI", "requests", "2.19.0", "2.32.4"), env)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	show, _ := git(ctx, root, "show", res.Branch+":requirements.txt")
	if !strings.Contains(show, "Requests[socks]==2.32.4 ; python_version >= \"3.8\"") || !strings.Contains(show, "flask==3.0.0") {
		t.Errorf("requirements on the branch:\n%s", show)
	}
	if ls, _ := git(ctx, root, "ls-tree", "-r", "--name-only", res.Branch); strings.Contains(ls, ".warden-venv") {
		t.Errorf("the venv was committed: %s", ls)
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "python3 -m venv .warden-venv net=[]") ||
		!strings.Contains(ran[1], "pip install -q --no-cache-dir --disable-pip-version-check -r requirements.txt net=[pypi.org files.pythonhosted.org]") {
		t.Errorf("ran %v", ran)
	}
	// a pin that is not there
	if res := Act(ctx, "pip-bump", map[string]any{"root": root}, pkgFinding("PyPI", "django", "4.0", "4.2.20"), env); res.OK || !strings.Contains(res.Summary, "no django== pin") {
		t.Errorf("%+v", res)
	}
	// red after the bump and no brain: dropped
	env.Checks = func(context.Context, string) (string, error) { return "FAILED test_app.py", errors.New("exit 1") }
	if res := Act(ctx, "pip-bump", map[string]any{"root": root}, pkgFinding("PyPI", "flask", "3.0.0", "3.1.1"), env); res.OK || res.Branch != "" {
		t.Errorf("%+v", res)
	}
}

func TestRegistryFreshness(t *testing.T) {
	old, young := time.Now().Add(-60*24*time.Hour), time.Now().Add(-2*24*time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@scope%2Fpkg", "/@scope/pkg":
			_ = json.NewEncoder(w).Encode(map[string]any{"time": map[string]time.Time{"1.0.0": old, "1.1.0": young}})
		case "/pypi/requests/2.32.4/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"urls": []map[string]time.Time{{"upload_time_iso_8601": old.Add(time.Hour)}, {"upload_time_iso_8601": old}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := &Env{Web: &Web{NPM: srv.URL, PyPI: srv.URL, HTTP: srv.Client()}}
	ctx := context.Background()
	for _, c := range []struct{ eco, name, ver, pub, fresh string }{
		{"npm", "@scope/pkg", "1.0.0", old.UTC().Format("2006-01-02"), "false"},
		{"npm", "@scope/pkg", "1.1.0", young.UTC().Format("2006-01-02"), "true"},
		{"PyPI", "requests", "2.32.4", old.UTC().Format("2006-01-02"), "false"},
		{"PyPI", "requests", "9.9.9", "unknown", "true"},
	} {
		if pub, fresh := env.freshness(ctx, c.eco, c.name, c.ver); pub != c.pub || fresh != c.fresh {
			t.Errorf("%s %s@%s: %s %s, want %s %s", c.eco, c.name, c.ver, pub, fresh, c.pub, c.fresh)
		}
	}
}
