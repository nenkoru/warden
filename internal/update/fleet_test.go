package update

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/github/githubtest"
)

func withBeacon(t *testing.T) func(*Config) {
	p := filepath.Join(t.TempDir(), "beacon-token")
	if err := os.WriteFile(p, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(c *Config) { c.BeaconTokenFile = p }
}

func (w *world) requests(prefix string) int {
	n := 0
	w.f.With(func(f *githubtest.Fake) {
		for _, r := range f.Requests {
			if strings.HasPrefix(r, prefix) {
				n++
			}
		}
	})
	return n
}

func TestBeacon(t *testing.T) {
	w := newWorld(t, "propose", withBeacon(t))
	// someone else's comment claiming this instance's name: never edited, never trusted
	fleet := w.f.AddIssue(FleetTitle, "", "bot", "OWNER", FleetLabel)
	eve := w.f.AddComment(fleet, "eve", `<!-- warden-fleet {"name":"mac","version":"v9.9.9","channel":"stable","ticks_ok":99} -->`)
	ctx := context.Background()
	w.begin()
	if err := w.u.End(ctx, true); err != nil {
		t.Fatal(err)
	}
	var mine int64
	w.f.With(func(f *githubtest.Fake) {
		if len(f.Comments[fleet]) != 2 {
			t.Fatalf("%q", f.Comments[fleet])
		}
		c := f.Comments[fleet][1]
		if !strings.Contains(c, "**mac** · v0.2.0 (stable)") || !strings.Contains(c, `<!-- warden-fleet {"name":"mac","version":"v0.2.0","channel":"stable","healthy_since":"2026-10-28T12:00:00Z","ticks_ok":1} -->`) {
			t.Fatal(c)
		}
		mine = int64(fleet)*1000 + 2
	})
	// within the hour: nothing; later: the same comment rewritten
	w.now = w.now.Add(30 * time.Minute)
	w.begin()
	_ = w.u.End(ctx, true)
	if w.requests("PATCH") != 0 {
		t.Fatal("rewritten within the hour")
	}
	w.now = w.now.Add(time.Hour)
	w.begin()
	if err := w.u.End(ctx, true); err != nil {
		t.Fatal(err)
	}
	if w.requests("PATCH /repos/acme/warden/issues/comments/"+strconv.FormatInt(mine, 10)) != 1 || w.requests("PATCH") != 1 {
		t.Fatal(w.f.Requests)
	}
	w.f.With(func(f *githubtest.Fake) {
		if len(f.Comments[fleet]) != 2 || !strings.Contains(f.Comments[fleet][1], `"ticks_ok":3`) || !strings.Contains(f.Comments[fleet][0], "v9.9.9") {
			t.Fatalf("%q", f.Comments[fleet])
		}
		for _, b := range f.Bodies {
			if strings.Contains(b, testToken) {
				t.Fatal("the token left in a body")
			}
		}
	})
	if err := w.u.Beacon.EditComment(ctx, eve, "mine now"); err == nil || !strings.Contains(err.Error(), "not this token's") {
		t.Fatal(err)
	}
	bs, err := FleetStatus(ctx, w.u.Beacon)
	if err != nil || len(bs) != 1 || bs[0].Name != "mac" || bs[0].Version != "v0.2.0" || bs[0].TicksOK != 3 || bs[0].Author != "bot" {
		t.Fatalf("%+v %v", bs, err)
	}
}

func TestBeaconCreatesFleetIssue(t *testing.T) {
	w := newWorld(t, "propose", withBeacon(t))
	w.begin()
	if err := w.u.End(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	w.f.With(func(f *githubtest.Fake) {
		var n int
		for _, i := range f.Issues {
			if i.Title == FleetTitle && i.Has(FleetLabel) {
				n = i.Number
			}
		}
		if n == 0 || len(f.Comments[n]) != 1 {
			t.Fatalf("no fleet issue with one comment: %v", f.Requests)
		}
	})
	if w.requests("POST /graphql") != 1 {
		t.Fatal("the fleet issue is not pinned")
	}
}

// TestRollbackIssue: a rollback with the beacon token opens one warden-rollback issue.
func TestRollbackIssue(t *testing.T) {
	w := newWorld(t, "install", withBeacon(t))
	w.u.Current = "0.3.0"
	for range 2 {
		if err := saveState(w.u.path(), State{From: "v0.2.0", To: "v0.3.0", Pending: true, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		if fs := w.begin(); len(fs) != 1 || strings.Contains(fs[0], "could not") {
			t.Fatal(fs)
		}
	}
	n := 0
	w.f.With(func(f *githubtest.Fake) {
		for _, i := range f.Issues {
			if i.Title == "release v0.3.0 rolled back on mac" && i.Has(RollbackLabel) {
				n++
			}
		}
	})
	if n != 1 {
		t.Fatalf("%d rollback issues", n)
	}
	// and a stable instance then waits on it
	w.u.Current = "0.2.0"
	w.release("v0.3.0", false, 48*time.Hour, nil)
	w.u.st.Bad = nil
	if pick, waiting, _ := w.u.plan(context.Background()); pick != nil || !strings.Contains(waiting, "warden-rollback issue") {
		t.Fatal(pick, waiting)
	}
}

func TestNamesVersion(t *testing.T) {
	for text, want := range map[string]bool{
		"release v0.3.0-rc.1 rolled back on buvg01": true, "v0.3.0-rc.1": true, "0.3.0-rc.1, see logs": true, "(v0.3.0-rc.1).": true,
		"release v0.3.0-rc.10 rolled back": false, "release v0.3.0 rolled back": false, "v0.3.0-rc.1x": false, "v10.3.0-rc.1": false,
	} {
		if NamesVersion(text, "v0.3.0-rc.1") != want {
			t.Errorf("NamesVersion(%q) = %v", text, !want)
		}
	}
	if NamesVersion("release v0.3.0-rc.1 rolled back", "v0.3.0") {
		t.Error("an rc names its final version")
	}
}

func TestParseBeacon(t *testing.T) {
	for _, body := range []string{"", "<!-- warden-fleet {} -->", "<!-- warden-fleet {\"name\":1} -->", "<!-- warden-fleet not json -->"} {
		if _, ok := parseBeacon(body); ok {
			t.Errorf("%q parsed", body)
		}
	}
	b := Beacon{Name: "a --> b @x", Version: "v1.0.0", Channel: "canary"}
	got, ok := parseBeacon(beaconBody(b, time.Now()))
	if !ok || got.Name != b.Name || got.Channel != "canary" {
		t.Fatalf("%+v", got)
	}
	if body := beaconBody(b, time.Now()); strings.Contains(body, "@x") || strings.Count(body, "-->") != 1 {
		t.Fatal(body)
	}
}

func TestKubernetes(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	ns := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(ns, []byte("agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup := func(withDigest, grant bool) (*world, *[][]string) {
		w := newWorld(t, "install", func(c *Config) {
			c.Image, c.Deployment, c.GrantImageBump = "git.bayfut.net/nenkoru/warden", "warden", grant
		})
		w.u.K8s, w.u.NSFile = true, ns
		var extra map[string][]byte
		if withDigest {
			extra = map[string][]byte{"warden_0.3.0_image-digest.txt": []byte(digest + "\n")}
		}
		w.release("v0.3.0", false, 48*time.Hour, extra)
		var calls [][]string
		w.u.LookPath = func(n string) (string, error) {
			if n == "kubectl" {
				return "/usr/bin/kubectl", nil
			}
			return "", os.ErrNotExist
		}
		w.u.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		}
		return w, &calls
	}
	// propose: a digest published by the release (verified like the archive)
	w, calls := setup(true, false)
	fs := w.begin()
	if len(fs) != 1 || fs[0] != "update/low: update available: warden v0.3.0 — set image to git.bayfut.net/nenkoru/warden@"+digest {
		t.Fatal(fs)
	}
	if len(w.v.seen) != 2 || w.v.seen[1].Name != "warden_0.3.0_image-digest.txt" {
		t.Fatalf("%+v", w.v.seen)
	}
	_ = w.u.End(context.Background(), true)
	if len(*calls) != 0 || read(t, w.bin) != string(script("0.2.0")) || w.u.Restart {
		t.Fatal("touched the binary or the cluster without the grant", *calls)
	}
	if s := w.u.Status(); !strings.Contains(s, "not granted") {
		t.Fatal(s)
	}
	// granted: kubectl set image on its own Deployment, in its namespace
	w, calls = setup(true, true)
	w.begin()
	_ = w.u.End(context.Background(), true)
	want := "/usr/bin/kubectl -n agents set image deployment/warden warden=git.bayfut.net/nenkoru/warden@" + digest
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != want {
		t.Fatal(*calls)
	}
	if s := w.u.Status(); !strings.Contains(s, "image bumped") {
		t.Fatal(s)
	}
	// no digest published: the owner builds; the grant has nothing to set
	w, calls = setup(false, true)
	if fs := w.begin(); len(fs) != 1 || !strings.Contains(fs[0], "@sha256:… after building it") {
		t.Fatal(fs)
	}
	_ = w.u.End(context.Background(), true)
	if len(*calls) != 0 || !strings.Contains(w.u.Status(), "publishes no image digest") {
		t.Fatal(*calls, w.u.Status())
	}
	// a pending state never rolls a pod's binary back
	w, _ = setup(false, false)
	_ = saveState(w.u.path(), State{To: "v0.3.0", Pending: true, Attempts: 3})
	w.u.Current = "0.3.0"
	if fs := w.begin(); len(fs) != 0 {
		t.Fatal(fs)
	}
}
