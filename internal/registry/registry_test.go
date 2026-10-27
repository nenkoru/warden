package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	ok := map[string]Ref{
		"mcp:io.github.containers/kubernetes-mcp-server@0.0.67": {Kind: "mcp", Name: "io.github.containers/kubernetes-mcp-server", Version: "0.0.67"},
		"mcp:io.github.x/y": {Kind: "mcp", Name: "io.github.x/y"},
		"skill:github.com/anthropics/skills//skills/pdf@v1":                      {Kind: "skill", Source: "github.com/anthropics/skills", Name: "skills/pdf", Version: "v1"},
		"skill:file:///tmp/r//a/b":                                               {Kind: "skill", Source: "file:///tmp/r", Name: "a/b"},
		"skill:github.com/o/r@abc":                                               {Kind: "skill", Source: "github.com/o/r", Name: ".", Version: "abc"},
		"plugin:security-guidance@github.com/anthropics/claude-plugins-official": {Kind: "plugin", Name: "security-guidance", Source: "github.com/anthropics/claude-plugins-official"},
		"plugin:p@github.com/o/m@v2":                                             {Kind: "plugin", Name: "p", Source: "github.com/o/m", Version: "v2"},
		"pi:pi-mcp-adapter@5.3.1":                                                {Kind: "pi", Name: "pi-mcp-adapter", Version: "5.3.1"},
		"pi:@scope/pkg@1.0.0":                                                    {Kind: "pi", Name: "@scope/pkg", Version: "1.0.0"},
		"pi:git:github.com/o/r@v1":                                               {Kind: "pi", Name: "github.com/o/r", Source: "github.com/o/r", Version: "v1"},
	}
	for s, want := range ok {
		got, err := Parse(s)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", s, got, err, want)
		}
		if back, _ := Parse(got.String()); back != got {
			t.Errorf("String round trip %q -> %q", s, got.String())
		}
	}
	for _, s := range []string{"", "mcp:", "foo:x", "plugin:x", "skill:github.com/o/r//../x", "pi:BAD NAME", "skill:-oops//x"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

type tf struct {
	name, data string
	mode       int64
}

func tgz(t *testing.T, files ...tf) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if f.mode == 0 {
			f.mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sri(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

const digest = "sha256:e47f6ccc0000000000000000000000000000000000000000000000000000abcd"

func fakeRegistries(t *testing.T) (*Client, string) {
	pkg := tgz(t,
		tf{name: "package/package.json", data: `{"name":"pkg","version":"1.0.0","pi":{"skills":["./skills"],"extensions":["./index.ts"]}}`},
		tf{name: "package/index.ts", data: "export default function () {}"},
		tf{name: "package/skills/hello/SKILL.md", data: "---\nname: hello\ndescription: hi\n---\n"},
		tf{name: "package/bin/run.js", data: "#!/usr/bin/env node\n", mode: 0o755})
	evil := tgz(t, tf{name: "package/../../escaped.txt", data: "x"})
	whl := []byte("PK\x03\x04 not really a wheel")
	whlSum := sha256.Sum256(whl)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		npm := func(name string, integ string, file string) {
			js(map[string]any{"name": name, "version": "1.0.0", "dist": map[string]string{"tarball": srv.URL + "/files/" + file, "integrity": integ},
				"optionalDependencies": map[string]string{name + "-darwin-arm64": "1.0.0"}, "scripts": map[string]string{"postinstall": "node x"}})
		}
		host := srv.Listener.Addr().String()
		servers := map[string]any{
			"io.github.t/npm":    map[string]any{"name": "io.github.t/npm", "version": "1.0.0", "packages": []any{map[string]any{"registryType": "npm", "identifier": "pkg", "version": "1.0.0", "transport": map[string]string{"type": "stdio"}, "packageArguments": []any{map[string]string{"type": "positional", "value": "--stdio"}, map[string]any{"type": "named", "name": "--cfg", "isRequired": true, "placeholder": "file"}}, "environmentVariables": []any{map[string]any{"name": "TOKEN", "isRequired": true}, map[string]any{"name": "OPT"}}}, map[string]any{"registryType": "pypi", "identifier": "pyx"}}},
			"io.github.t/py":     map[string]any{"name": "io.github.t/py", "version": "2.0.0", "packages": []any{map[string]any{"registryType": "pypi", "identifier": "pyx", "version": "2.0.0"}}},
			"io.github.t/oci":    map[string]any{"name": "io.github.t/oci", "version": "1.0.0", "packages": []any{map[string]any{"registryType": "oci", "identifier": host + "/o/img:v1", "transport": map[string]string{"type": "stdio"}}}},
			"io.github.t/ocibad": map[string]any{"name": "io.github.t/ocibad", "version": "1.0.0", "packages": []any{map[string]any{"registryType": "oci", "identifier": host + "/o/missing:v1"}}},
			"io.github.t/remote": map[string]any{"name": "io.github.t/remote", "version": "3.0.0", "remotes": []any{map[string]string{"type": "streamable-http", "url": "https://mcp.example/mcp"}}},
		}
		switch {
		case strings.HasPrefix(p, "/v0/servers/"):
			name, ver, _ := strings.Cut(strings.TrimPrefix(p, "/v0/servers/"), "/versions/")
			name, _ = url.PathUnescape(name)
			s, ok := servers[name]
			if !ok || (ver != "latest" && ver != s.(map[string]any)["version"]) {
				http.NotFound(w, r)
				return
			}
			js(map[string]any{"server": s, "_meta": map[string]any{}})
		case p == "/npm/pkg/1.0.0", p == "/npm/pkg/latest":
			npm("pkg", sri(pkg)+" sha1-abc", "pkg.tgz")
		case p == "/npm/bad/1.0.0":
			npm("bad", sri([]byte("other")), "pkg.tgz")
		case p == "/npm/evil/1.0.0":
			npm("evil", sri(evil), "evil.tgz")
		case p == "/files/pkg.tgz":
			_, _ = w.Write(pkg)
		case p == "/files/evil.tgz":
			_, _ = w.Write(evil)
		case p == "/files/pyx-2.0.0-py3-none-any.whl":
			_, _ = w.Write(whl)
		case p == "/pypi/pyx/2.0.0/json":
			js(map[string]any{"urls": []any{
				map[string]any{"filename": "pyx-2.0.0.tar.gz", "packagetype": "sdist", "url": srv.URL + "/files/nope", "digests": map[string]string{"sha256": strings.Repeat("0", 64)}},
				map[string]any{"filename": "pyx-2.0.0-py3-none-any.whl", "packagetype": "bdist_wheel", "url": srv.URL + "/files/pyx-2.0.0-py3-none-any.whl", "digests": map[string]string{"sha256": hex.EncodeToString(whlSum[:])}}}})
		case p == "/token":
			js(map[string]string{"token": "tok"})
		case p == "/v2/o/img/manifests/v1":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:o/img:pull"`)
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{MCPRegistry: srv.URL, NPM: srv.URL + "/npm", PyPI: srv.URL, GitHub: "file:///nonexistent"}, srv.Listener.Addr().String()
}

func TestMCP(t *testing.T) {
	c, host := fakeRegistries(t)
	ctx, cache := context.Background(), t.TempDir()
	res := func(s string) (Locked, error) {
		r, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return Resolve(ctx, r, cache, c)
	}

	l, err := res("mcp:io.github.t/npm")
	if err != nil {
		t.Fatal(err)
	}
	if l.Ref.Version != "1.0.0" || !reflect.DeepEqual(l.Command, []string{"npx", "-y", "pkg@1.0.0", "--stdio"}) ||
		!reflect.DeepEqual(l.Env, []string{"TOKEN"}) || !strings.HasPrefix(l.Integrity, "sha512-") || len(l.Notes) < 3 {
		t.Fatalf("npm: %+v", l)
	}
	if err := Verify(l); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(l.Path, "index.ts"), []byte("tampered"), 0o644)
	if Verify(l) == nil {
		t.Fatal("Verify missed a modified unpacked file")
	}
	if _, err := res("mcp:io.github.t/npm@1.0.0"); err != nil { // re-resolve restores
		t.Fatal(err)
	}
	_ = os.WriteFile(l.Archive, []byte("tampered"), 0o644)
	if Verify(l) == nil {
		t.Fatal("Verify missed a modified tarball")
	}
	if _, err := res("mcp:io.github.t/npm@9.9.9"); err == nil {
		t.Fatal("unknown version resolved")
	}

	l, err = res("mcp:io.github.t/py@2.0.0")
	if err != nil || l.Command[0] != "uvx" || l.Command[1] != "pyx==2.0.0" || !strings.HasPrefix(l.Integrity, "sha256:") {
		t.Fatalf("pypi: %+v %v", l, err)
	}
	if err := Verify(l); err != nil {
		t.Fatal(err)
	}

	l, err = res("mcp:io.github.t/oci")
	want := []string{"docker", "run", "--rm", "-i", host + "/o/img@" + digest}
	if err != nil || l.Integrity != digest || !reflect.DeepEqual(l.Command, want) {
		t.Fatalf("oci: %+v %v", l, err)
	}
	if err := Verify(l); err != nil {
		t.Fatal(err)
	}
	l, err = res("mcp:io.github.t/ocibad")
	if err != nil || l.Integrity != "" || !strings.Contains(strings.Join(l.Notes, ";"), "unpinned digest") {
		t.Fatalf("oci unpinned: %+v %v", l, err)
	}
	if Verify(l) == nil {
		t.Fatal("Verify passed an unpinned image")
	}

	l, err = res("mcp:io.github.t/remote")
	if err != nil || l.URL != "https://mcp.example/mcp" || Verify(l) != nil {
		t.Fatalf("remote: %+v %v", l, err)
	}
}

func TestNPMIntegrityZipSlipAndPi(t *testing.T) {
	c, _ := fakeRegistries(t)
	ctx, cache := context.Background(), t.TempDir()
	if _, err := Resolve(ctx, Ref{Kind: "pi", Name: "bad", Version: "1.0.0"}, cache, c); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("wrong integrity accepted: %v", err)
	}
	if exists(filepath.Join(cache, "npm", "bad@1.0.0", "pkg.tgz")) {
		t.Fatal("mismatched tarball kept")
	}
	if _, err := Resolve(ctx, Ref{Kind: "pi", Name: "evil", Version: "1.0.0"}, cache, c); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("zip-slip accepted: %v", err)
	}
	if exists(filepath.Join(cache, "npm", "escaped.txt")) || exists(filepath.Join(cache, "escaped.txt")) {
		t.Fatal("zip-slip wrote outside")
	}
	l, err := Resolve(ctx, Ref{Kind: "pi", Name: "pkg"}, cache, c)
	if err != nil {
		t.Fatal(err)
	}
	if l.Ref.Version != "1.0.0" || len(l.Skills) != 1 || filepath.Base(l.Skills[0]) != "hello" ||
		!strings.Contains(strings.Join(l.Notes, ";"), "not run by warden: ./index.ts") {
		t.Fatalf("pi: %+v", l)
	}
	if st, _ := os.Stat(filepath.Join(l.Path, "bin", "run.js")); st == nil || st.Mode()&0o100 == 0 {
		t.Fatal("exec bit lost")
	}
	if err := Verify(l); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	_ = os.MkdirAll(dir, 0o755)
	run(t, dir, "init", "-q", "-b", "main")
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		_ = os.WriteFile(p, []byte(data), mode)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")
	run(t, dir, "tag", "-a", "v1", "-m", "v1")
	return run(t, dir, "rev-parse", "HEAD")
}

func TestGitSkillsAndPlugins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	tmp := t.TempDir()
	gh := filepath.Join(tmp, "gh")
	extSHA := repo(t, filepath.Join(gh, "o", "ext"), map[string]string{"skills/x/SKILL.md": "x"})
	mkt := filepath.Join(tmp, "mkt")
	sha := repo(t, mkt, map[string]string{
		".claude-plugin/marketplace.json": `{"name":"m","owner":{"name":"o"},"metadata":{"pluginRoot":"./plugins"},"plugins":[
			{"name":"sec","source":"sec"},
			{"name":"docs","source":"./","strict":false,"skills":["./skills/pdf"]},
			{"name":"ext","source":{"source":"github","repo":"o/ext","ref":"v1"}},
			{"name":"cmd","source":{"source":"command","command":"echo hi"}},
			{"name":"esc","source":"../../etc"}]}`,
		"plugins/sec/hooks/hooks.json":           `{"hooks":{}}`,
		"plugins/sec/.mcp.json":                  `{"mcpServers":{}}`,
		"plugins/sec/skills/guard/SKILL.md":      "guard",
		"plugins/sec/.claude-plugin/plugin.json": `{"name":"sec","mcpServers":{"a":{}}}`,
		"skills/pdf/SKILL.md":                    "pdf",
		"skills/pdf/run.sh":                      "#!/bin/sh\n",
		"package.json":                           `{"name":"r","omp":{"skills":["skills/*"],"extensions":["ext.ts"]}}`,
	})
	c := &Client{GitHub: "file://" + gh}
	ctx, cache := context.Background(), t.TempDir()
	res := func(s string) (Locked, error) {
		r, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return Resolve(ctx, r, cache, c)
	}

	l, err := res("skill:file://" + mkt + "//skills/pdf@v1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Resolved != sha || !reflect.DeepEqual(l.Skills, []string{l.Path}) || !strings.HasPrefix(l.Integrity, "sha256:") {
		t.Fatalf("skill: %+v", l)
	}
	if err := Verify(l); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(filepath.Dir(filepath.Dir(l.Path)), ".git")) {
		t.Fatal(".git kept")
	}
	l2, err := res("skill:file://" + mkt + "//skills/pdf@" + sha)
	if err != nil || l2.Integrity != l.Integrity {
		t.Fatalf("tree hash not deterministic: %v %s %s", err, l.Integrity, l2.Integrity)
	}
	_ = os.WriteFile(filepath.Join(l2.Path, "SKILL.md"), []byte("changed"), 0o644)
	if Verify(l2) == nil {
		t.Fatal("Verify missed a modified file")
	}
	if _, err := res("skill:file://" + mkt + "//nope@v1"); err == nil {
		t.Fatal("missing subpath resolved")
	}

	l, err = res("plugin:sec@file://" + mkt)
	if err != nil {
		t.Fatal(err)
	}
	if l.Resolved != sha || len(l.Skills) != 1 || filepath.Base(l.Skills[0]) != "guard" ||
		filepath.Base(l.Hooks) != "hooks.json" || filepath.Base(l.MCPJSON) != ".mcp.json" || len(l.Notes) != 1 || Verify(l) != nil {
		t.Fatalf("plugin sec: %+v", l)
	}
	l, err = res("plugin:docs@file://" + mkt + "@v1")
	if err != nil || len(l.Skills) != 1 || filepath.Base(l.Skills[0]) != "pdf" || l.Hooks != "" {
		t.Fatalf("plugin docs: %+v %v", l, err)
	}
	l, err = res("plugin:ext@file://" + mkt)
	if err != nil || l.Resolved != extSHA || len(l.Skills) != 1 || Verify(l) != nil {
		t.Fatalf("plugin ext: %+v %v", l, err)
	}
	for _, bad := range []string{"cmd", "esc", "missing"} {
		if _, err := res("plugin:" + bad + "@file://" + mkt); err == nil {
			t.Errorf("plugin %s resolved", bad)
		}
	}

	l, err = res("pi:git:file://" + mkt + "@v1")
	if err != nil || l.Resolved != sha || len(l.Skills) != 1 || !strings.Contains(strings.Join(l.Notes, ";"), "ext.ts") {
		t.Fatalf("pi git: %+v %v", l, err)
	}
}

func TestLockRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "warden.lock")
	if l, err := ReadLock(p); err != nil || len(l.Entries) != 0 {
		t.Fatal("missing lock should be empty", err)
	}
	var l Lock
	a := Locked{Ref: Ref{Kind: "mcp", Name: "io.github.x/y", Version: "1.0.0"}, Resolved: "https://r/x.tgz", Integrity: "sha512-AAAA",
		Path: "/c/x", Archive: "/c/x.tgz", Command: []string{"npx", "-y", "x@1.0.0"}, Env: []string{"TOKEN"}, Notes: []string{"n"}}
	b := Locked{Ref: Ref{Kind: "plugin", Name: "sec", Source: "github.com/o/m"}, Resolved: strings.Repeat("a", 40), Integrity: "sha256:00",
		Path: "/c/p", Skills: []string{"/c/p/skills/s"}, Hooks: "/c/p/hooks/hooks.json", MCPJSON: "/c/p/.mcp.json"}
	l.Put(b)
	l.Put(a)
	a.Ref.Version, a.Integrity = "1.0.1", "sha512-BBBB"
	l.Put(a)
	if len(l.Entries) != 2 || l.Entries[0].Ref.Kind != "mcp" || l.Entries[0].Integrity != "sha512-BBBB" {
		t.Fatalf("Put: %+v", l.Entries)
	}
	if err := l.Write(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLock(p)
	raw, _ := os.ReadFile(p)
	if err != nil || !reflect.DeepEqual(got, l) || !bytes.Contains(raw, []byte("[[extension]]")) {
		t.Fatalf("round trip: %v\n%s\n%+v", err, raw, got)
	}
}
