package meter

import (
	"bufio"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}
func (c *clock) Add(d time.Duration) { c.Set(c.Now().Add(d)) }

// usd: one input token of "m" costs $1/1M, so Usage{Input: usd(x)} costs $x.
func usd(x float64) Usage { return Usage{Input: int64(math.Round(x * 1e6))} }

var oneDollar = Prices{"m": {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}}

func open(t *testing.T, cfg Config) *Ledger {
	t.Helper()
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func kinds(as []Alert) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Kind)
	}
	return out
}

func TestPrices(t *testing.T) {
	ps := DefaultPrices()
	u := Usage{Input: 1000, Output: 2000, CacheRead: 3000, CacheWrite: 4000}
	if got := ps.Lookup("claude-opus-5-5").Cost(u); !near(got, (1000*4+2000*20+3000*0.4+4000*5)/1e6) {
		t.Errorf("opus cost = %v", got)
	}
	if got := ps.Lookup("claude-haiku-5-5").Cost(Usage{CacheRead: 1e6, CacheWrite: 1e6}); !near(got, 0.135) {
		t.Errorf("haiku cache cost = %v", got)
	}
	if got := ps.Lookup("claude-sonnet-5-5-20261001"); got != ps["claude-sonnet-5-5"] {
		t.Errorf("prefix lookup = %v", got)
	}
	if got := ps.Lookup("claude-opus-5-5[1m]"); got != ps["claude-opus-5-5"] {
		t.Errorf("prefix lookup = %v", got)
	}
	for _, m := range []string{"glm-5", "", "claude-opus-9"} {
		if got := ps.Lookup(m); got != ps["claude-fable-5-1"] {
			t.Errorf("unknown %q = %v, want the most expensive", m, got)
		}
	}
	l := open(t, Config{StateDir: t.TempDir()})
	if got := l.Estimate("claude-opus-5-5", 1000, 4000); !near(got, (1000*20+1000*4)/1e6) {
		t.Errorf("estimate = %v", got)
	}
}

func TestThresholdsAndDayRollover(t *testing.T) {
	tz := time.FixedZone("X", 3*3600)
	clk := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, tz)}
	l := open(t, Config{PerDayUSD: 1000, AlertEveryUSD: 100, Prices: oneDollar, StateDir: t.TempDir(), Now: clk.Now, TZ: tz})
	steps := []struct {
		cost float64
		want []string // threshold texts
	}{
		{60, nil}, {60, []string{"$100"}}, {60, nil}, {70, []string{"$200"}}, {200, []string{"$300", "$400"}}, {0.5, nil},
	}
	for i, s := range steps {
		cost, alerts := l.Record("m", usd(s.cost))
		if !near(cost, s.cost) {
			t.Fatalf("step %d: cost %v", i, cost)
		}
		if len(alerts) != len(s.want) {
			t.Fatalf("step %d: alerts %+v, want %v", i, alerts, s.want)
		}
		for j, a := range alerts {
			if a.Kind != "threshold" || !strings.Contains(a.Text, s.want[j]+" spent") {
				t.Errorf("step %d: alert %+v, want %s", i, a, s.want[j])
			}
		}
	}
	if got := l.Alerts(); len(got) != 4 {
		t.Errorf("pending = %v", kinds(got))
	}
	if got := l.Alerts(); len(got) != 0 {
		t.Errorf("pending not cleared: %v", kinds(got))
	}
	// 23:59 → 00:01 in TZ: still the same UTC date, a new day here
	clk.Set(time.Date(2026, 10, 27, 23, 59, 0, 0, tz))
	if today, _ := l.Spent(); !near(today, 450.5) {
		t.Errorf("today = %v", today)
	}
	clk.Set(time.Date(2026, 10, 28, 0, 1, 0, 0, tz))
	if today, _ := l.Spent(); today != 0 {
		t.Errorf("after rollover today = %v", today)
	}
	if _, alerts := l.Record("m", usd(120)); len(alerts) != 1 || !strings.Contains(alerts[0].Text, "$100 spent") {
		t.Errorf("new day alerts = %+v", alerts)
	}
}

func TestBurn(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)}
	l := open(t, Config{BurnPerHourUSD: 100, Prices: oneDollar, StateDir: t.TempDir(), Now: clk.Now, TZ: time.UTC})
	expect := func(cost float64, after time.Duration, want int, lastHour float64) {
		t.Helper()
		clk.Add(after)
		_, alerts := l.Record("m", usd(cost))
		if len(alerts) != want || (want == 1 && alerts[0].Kind != "burn") {
			t.Fatalf("alerts = %+v, want %d burn", alerts, want)
		}
		if _, h := l.Spent(); !near(h, lastHour) {
			t.Fatalf("last hour = %v, want %v", h, lastHour)
		}
	}
	expect(60, 0, 0, 60)
	expect(50, 10*time.Minute, 1, 110) // over $100/h
	expect(10, 10*time.Minute, 0, 120) // still over, but alerted 10 min ago
	expect(1, 25*time.Minute, 1, 121)  // 35 min after the last alert
	expect(1, 85*time.Minute, 0, 1)    // the window moved on
	expect(150, 1*time.Minute, 1, 151) // a single runaway call
}

func TestCap(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)}
	l := open(t, Config{PerDayUSD: 1000, Prices: oneDollar, StateDir: t.TempDir(), Now: clk.Now})
	if _, alerts := l.Record("m", usd(990)); len(alerts) != 0 {
		t.Errorf("alerts = %+v", alerts)
	}
	if err := l.Allow(5); err != nil {
		t.Errorf("Allow(5) = %v", err)
	}
	if err := l.Allow(20); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Allow(20) = %v", err)
	}
	if _, alerts := l.Record("m", usd(15)); len(alerts) != 1 || alerts[0].Kind != "cap" || !near(alerts[0].SpentToday, 1005) {
		t.Errorf("alerts = %+v", alerts)
	}
	if _, alerts := l.Record("m", usd(1)); len(alerts) != 0 {
		t.Errorf("cap alerted twice: %+v", alerts)
	}
	if err := l.Allow(0); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Allow(0) at the cap = %v", err)
	}
	clk.Add(24 * time.Hour)
	if err := l.Allow(500); err != nil {
		t.Errorf("next day Allow = %v", err)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)}
	cfg := Config{PerDayUSD: 1000, AlertEveryUSD: 100, BurnPerHourUSD: 100, Prices: oneDollar, StateDir: dir, Now: clk.Now}
	l := open(t, cfg)
	l.Record("m", usd(70))
	l.Record("m", usd(80)) // $100 threshold, burn
	l2 := open(t, cfg)
	if today, hour := l2.Spent(); !near(today, 150) || !near(hour, 150) {
		t.Errorf("reopened: today %v hour %v", today, hour)
	}
	if _, alerts := l2.Record("m", usd(10)); len(alerts) != 0 { // nothing re-alerted
		t.Errorf("alerts after reopen = %+v", alerts)
	}
	if _, alerts := l2.Record("m", usd(50)); len(alerts) != 1 || alerts[0].Kind != "threshold" {
		t.Errorf("alerts = %+v", alerts)
	}
	f, err := os.Open(filepath.Join(dir, "meter.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
	}
	if n != 4 {
		t.Errorf("meter.jsonl has %d records, want 4", n)
	}
	if fi, err := os.Stat(filepath.Join(dir, "meter.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("meter.json: %v %v", fi, err)
	}
}

func TestConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AlertEveryUSD: 10, Prices: oneDollar, StateDir: dir}
	a, b := open(t, cfg), open(t, cfg) // two ledgers on one dir: the file lock keeps them consistent
	var wg sync.WaitGroup
	for i := range 100 {
		l := a
		if i%2 == 1 {
			l = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Record("m", usd(1))
		}()
	}
	wg.Wait()
	for _, l := range []*Ledger{a, b} {
		if today, _ := l.Spent(); !near(today, 100) {
			t.Errorf("today = %v, want 100", today)
		}
	}
	if n := len(a.Alerts()) + len(b.Alerts()); n != 10 {
		t.Errorf("threshold alerts = %d, want 10 (once per $10)", n)
	}
}

func TestOpenNeedsDir(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Error("Open without StateDir succeeded")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meter.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{StateDir: dir}); err == nil {
		t.Error("Open over a corrupt ledger succeeded")
	}
}
