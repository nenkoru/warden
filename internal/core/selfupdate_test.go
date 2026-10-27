package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/github/githubtest"
	"github.com/nenkoru/warden/internal/update"
)

type passVerifier struct{}

func (passVerifier) Verify(context.Context, update.Subject) error { return nil }

// fakeRelease: a release whose archive holds a stand-in binary answering the preflight.
func fakeRelease(t *testing.T, f *githubtest.Fake, tag string, at time.Time) {
	ver := strings.TrimPrefix(tag, "v")
	bin := []byte("#!/bin/sh\necho warden " + ver + "\n")
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "warden", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(bin)
	tw.Close()
	zw.Close()
	name := "warden_" + ver + "_linux_amd64.tar.gz"
	sum := sha256.Sum256(b.Bytes())
	f.AddRelease(tag, false, at, map[string][]byte{name: b.Bytes(), "checksums.txt": []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")})
}

// TestSelfUpdateTick: verified at tick start, installed at the healthy end
// (the process asked to restart); a new binary that never ends a tick is
// rolled back on the next start, and the rollback pings.
func TestSelfUpdateTick(t *testing.T) {
	f := githubtest.New(t, "tok", "bot", "acme/warden")
	f.Anonymous = true
	root := t.TempDir()
	bin := filepath.Join(root, "warden")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho warden 0.2.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, root, `domain = "`+touchDomain(t, root)+`"
mode = "dry-run"
params = { root = "`+root+`" }
[update]
mode = "install"
repo = "acme/warden"
api = "`+f.URL()+`"
bin = "warden"
canary_hours = 0
`)
	if cfg.Update.Bin != bin || cfg.Update.Channel != "stable" {
		t.Fatalf("%+v", cfg.Update)
	}
	fakeRelease(t, f, "v0.3.0", time.Now().Add(-time.Hour))
	in := open(t, cfg)
	var ran []string
	in.Run = fakeRun(&ran)
	in.Upd.Current, in.Upd.GOOS, in.Upd.GOARCH, in.Upd.Verifier = "0.2.0", "linux", "amd64", passVerifier{}
	in.Upd.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	t1, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := os.ReadFile(filepath.Join(cfg.State, "reports", "latest.md"))
	if !strings.Contains(string(rep), "- version v0.2.0 (update: v0.3.0 verified, installing at the end of this tick)") {
		t.Fatal(string(rep))
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.3.0") || !t1.Restart {
		t.Fatalf("not installed (restart %v): %s %v", t1.Restart, b, t1.Errors)
	}
	if j, _ := os.ReadFile(filepath.Join(cfg.State, "journal.jsonl")); !strings.Contains(string(j), `"ev":"update.installed"`) {
		t.Fatal(string(j))
	}

	// the new binary begins a tick and dies; the next start rolls back
	in.Upd.Current = "0.3.0"
	if _, err := in.Upd.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	t2, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rb *Item
	for _, it := range t2.Items {
		if it.Kind == "rollback" {
			rb = it
		}
	}
	if _, urgent := attention(t2); rb == nil || rb.Disp != Report || !rb.New || !t2.Restart || len(urgent) == 0 { // a rollback pings at once
		t.Fatalf("no pinging rollback: %+v", t2.Items)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "0.2.0") {
		t.Fatal("the previous binary is not back")
	}
	rep, _ = os.ReadFile(t2.Report)
	if !strings.Contains(string(rep), "rolled back warden v0.3.0 → v0.2.0") || !strings.Contains(string(rep), "(update: rolled back from v0.3.0") {
		t.Fatal(string(rep))
	}
}

func TestUpdateConfig(t *testing.T) {
	root := t.TempDir()
	cfg := writeConfig(t, root, `domain = "`+touchDomain(t, root)+`"
`)
	if cfg.Update.Mode != "off" || cfg.Update.Repo != "nenkoru/warden" || cfg.Update.Every != "6h" {
		t.Fatalf("%+v", cfg.Update)
	}
	for _, bad := range []string{"[update]\nmode = \"yes\"\n", "[update]\nevery = \"10m\"\n", "[update]\nauto = true\n"} {
		p := filepath.Join(root, "bad.toml")
		_ = os.WriteFile(p, []byte(`domain = "`+touchDomain(t, root)+"\"\n"+bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	in := open(t, cfg)
	if l := in.versionLine(); !strings.HasPrefix(l, "version ") || !strings.HasSuffix(l, "(updates off)") {
		t.Fatal(l)
	}
}
