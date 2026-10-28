package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

const testToken = "ghp_beaconSecret0123"

// fakeVerifier: passes every subject except the digests in fail.
type fakeVerifier struct {
	fail map[string]bool
	seen []Subject
}

func (v *fakeVerifier) Verify(_ context.Context, s Subject) error {
	v.seen = append(v.seen, s)
	if v.fail[s.Digest] {
		return errors.New("provenance: no attestation verifies")
	}
	return nil
}

// world: a fake GitHub with releases, a binary to replace, a fake clock.
type world struct {
	t   *testing.T
	f   *githubtest.Fake
	u   *Updater
	v   *fakeVerifier
	now time.Time
	bin string
	dir string
}

func newWorld(t *testing.T, mode string, edit ...func(*Config)) *world {
	t.Helper()
	w := &world{t: t, f: githubtest.New(t, testToken, "bot", "acme/warden"), v: &fakeVerifier{fail: map[string]bool{}},
		now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), dir: t.TempDir()}
	w.f.Anonymous = true
	w.bin = filepath.Join(w.dir, "bin", "warden")
	if err := os.MkdirAll(filepath.Dir(w.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.bin, script("0.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Mode: mode, Repo: "acme/warden", API: w.f.URL(), Bin: w.bin}
	for _, e := range edit {
		e(&cfg)
	}
	if err := cfg.Normalize(func(p string) string { return p }); err != nil {
		t.Fatal(err)
	}
	u, err := New(cfg, "mac", filepath.Join(w.dir, "state"), "")
	if err != nil {
		t.Fatal(err)
	}
	u.Current, u.GOOS, u.GOARCH, u.K8s = "0.2.0", "linux", "amd64", false
	u.Now = func() time.Time { return w.now }
	u.Verifier = w.v
	u.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	w.u = u
	return w
}

// script: a stand-in warden binary that answers the preflight.
func script(ver string) []byte { return []byte("#!/bin/sh\necho warden " + ver + "\n") }

func tarGz(t *testing.T, files map[string][]byte, types map[string]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		h := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if ty, ok := types[name]; ok {
			h.Typeflag, h.Size, h.Linkname = ty, 0, "/etc/passwd"
			content = nil
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// release publishes tag (ver without v) for linux/amd64 with a matching checksums.txt.
func (w *world) release(tag string, pre bool, age time.Duration, extra map[string][]byte) []byte {
	ver := strings.TrimPrefix(tag, "v")
	arch := tarGz(w.t, map[string][]byte{"warden": script(ver), "README.md": []byte("hi"), "packs/x/domain.toml": []byte("")}, nil)
	name := fmt.Sprintf("warden_%s_linux_amd64.tar.gz", ver)
	sums := fmt.Sprintf("%s  %s\n%s  warden_%s_darwin_arm64.tar.gz\n", sha(arch), name, strings.Repeat("0", 64), ver)
	assets := map[string][]byte{name: arch}
	for n, b := range extra {
		assets[n] = b
		sums += fmt.Sprintf("%s  %s\n", sha(b), n)
	}
	assets["checksums.txt"] = []byte(sums)
	w.f.AddRelease(tag, pre, w.now.Add(-age), assets)
	return arch
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (w *world) begin() []string {
	w.t.Helper()
	fs, err := w.u.Begin(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Kind+"/"+f.Severity+": "+f.Title)
	}
	return out
}

// TestInstallAndConfirm: verified at tick start, installed at a healthy
// end, confirmed by the new binary's first healthy tick.
func TestInstallAndConfirm(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	if fs := w.begin(); len(fs) != 0 {
		t.Fatal(fs)
	}
	if s := w.u.Status(); !strings.Contains(s, "v0.3.0 verified, installing at the end of this tick") || !strings.HasPrefix(s, "version v0.2.0 (update: ") {
		t.Fatal(s)
	}
	if len(w.v.seen) != 1 || w.v.seen[0].Name != "warden_0.3.0_linux_amd64.tar.gz" || w.v.seen[0].Tag != "v0.3.0" || w.v.seen[0].Repo != "acme/warden" {
		t.Fatalf("verified %+v", w.v.seen)
	}
	if read(t, w.bin) != string(script("0.2.0")) {
		t.Fatal("installed before the tick ended")
	}
	if err := w.u.End(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if read(t, w.bin) != string(script("0.3.0")) || read(t, w.bin+".prev") != string(script("0.2.0")) {
		t.Fatal("not installed with the previous kept")
	}
	if st, _ := os.Stat(w.bin); st.Mode().Perm() != 0o755 {
		t.Fatal(st.Mode())
	}
	if _, err := os.Stat(w.bin + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
	if !w.u.Restart || w.u.Installed != "v0.3.0" {
		t.Fatal("no restart asked")
	}
	st, _ := loadState(w.u.path())
	if !st.Pending || st.From != "v0.2.0" || st.To != "v0.3.0" || st.Attempts != 0 || !st.At.Equal(w.now) {
		t.Fatalf("%+v", st)
	}
	// the new binary's first tick
	w.u.Current = "0.3.0"
	w.now = w.now.Add(time.Minute)
	w.begin()
	if st, _ := loadState(w.u.path()); st.Attempts != 1 || !st.Pending {
		t.Fatalf("%+v", st)
	}
	if s := w.u.Status(); !strings.Contains(s, "installed v0.3.0, pending health (tick 1)") {
		t.Fatal(s)
	}
	if err := w.u.End(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st, _ = loadState(w.u.path())
	if st.Pending || st.Attempts != 0 || !st.HealthySince.Equal(w.now) || st.TicksOK != 1 {
		t.Fatalf("%+v", st)
	}
	if s := w.u.Status(); s != "version v0.3.0 (update: up to date)" {
		t.Fatal(s)
	}
}

// TestRollback: the new binary begins a tick and never ends one; the next
// start restores the previous binary, pings, and never takes it again.
func TestRollback(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	w.begin()
	if err := w.u.End(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	w.u.Current = "0.3.0"
	w.now = w.now.Add(time.Minute)
	w.begin() // attempt 1 … and it crashes: no End
	w.now = w.now.Add(time.Minute)
	fs := w.begin()
	if len(fs) != 1 || !strings.HasPrefix(fs[0], "rollback/high: rolled back warden v0.3.0 → v0.2.0") {
		t.Fatal(fs)
	}
	if read(t, w.bin) != string(script("0.2.0")) || !w.u.Restart {
		t.Fatal("the previous binary is not back")
	}
	if _, err := os.Stat(w.bin + ".prev"); err == nil {
		t.Fatal(".prev should have moved back")
	}
	st, _ := loadState(w.u.path())
	if st.Pending || !st.bad("v0.3.0") || st.RolledBack != "v0.3.0" {
		t.Fatalf("%+v", st)
	}
	if s := w.u.Status(); !strings.Contains(s, "rolled back from v0.3.0") {
		t.Fatal(s)
	}
	// the restored binary: v0.3.0 is never installed again
	w.u.Current = "0.2.0"
	w.now = w.now.Add(7 * time.Hour)
	if fs := w.begin(); len(fs) != 0 || w.u.ready != nil {
		t.Fatal(fs, w.u.ready)
	}
	if s := w.u.Status(); !strings.Contains(s, "v0.3.0 rolled back here before") {
		t.Fatal(s)
	}
	_ = w.u.End(context.Background(), true)
	if read(t, w.bin) != string(script("0.2.0")) {
		t.Fatal("reinstalled a rolled-back release")
	}
	// a newer release is taken
	w.release("v0.3.1", false, 48*time.Hour, nil)
	w.now = w.now.Add(7 * time.Hour)
	w.begin()
	if w.u.ready == nil || w.u.ready.tag != "v0.3.1" {
		t.Fatal("v0.3.1 not taken")
	}
}

// TestRollbackWithoutPrevious: nothing to restore — still recorded, still pinging.
func TestRollbackWithoutPrevious(t *testing.T) {
	w := newWorld(t, "install")
	w.u.Current = "0.3.0"
	if err := saveState(w.u.path(), State{From: "v0.2.0", To: "v0.3.0", Pending: true, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	fs, err := w.u.Begin(context.Background())
	if err != nil || len(fs) != 1 || fs[0].Kind != "rollback" || !strings.Contains(fs[0].Detail, "FAILED") {
		t.Fatal(fs, err)
	}
}

// TestNotRunningYet: installed by hand while a listen process runs the old
// binary — its ticks count nothing, and it asks for one restart.
func TestNotRunningYet(t *testing.T) {
	w := newWorld(t, "install")
	if err := saveState(w.u.path(), State{From: "v0.2.0", To: "v0.3.0", Pending: true}); err != nil {
		t.Fatal(err)
	}
	w.begin()
	if !w.u.Restart {
		t.Fatal("no restart asked")
	}
	_ = w.u.End(context.Background(), true)
	w.begin()
	if w.u.Restart {
		t.Fatal("restart asked twice")
	}
	st, _ := loadState(w.u.path())
	if !st.Pending || st.Attempts != 0 {
		t.Fatalf("%+v", st)
	}
	if s := w.u.Status(); !strings.Contains(s, "not running yet") {
		t.Fatal(s)
	}
}

// TestUnhealthyEnd: a tick that returns an error installs nothing and confirms nothing.
func TestUnhealthyEnd(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	w.begin()
	if err := w.u.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if read(t, w.bin) != string(script("0.2.0")) || w.u.Restart {
		t.Fatal("installed after an unhealthy tick")
	}
}

func TestPlan(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.1.9", false, 100*time.Hour, nil)
	w.release("v0.3.0", false, 30*time.Hour, nil)
	w.release("v0.4.0-rc.1", true, 100*time.Hour, nil)
	w.release("v0.4.0", false, 2*time.Hour, nil)
	w.f.With(func(f *githubtest.Fake) {
		f.Releases = append(f.Releases, github.Release{TagName: "v9.0.0", Draft: true}, github.Release{TagName: "junk"})
	})
	ctx := context.Background()
	pick, waiting, err := w.u.plan(ctx)
	if err != nil || pick == nil || pick.TagName != "v0.3.0" || !strings.Contains(waiting, "v0.4.0 published 2h0m0s ago; stable waits 24h") {
		t.Fatal(pick, waiting, err)
	}
	// an open warden-rollback issue names v0.3.0: the stable waits
	w.f.AddIssue("release v0.3.0 rolled back on buvg01", "", "bot", "OWNER", "warden-rollback")
	w.f.AddIssue("release v0.3.01 is fine", "", "x", "NONE", "warden-rollback")
	if pick, waiting, _ := w.u.plan(ctx); pick != nil || !strings.Contains(waiting, "v0.4.0") {
		t.Fatal(pick, waiting)
	}
	// canary: prereleases too, no waiting — but not what a rollback issue names
	*w.u.Cfg.CanaryHours = 0
	w.u.Cfg.Channel = "canary"
	if pick, _, _ := w.u.plan(ctx); pick == nil || pick.TagName != "v0.4.0" {
		t.Fatal(pick)
	}
	w.u.st.Bad = []string{"v0.4.0"}
	if pick, waiting, _ := w.u.plan(ctx); pick == nil || pick.TagName != "v0.4.0-rc.1" || !strings.Contains(waiting, "rolled back here") {
		t.Fatal(pick, waiting)
	}
	// dev: never, unless allow_dev
	w.u.Current = "dev"
	if pick, waiting, _ := w.u.plan(ctx); pick != nil || !strings.Contains(waiting, "dev build") {
		t.Fatal(pick, waiting)
	}
	w.u.Cfg.AllowDev = true
	if pick, _, _ := w.u.plan(ctx); pick == nil {
		t.Fatal("allow_dev")
	}
	// no archive for this platform
	w.u.GOOS, w.u.st.Bad = "plan9", nil
	if pick, waiting, _ := w.u.plan(ctx); pick != nil || !strings.Contains(waiting, "plan9") {
		t.Fatal(pick, waiting)
	}
}

// TestPlanRollbackIssuesUnreadable: when the rollback issues cannot be read, nothing is taken.
func TestPlanRollbackIssuesUnreadable(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues") {
			http.Error(rw, `{"message":"rate limited"}`, http.StatusForbidden)
			return
		}
		w.f.Server.Config.Handler.ServeHTTP(rw, r)
	}))
	defer srv.Close()
	w.u.GH, _ = github.Anonymous(srv.URL, "acme/warden")
	if pick, _, err := w.u.plan(context.Background()); err == nil || pick != nil {
		t.Fatal("took a release without reading anything")
	}
}

// TestVerificationFailsClosed: a wrong checksum, a missing line, a failed
// provenance, a bad archive — a finding that pings, and nothing installed.
func TestVerificationFailsClosed(t *testing.T) {
	cases := map[string]func(w *world){
		"provenance": func(w *world) {
			arch := w.release("v0.3.0", false, 48*time.Hour, nil)
			w.v.fail[sha(arch)] = true
		},
		"checksum": func(w *world) {
			w.release("v0.3.0", false, 48*time.Hour, nil)
			w.f.With(func(f *githubtest.Fake) {
				for _, a := range f.Releases[0].Assets {
					if strings.HasSuffix(a.Name, ".tar.gz") {
						f.Assets[a.ID] = append(f.Assets[a.ID], 0)
					}
				}
			})
		},
		"no line": func(w *world) {
			w.release("v0.3.0", false, 48*time.Hour, nil)
			w.f.With(func(f *githubtest.Fake) {
				for _, a := range f.Releases[0].Assets {
					if a.Name == "checksums.txt" {
						f.Assets[a.ID] = []byte(strings.Repeat("0", 64) + "  other.tar.gz\n")
					}
				}
			})
		},
		"no binary": func(w *world) {
			arch := tarGz(w.t, map[string][]byte{"README.md": []byte("x")}, nil)
			w.f.AddRelease("v0.3.0", false, w.now.Add(-48*time.Hour), map[string][]byte{"warden_0.3.0_linux_amd64.tar.gz": arch,
				"checksums.txt": []byte(sha(arch) + "  warden_0.3.0_linux_amd64.tar.gz\n")})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, "install")
			setup(w)
			fs := w.begin()
			if len(fs) != 1 || !strings.HasPrefix(fs[0], "update/high: warden release v0.3.0 fails verification — not installed") {
				t.Fatal(fs)
			}
			if w.u.ready != nil {
				t.Fatal("a candidate after a failed verification")
			}
			_ = w.u.End(context.Background(), true)
			if read(t, w.bin) != string(script("0.2.0")) {
				t.Fatal("installed an unverified release")
			}
			if s := w.u.Status(); !strings.Contains(s, "last check:") {
				t.Fatal(s)
			}
		})
	}
}

// TestPreflight: a binary that does not start (or says another version) replaces nothing.
func TestPreflight(t *testing.T) {
	w := newWorld(t, "install")
	arch := tarGz(t, map[string][]byte{"warden": script("9.9.9")}, nil)
	w.f.AddRelease("v0.3.0", false, w.now.Add(-48*time.Hour), map[string][]byte{"warden_0.3.0_linux_amd64.tar.gz": arch,
		"checksums.txt": []byte(sha(arch) + "  warden_0.3.0_linux_amd64.tar.gz\n")})
	w.begin()
	if err := w.u.End(context.Background(), true); err == nil || !strings.Contains(err.Error(), "says") {
		t.Fatal(err)
	}
	if read(t, w.bin) != string(script("0.2.0")) || w.u.Restart {
		t.Fatal("replaced by a binary that failed its preflight")
	}
	if _, err := os.Stat(w.bin + ".new"); err == nil {
		t.Fatal(".new left behind")
	}
	if st, _ := loadState(w.u.path()); st.Pending || !strings.Contains(st.Error, "says") {
		t.Fatalf("%+v", st)
	}
}

func TestBinSymlinkRefused(t *testing.T) {
	w := newWorld(t, "install")
	link := filepath.Join(w.dir, "link")
	if err := os.Symlink(w.bin, link); err != nil {
		t.Fatal(err)
	}
	w.u.Cfg.Bin = link
	if err := w.u.install(context.Background(), "v0.3.0", script("0.3.0")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal(err)
	}
}

func TestEvery(t *testing.T) {
	w := newWorld(t, "propose")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	if fs := w.begin(); len(fs) != 1 || !strings.HasPrefix(fs[0], "update/low: update available: warden v0.3.0 (verified; running 0.2.0)") {
		t.Fatal(fs)
	}
	_ = w.u.End(context.Background(), true)
	if read(t, w.bin) != string(script("0.2.0")) {
		t.Fatal("propose installed")
	}
	n := len(w.f.Requests)
	w.now = w.now.Add(5 * time.Hour)
	w.begin()
	if len(w.f.Requests) != n {
		t.Fatal("checked before update.every")
	}
	if s := w.u.Status(); !strings.Contains(s, "v0.3.0 available") {
		t.Fatal(s)
	}
	w.now = w.now.Add(time.Hour)
	w.begin()
	if len(w.f.Requests) == n {
		t.Fatal("not checked after update.every")
	}
}

func TestOff(t *testing.T) {
	w := newWorld(t, "off")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	if fs := w.begin(); len(fs) != 0 || len(w.f.Requests) != 0 {
		t.Fatal(fs, w.f.Requests)
	}
	_ = w.u.End(context.Background(), true)
	if _, err := os.Stat(w.u.path()); err == nil {
		t.Fatal("state written while off")
	}
	if s := w.u.Status(); s != "version v0.2.0 (updates off)" {
		t.Fatal(s)
	}
}

// TestRun: `warden update --check`, then `warden update`.
func TestRun(t *testing.T) {
	w := newWorld(t, "off")
	w.release("v0.3.0", false, 48*time.Hour, nil)
	msg, err := w.u.Run(context.Background(), true)
	if err != nil || !strings.Contains(msg, "v0.3.0 verified") || read(t, w.bin) != string(script("0.2.0")) {
		t.Fatal(msg, err)
	}
	msg, err = w.u.Run(context.Background(), false)
	if err != nil || !strings.Contains(msg, "installed v0.3.0") || read(t, w.bin) != string(script("0.3.0")) {
		t.Fatal(msg, err)
	}
	if msg, _ := w.u.Run(context.Background(), false); !strings.Contains(msg, "waits for its first healthy tick") {
		t.Fatal(msg)
	}
	// the state machine runs although mode is off: an install is in flight
	w.u.Current = "0.3.0"
	w.begin()
	if st, _ := loadState(w.u.path()); st.Attempts != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestExtract(t *testing.T) {
	ok := tarGz(t, map[string][]byte{"warden": []byte("bin"), "packs/warden/x": []byte("no")}, nil)
	if b, err := extract(ok); err != nil || string(b) != "bin" {
		t.Fatal(string(b), err)
	}
	nested := tarGz(t, map[string][]byte{"warden_0.3.0/warden": []byte("bin")}, nil)
	if b, err := extract(nested); err != nil || string(b) != "bin" {
		t.Fatal(string(b), err)
	}
	for name, a := range map[string][]byte{
		"traversal": tarGz(t, map[string][]byte{"../warden": []byte("x")}, nil),
		"absolute":  tarGz(t, map[string][]byte{"/usr/bin/warden": []byte("x")}, nil),
		"inside":    tarGz(t, map[string][]byte{"a/../../warden": []byte("x")}, nil),
		"symlink":   tarGz(t, map[string][]byte{"warden": []byte("x")}, map[string]byte{"warden": tar.TypeSymlink}),
		"two":       tarGz(t, map[string][]byte{"warden": []byte("x"), "d/warden": []byte("y")}, nil),
		"none":      tarGz(t, map[string][]byte{"README.md": []byte("x")}, nil),
		"not gzip":  []byte("plain"),
	} {
		if _, err := extract(a); err == nil {
			t.Errorf("%s: extracted", name)
		}
	}
}

func TestConfig(t *testing.T) {
	c := Config{}
	if err := c.Normalize(func(p string) string { return "/abs/" + p }); err != nil {
		t.Fatal(err)
	}
	if c.Mode != "off" || c.Repo != "nenkoru/warden" || c.Every != "6h" || c.Channel != "stable" || *c.CanaryHours != 24 ||
		c.API != "https://api.github.com" || c.Workflow != ".github/workflows/release.yml" || c.SignerRef != "refs/tags/{tag}" || c.Container != "warden" {
		t.Fatalf("%+v", c)
	}
	zero := 0
	c = Config{Mode: "install", CanaryHours: &zero, TokenFile: "tok"}
	if err := c.Normalize(func(p string) string { return "/abs/" + p }); err != nil || *c.CanaryHours != 0 || c.TokenFile != "/abs/tok" {
		t.Fatal(err, c)
	}
	neg := -1
	for _, bad := range []Config{{Mode: "auto"}, {Every: "30m"}, {Every: "x"}, {Channel: "beta"}, {CanaryHours: &neg}, {Repo: "nope"},
		{API: "http://api.github.com"}, {SignerRef: "refs/tags/("}, {GrantImageBump: true, Image: "x"}} {
		if err := bad.Normalize(func(p string) string { return p }); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// TestCosign: run when cosign is installed and the release carries a
// bundle; a failure fails the release; absence is fine.
func TestCosign(t *testing.T) {
	w := newWorld(t, "install")
	w.release("v0.3.0", false, 48*time.Hour, map[string][]byte{"checksums.txt.bundle": []byte(`{}`)})
	var calls [][]string
	fail := false
	w.u.LookPath = func(n string) (string, error) { return "/usr/bin/" + n, nil }
	w.u.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/cosign" {
			return runCmd(context.Background(), name, args...)
		}
		calls = append(calls, args)
		if fail {
			return []byte("bad signature"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if fs := w.begin(); len(fs) != 0 || w.u.ready == nil || !strings.Contains(strings.Join(w.u.ready.notes, ";"), "cosign: checksums.txt") {
		t.Fatal(fs, w.u.ready)
	}
	if len(calls) != 1 || calls[0][0] != "verify-blob" || calls[0][1] != "--bundle" || !strings.Contains(strings.Join(calls[0], " "), "--certificate-oidc-issuer "+Issuer) {
		t.Fatal(calls)
	}
	fail = true
	w.now = w.now.Add(7 * time.Hour)
	if fs := w.begin(); len(fs) != 1 || !strings.Contains(fs[0], "fails verification") || w.u.ready != nil {
		t.Fatal(fs)
	}
}
