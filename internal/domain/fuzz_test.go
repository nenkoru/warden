package domain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzz targets: the parsers that read what a pack author or a command
// writes. Each asserts no panic and a cheap invariant.

func FuzzManifest(f *testing.F) {
	packs, _ := filepath.Glob(filepath.Join("..", "..", "packs", "*", "domain.toml"))
	for _, p := range packs {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("name = \"x\"\n[[observe]]\nname = \"o\"\nrun = { cmd = [\"true\"] }\nfix = \"a\"\n[[act]]\nname = \"a\"\nrun = { builtin = \"propose\" }\n"))
	f.Add([]byte("name = \"x\"\n[[observe]]\nname = \"o\"\nrun = { mcp = \"m\", tool = \"t\" }\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := decode(b)
		if err != nil {
			return
		}
		if d.validate() == nil && d.Name == "" {
			t.Fatal("a valid manifest has a name")
		}
	})
}

func FuzzRecords(f *testing.F) {
	f.Add("", "items[]", []byte(events))
	f.Add("", "items[].spec.containers[]", []byte(`{"items":[{"spec":{"containers":[{"image":"nginx:1"}]}}]}`))
	f.Add(`^(?P<name>\S+)\s+(?P<version>\S+)$`, "", []byte("go 1.22\nnode 20.1\n"))
	f.Add("", "", []byte(`[1, "a", null, {"x": 2}]`))
	f.Add("", "[]", []byte(`{}`))
	f.Fuzz(func(t *testing.T, lines, items string, raw []byte) {
		recs, err := Map{Lines: lines, Items: items, Where: map[string]string{}}.Records(raw)
		if err != nil {
			return
		}
		for _, r := range recs {
			if r == nil {
				t.Fatal("a nil record")
			}
			if _, ok := r["line"]; lines != "" && !ok {
				t.Fatalf("a text record without its line: %v", r)
			}
		}
	})
}

func FuzzFindings(f *testing.F) {
	f.Add([]byte("high: disk full\nlow: noise\nhigh: disk full\n"), "{msg}", "", false)
	f.Add([]byte(`{"id":"a","kind":"k","severity":"high","subject":"s","title":"t"}`+"\n\n"+`{"title":"u"}`), "", "", true)
	f.Add([]byte("x: y"), "{msg} {nope}", "{sev}", false)
	f.Fuzz(func(t *testing.T, raw []byte, title, key string, native bool) {
		step := Step{Name: "s", Fix: "a"}
		var m *Map
		if !native {
			m = &Map{Lines: `^(?P<sev>\w+):\s*(?P<msg>.*)$`, Title: title, Key: key, Severity: "{sev}"}
		}
		fs, err := m.Findings(raw, step, map[string]string{"p": "v"})
		if err != nil {
			return
		}
		ids := map[string]bool{}
		for _, x := range fs {
			if _, ok := Severities[x.Severity]; !ok {
				t.Fatalf("severity %q", x.Severity)
			}
			if x.ID != ID(step.Name, x.Key) {
				t.Fatalf("id %s of key %q", x.ID, x.Key)
			}
			if m != nil {
				if ids[x.ID] || x.Count < 1 || x.Fix == nil {
					t.Fatalf("mapped findings: one per key, counted, with the step's fix: %+v", x)
				}
				ids[x.ID] = true
			}
		}
	})
}

func FuzzParseComponent(f *testing.F) {
	for _, s := range []string{"nginx:1.25.3", "ghcr.io/org/app:v2@sha256:abc", "golang.org/x/text@v0.3.7", "requests==2.31.0", "php 8.3.12",
		"localhost:5000/tool", "@scope/pkg@1.0.0", " ", ":x", "@sha256:"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := ParseComponent(s)
		if len(c.Name)+len(c.Version) > len(s) {
			t.Fatalf("%q → %+v: longer than the input", s, c)
		}
		if strings.TrimSpace(s) != "" && c.Name == "" {
			t.Fatalf("%q → %+v: no name", s, c)
		}
	})
}

func FuzzFill(f *testing.F) {
	f.Add("{reason}: {involvedObject.kind}/{involvedObject.name} in {p}", []byte(`{"reason":"BackOff","involvedObject":{"kind":"Pod","name":"a"}}`))
	f.Add(" plain ", []byte(`{}`))
	f.Add("{n} {f} {b} {a}", []byte(`{"n":null,"f":1.5,"b":true,"a":[1,{"x":2}]}`))
	f.Fuzz(func(t *testing.T, tpl string, rec []byte) {
		var r map[string]any
		_ = json.Unmarshal(rec, &r)
		out := Fill(tpl, r, map[string]string{"p": "v"})
		if out != strings.TrimSpace(out) {
			t.Fatalf("%q: not trimmed", out)
		}
		if !strings.Contains(tpl, "{") && out != strings.TrimSpace(tpl) {
			t.Fatalf("%q → %q: a template without placeholders changed", tpl, out)
		}
	})
}
