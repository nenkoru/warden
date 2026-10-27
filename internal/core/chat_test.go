package core

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "привет" from the owner is not an order, but it gets an answer (with the
// state of things), not silence; the brain is not called in quiet hours.
func TestOwnerGreetingIsAnswered(t *testing.T) {
	d := filepath.Join(t.TempDir(), "quiet")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "domain.toml"), []byte("name = \"quiet\"\n[[observe]]\nname = \"none\"\nrun = { cmd = [\"true\"] }\n"), 0o644)
	inbox, out := t.TempDir(), filepath.Join(t.TempDir(), "replies")
	cfg := writeConfig(t, t.TempDir(), `name = "g"
domain = "`+d+`"
mode = "act"
replies = true
sandbox = "none" # replies are refused without a sandbox; this test is about the answer
[[signals]]
name = "chat"
kind = "dir"
dir = "`+inbox+`"
trust = "owner"
reply = ["sh", "-c", "printf '%s\\n' \"$1\" >> `+out+`", "reply", "{text}"]
`)
	in := open(t, cfg)
	calls := 0
	in.Brain = countBrain{word: "привет", calls: &calls}
	q, err := newQuietBrain(in.Brain, "0030-0800", "Etc/GMT-3", func() time.Time { return time.Date(2026, 10, 28, 1, 0, 0, 0, time.FixedZone("UTC+3", 3*3600)) })
	if err != nil {
		t.Fatal(err)
	}
	in.Brain = q
	_ = os.WriteFile(filepath.Join(inbox, "1.txt"), []byte("привет"), 0o644)
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "got it («привет») — not a request I act on. Now: all quiet") {
		t.Fatalf("reply: %q", b)
	}
	if calls != 0 {
		t.Fatal("the brain was called in quiet hours")
	}
	// a stranger gets nothing back
	in.Cfg.Signals[0].Trust = "anyone"
	_ = os.WriteFile(filepath.Join(inbox, "2.txt"), []byte("hi there"), 0o644)
	_, _ = in.Tick(context.Background())
	if b2, _ := os.ReadFile(out); string(b2) != string(b) {
		t.Fatalf("a stranger got an answer: %q", b2)
	}
}

func TestQuietBrainWindowAndChatStreams(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*3600)
	for _, c := range []struct {
		h, m  int
		quiet bool
	}{{0, 29, false}, {0, 30, true}, {7, 59, true}, {8, 0, false}, {21, 0, false}} {
		q, _ := newQuietBrain(nil, "0030-0800", "Etc/GMT-3", func() time.Time { return time.Date(2026, 10, 28, c.h, c.m, 0, 0, zone) })
		if q.(quietBrain).quiet() != c.quiet {
			t.Errorf("%02d:%02d quiet=%v", c.h, c.m, !c.quiet)
		}
	}
	q, _ := newQuietBrain(nil, "2300-0200", "", func() time.Time { return time.Date(2026, 10, 28, 1, 0, 0, 0, time.Local) })
	if !q.(quietBrain).quiet() {
		t.Error("a window across midnight")
	}
	if _, err := newQuietBrain(nil, "25:00-8", "", time.Now); err == nil {
		t.Error("a bad window was accepted")
	}
	logs, chat := &SignalSource{Name: "app"}, &SignalSource{Name: "chat", Chat: true}
	if keep(logs, "привет") || !keep(logs, "panic: boom") || !keep(chat, "привет") || keep(chat, "  ") {
		t.Error("stream filter")
	}
}

// A line in a chat stream wakes a tick within seconds (log streams wait 20 s to batch).
func TestListenChatWakesFast(t *testing.T) {
	in := filepath.Join(t.TempDir(), "chat.in")
	_ = os.WriteFile(in, nil, 0o644)
	cfg := writeConfig(t, t.TempDir(), `name = "c"
domain = "`+filepath.Join(packs(t), "go-project")+`"
[[signals]]
name = "chat"
kind = "stream"
argv = ["tail", "-n0", "-F", "`+in+`"]
chat = true
trust = "owner"
`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticks := make(chan time.Time, 4)
	go Listen(ctx, cfg, nil, func(context.Context) { ticks <- time.Now() }, io.Discard)
	<-ticks // the first tick at start
	time.Sleep(time.Second)
	f, _ := os.OpenFile(in, os.O_APPEND|os.O_WRONLY, 0)
	start := time.Now()
	_, _ = f.WriteString("привет\n")
	f.Close()
	select {
	case at := <-ticks:
		if d := at.Sub(start); d > 8*time.Second {
			t.Fatalf("the chat line ticked after %v", d)
		}
	case <-ctx.Done():
		t.Fatal("no tick for the chat line")
	}
}
