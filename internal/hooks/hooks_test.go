package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sh(event, matcher, script string, args ...string) Hook {
	return Hook{Event: event, Matcher: matcher, Command: append([]string{"sh", "-c", script, "hook"}, args...)}
}

type finding struct {
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

func TestPayloadAndSubstitution(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	hs := []Hook{sh("Report", "report|ask", `cat > "$1"; echo "$2|$3|$4|$5" >> "$1"`, out, "{title}", "{event}", "{subject}", "{count}")}
	o := Fire(context.Background(), hs, "Report", "report", finding{Title: "disk low", Severity: "high", Count: 2})
	if o.Block || len(o.Errors) > 0 || len(o.Ran) != 1 {
		t.Fatalf("outcome = %+v", o)
	}
	b, _ := os.ReadFile(out)
	got := string(b)
	if !strings.Contains(got, `"title":"disk low"`) || !strings.HasSuffix(got, "disk low|Report|report|{count}\n") {
		t.Fatalf("got %q", got)
	}
}

func TestMatcherAndEvent(t *testing.T) {
	hs := []Hook{
		sh("PreAction", "^bump$", "true"),
		sh("PreAction", "", "true"),
		sh("PostAction", "", "true"),
		sh("PreAction", "(", "true"),
	}
	o := Fire(context.Background(), hs, "PreAction", "clean", nil)
	if len(o.Ran) != 1 || len(o.Errors) != 1 || !strings.Contains(o.Errors[0], "matcher") {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestBlockExit2(t *testing.T) {
	o := Fire(context.Background(), []Hook{sh("PreAction", "", "echo 'not on fridays' >&2; exit 2")}, "PreAction", "apply", nil)
	if !o.Block || o.Reason != "not on fridays" || len(o.Errors) != 0 {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestBlockJSON(t *testing.T) {
	hs := []Hook{
		sh("PreAction", "", `echo '{"decision":"block","reason":"frozen"}'`),
		sh("PreAction", "", `echo '{"decision":"approve"}'`),
	}
	o := Fire(context.Background(), hs, "PreAction", "apply", nil)
	if !o.Block || o.Reason != "frozen" || len(o.Ran) != 2 {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestErrors(t *testing.T) {
	hs := []Hook{
		sh("Ask", "", "echo boom >&2; exit 1"),
		{Event: "Ask", Command: []string{"sh", "-c", "sleep 5"}, Timeout: "100ms", From: "plugin-x"},
		{Event: "Ask", Command: []string{"true"}, Timeout: "soon"},
		{Event: "Ask", Command: []string{"/nonexistent/warden-hook"}},
	}
	start := time.Now()
	o := Fire(context.Background(), hs, "Ask", "x", map[string]string{"a": "b"})
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
	if o.Block || len(o.Errors) != 4 {
		t.Fatalf("outcome = %+v", o)
	}
	if !strings.Contains(o.Errors[0], "boom") || !strings.Contains(o.Errors[1], "plugin-x: ") || !strings.Contains(o.Errors[1], "timed out") || !strings.Contains(o.Errors[2], "bad timeout") {
		t.Fatalf("errors = %q", o.Errors)
	}
}
