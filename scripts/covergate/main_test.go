package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const funcOut = "github.com/nenkoru/warden/internal/domain/domain.go:120:\tLoad\t\t85.7%\n" +
	"github.com/nenkoru/warden/internal/domain/domain.go:141:\tvalidate\t100.0%\n" +
	"total:\t\t\t\t\t\t\t(statements)\t\t71.4%\n"

func TestParseTotal(t *testing.T) {
	v, err := parseTotal(strings.NewReader(funcOut))
	if err != nil || v != 71.4 {
		t.Fatal(v, err)
	}
	for _, bad := range []string{"", "a.go:1:\tf\t10.0%\n", "total:\t(statements)\tx%\n", "total:\t(statements)\t101.0%\n", "total:\t(statements)\t50\n"} {
		if _, err := parseTotal(strings.NewReader(bad)); err == nil {
			t.Fatalf("%q: want an error", bad)
		}
	}
}

func TestCompare(t *testing.T) {
	base := &Record{Total: 71.4, SHA: "0123456789abcdef"}
	for _, c := range []struct {
		cur float64
		ok  bool
	}{{71.4, true}, {75, true}, {71.3, true}, {71.29, false}, {60, false}} {
		ok, msg := compare(base, c.cur, 0.1)
		if ok != c.ok {
			t.Fatalf("%v: %v %s", c.cur, ok, msg)
		}
		if !ok && !strings.Contains(msg, "0123456") {
			t.Fatal(msg)
		}
	}
	if ok, msg := compare(nil, 10, 0.1); !ok || !strings.Contains(msg, "no baseline") {
		t.Fatal(msg)
	}
}

func TestRunRecordsAndGates(t *testing.T) {
	dir := t.TempDir()
	p := func(n string) string { return filepath.Join(dir, n) }
	if err := os.WriteFile(p("cover.txt"), []byte(funcOut), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"-func", p("cover.txt"), "-sha", "abc", "-out", p("coverage.json")}, &out); code != 0 {
		t.Fatal(code, out.String())
	}
	if b, _ := os.ReadFile(p("coverage.json")); strings.TrimSpace(string(b)) != `{"total":71.4,"sha":"abc"}` {
		t.Fatalf("%s", b)
	}
	out.Reset()
	if code := run([]string{"-func", p("cover.txt"), "-base", p("missing.json")}, &out); code != 0 || !strings.Contains(out.String(), "::notice::") {
		t.Fatal(code, out.String())
	}
	if err := os.WriteFile(p("base.json"), []byte(`{"total":80,"sha":"def"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"-func", p("cover.txt"), "-base", p("base.json")}, &out); code != 1 || !strings.Contains(out.String(), "::error::coverage dropped") {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := run([]string{"-func", p("cover.txt"), "-base", p("base.json"), "-max-drop", "10"}, &out); code != 0 {
		t.Fatal(code, out.String())
	}
	if code := run([]string{"-func", p("cover.txt")}, &out); code != 2 {
		t.Fatal(code)
	}
	if err := os.WriteFile(p("bad.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-func", p("cover.txt"), "-base", p("bad.json")}, &out); code != 1 {
		t.Fatal(code)
	}
}
