package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackoffPause(t *testing.T) {
	cfg := &Config{Wake: Wake{Every: "1h"}, Backoff: Backoff{After: 1, Max: "3h"}, State: t.TempDir()}
	if err := cfg.Backoff.normalize(); err != nil {
		t.Fatal(err)
	}
	in := &Instance{Cfg: cfg, St: &State{}}
	t0 := time.Unix(1_800_000_000, 0)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	may := func(m int) bool { ok, _ := in.mayPing(at(m), false); return ok }

	if !may(0) {
		t.Fatal("the first ping goes")
	}
	in.pinged(at(0)) // 1 ignored ≥ after: pauses 1h, 2h, 3h (max), 3h …
	for _, c := range []struct {
		m    int
		want bool
	}{{30, false}, {60, true}} {
		if may(c.m) != c.want {
			t.Fatalf("at +%dm: want %v", c.m, c.want)
		}
	}
	in.pinged(at(60))
	if may(60+119) || !may(60+120) {
		t.Fatal("the second pause is 2h")
	}
	in.pinged(at(180))
	if may(180+179) || !may(180+180) {
		t.Fatal("the pause stops at max")
	}
	if ok, _ := in.mayPing(at(181), true); !ok {
		t.Fatal("critical never waits")
	}
	_ = Touch(cfg.State, at(200))
	if !may(201) {
		t.Fatal("the owner looked: pings may go again")
	}
	_ = writeJSON(filepath.Join(cfg.State, "pings.json"), in.St.Pings)
	if !strings.Contains(backoffLine(cfg), "since you last looked") {
		t.Error(backoffLine(cfg))
	}
}

// End to end: news every tick, a Report hook as the ping. Two pings go,
// the third tick is held (report written, news kept new, no hook), the
// owner looks, and the next tick pings again.
func TestBackoffHoldsPings(t *testing.T) {
	d := filepath.Join(t.TempDir(), "news")
	_ = os.MkdirAll(d, 0o755)
	n := filepath.Join(t.TempDir(), "n")
	_ = os.WriteFile(filepath.Join(d, "domain.toml"), []byte(`name = "news"
[[observe]]
name = "flag"
run = { cmd = ["sh", "-c", "n=$(cat `+n+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+n+`; echo '{\"key\":\"flag-'$n'\",\"kind\":\"health\",\"severity\":\"medium\",\"subject\":\"x\",\"title\":\"something new\"}'"] }
`), 0o644)
	pings := filepath.Join(t.TempDir(), "pings")
	cfg := writeConfig(t, t.TempDir(), `name = "n"
domain = "`+d+`"
mode = "act"
wake = { every = "1h" }
sandbox = "none" # the hook is the ping; this test is about the backoff, not the sandbox
[[hooks]]
event = "Report"
command = ["sh", "-c", "echo ping >> `+pings+`"]
`)
	in := open(t, cfg)
	t0 := time.Unix(1_800_000_000, 0)
	cur := t0
	in.Now = func() time.Time { return cur }
	tick := func(m int) *Tick {
		cur = t0.Add(time.Duration(m) * time.Minute)
		tk, err := in.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	count := func() int { b, _ := os.ReadFile(pings); return strings.Count(string(b), "ping") }

	tick(0)
	tick(1)
	if count() != 2 {
		t.Fatalf("pings: %d", count())
	}
	t3 := tick(2)
	if count() != 2 || t3.Held == "" || t3.Report == "" {
		t.Fatalf("third tick must be held: pings %d, held %q, report %q", count(), t3.Held, t3.Report)
	}
	if rep, _ := os.ReadFile(t3.Report); !strings.Contains(string(rep), "ping held") {
		t.Fatal(string(rep))
	}
	for _, it := range t3.Items {
		if s := in.St.Seen[it.ID]; s != nil && s.Reported != 0 {
			t.Fatal("held news was marked as told")
		}
	}
	if in.St.Pings.Held != 1 {
		t.Fatalf("held %d", in.St.Pings.Held)
	}
	_ = Touch(cfg.State, t0.Add(3*time.Minute))
	tick(4)
	if count() != 3 {
		t.Fatalf("after the owner looked: pings %d", count())
	}
}
