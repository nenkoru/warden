// Package builtin holds the steps Warden runs as its own Go code, kept to what
// must be fast or safe: observers of a Mac, inventories, OSV advisories,
// GitHub releases, feeds, and the acts go-bump, tool-clean and propose.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/policy"
	"github.com/nenkoru/warden/internal/version"
)

// Web: the outside world, base URLs swappable for tests.
type Web struct {
	OSV, Proxy, GitHub string
	NPM, PyPI          string // registries, for when a fix version was published ("": unknown)
	HTTP               *http.Client
}

func DefaultWeb() *Web {
	return &Web{OSV: "https://api.osv.dev", Proxy: "https://proxy.golang.org", GitHub: "https://github.com",
		NPM: "https://registry.npmjs.org", PyPI: "https://pypi.org", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (w *Web) do(ctx context.Context, method, url string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := w.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if s, ok := out.(*[]byte); ok {
		*s = b
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

// Env: what a builtin may use; core supplies it.
type Env struct {
	Params map[string]string
	State  string // warden state dir
	Web    *Web
	Run    func(ctx context.Context, dir string, argv []string, f policy.Floors) (string, error)
	Checks func(ctx context.Context, dir string) (string, error) // nil = none
	Brain  brain.Brain                                           // may be nil
	Tools  []brain.Tool
	Skills string
	Now    func() time.Time
	Log    func(string)
}

func (e *Env) web() *Web {
	if e == nil || e.Web == nil {
		return DefaultWeb()
	}
	return e.Web
}

func (e *Env) now() time.Time {
	if e == nil || e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

func (e *Env) log(s string) {
	if e != nil && e.Log != nil {
		e.Log(s)
	}
}

// param: a step's `with` value, else the instance param, else def.
func param(with map[string]any, env *Env, key, def string) string {
	if v, ok := with[key]; ok && v != nil {
		switch x := v.(type) {
		case string:
			if x != "" {
				return x
			}
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		case int64:
			return strconv.FormatInt(x, 10)
		case int:
			return strconv.Itoa(x)
		default:
			return fmt.Sprint(x)
		}
	}
	if env != nil && env.Params[key] != "" {
		return env.Params[key]
	}
	return def
}

func finding(step, key, kind, sev, subject, title, detail string) domain.Finding {
	return domain.Finding{ID: domain.ID(step, key), Key: key, Step: step, Kind: kind, Severity: sev, Subject: subject, Title: title, Detail: detail}
}

// git runs locally (it only touches the repository), never through hooks
// Warden did not write, never with the owner's signing or credential helpers.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "credential.helper=", "-c", "user.name=Warden", "-c", "user.email=warden@localhost"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Warden", "GIT_AUTHOR_EMAIL=warden@localhost",
		"GIT_COMMITTER_NAME=Warden", "GIT_COMMITTER_EMAIL=warden@localhost", "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(tail(out.String(), 400)))
	}
	return strings.TrimSpace(out.String()), nil
}

// writeFile: atomic (temp + rename), parents created.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- small helpers

func semverLess(a, b string) bool { return semverCmp(a, b) < 0 }

func semverCmp(a, b string) int {
	pa, qa := splitVer(a)
	pb, qb := splitVer(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case qa == qb:
		return 0
	case qa == "": // a release is newer than its pre-release
		return 1
	case qb == "":
		return -1
	case qa < qb:
		return -1
	}
	return 1
}

func splitVer(v string) ([3]int, string) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, _, _ = strings.Cut(v, "_") // Homebrew revision
	v, pre, _ := strings.Cut(v, "-")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out, pre
}

func vprefix(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

func gb(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&#39;", "'").Replace(b.String())
}

func firstLines(s string, n int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
