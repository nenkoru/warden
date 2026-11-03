package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/brain"
)

const goFile = "package proj\n\nfunc Hello() string {\n\treturn \"hello\"\n}\n\nfunc Bye() string {\n\treturn \"bye\"\n}\n"

func TestEdit(t *testing.T) {
	for _, c := range []struct {
		name, content, old, new string
		all                     bool
		want, how, err          string
	}{
		{name: "exact", content: goFile, old: "\treturn \"bye\"", new: "\treturn \"goodbye\"",
			want: strings.Replace(goFile, `"bye"`, `"goodbye"`, 1), how: "exact"},
		{name: "twice", content: goFile, old: "() string {", new: "() string { //", err: "occurs 2 times (lines 3, 7)"},
		{name: "all", content: goFile, old: "() string {", new: "() (s string) {", all: true,
			want: strings.ReplaceAll(goFile, "() string {", "() (s string) {"), how: "exact"},
		// spaces where the file has a tab: matched, and the new text gets the tab
		{name: "indent", content: goFile, old: "func Bye() string {\n    return \"bye\"\n}", new: "func Bye() string {\n    return \"see you\"\n}",
			want: strings.Replace(goFile, `"bye"`, `"see you"`, 1), how: "normalized"},
		{name: "quotes", content: "msg := \"it's\"\n", old: "msg := \"it’s\"", new: "msg := \"it is\"", want: "msg := \"it is\"\n", how: "normalized"},
		{name: "line numbers", content: goFile, old: "8:\treturn \"bye\"\n9:}", new: "\treturn \"ciao\"\n}",
			want: strings.Replace(goFile, `"bye"`, `"ciao"`, 1), how: "exact"},
		{name: "fuzzy", content: "x := computeTheLongValueOfSomethingImportant(alpha, beta, gamma, delta)\n",
			old: "x := computeTheLongValueOfSomethingImportant(alpha, beta, gama, delta)", new: "x := 0",
			want: "x := 0\n", how: "fuzzy 98%"},
		{name: "near miss", content: goFile, old: "func Bye() string {\n\treturn \"farewell\"\n}", new: "x",
			err: "is lines 7-9, exactly:\n7:func Bye() string {\n8:\treturn \"bye\"\n9:}"},
		{name: "nothing near", content: goFile, old: "type Server struct{}", new: "x", err: "nor anything close"},
		{name: "empty", content: goFile, old: "", new: "x", err: "old_string is empty"},
	} {
		got, how, err := edit(c.content, c.old, c.new, c.all)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want || how != c.how {
			t.Errorf("%s: got %q (%s, %v), want %q (%s)", c.name, got, how, err, c.want, c.how)
		}
	}
}

func TestReadWindow(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	text := b.String()
	out := readWindow(text, 1, readLines)
	if !strings.HasPrefix(out, "line 1\n") || !strings.Contains(out, "line 300\n") || strings.Contains(out, "line 301\n") ||
		!strings.HasSuffix(out, "[lines 1-300 of 500. Use offset=301 to continue.]") {
		t.Errorf("first window: …%s", tail(out, 120))
	}
	if out := readWindow(text, 490, 50); !strings.HasPrefix(out, "line 490\n") || !strings.HasSuffix(out, "line 500\n") {
		t.Errorf("the end: %q", out)
	}
	if out := readWindow(text, 900, 10); !strings.Contains(out, "past the end of the file (500 lines)") {
		t.Errorf("past the end: %q", out)
	}
	if out := readWindow(strings.Repeat("x", 600)+"\n", 1, 10); !strings.Contains(out, "…(line cut)") {
		t.Errorf("a long line: %q", out)
	}
}

// The coder's tools, called the way local models call them: other
// harnesses' names and argument names, a search before a read.
func TestFileTools(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", goFile)
	write("internal/x/x.go", "package x\n// Hello is used here too\n")
	write("internal/x/x_test.go", "package x\n")
	write(".git", "gitdir: elsewhere\n")
	write("bin.dat", "Hello\x00binary")
	tools := fileTools(root, &Env{})
	run := func(name string, args map[string]any) (string, bool) {
		tl := brain.Find(tools, name)
		if tl == nil {
			t.Fatalf("no tool %s", name)
		}
		return tl.Run(context.Background(), args)
	}
	if out, ok := run("grep", map[string]any{"pattern": "Hello"}); !ok || out != "a.go:3:func Hello() string {\ninternal/x/x.go:2:// Hello is used here too" {
		t.Errorf("grep: %q", out)
	}
	if out, _ := run("search", map[string]any{"query": "hello(", "ignore_case": true, "glob": "*.go"}); !strings.HasPrefix(out, "(not a valid RE2 pattern") ||
		!strings.Contains(out, "a.go:3:") {
		t.Errorf("grep, a bad pattern: %q", out)
	}
	if out, _ := run("grep", map[string]any{"pattern": "Hello", "path": "internal"}); out != "internal/x/x.go:2:// Hello is used here too" {
		t.Errorf("grep under a path: %q", out)
	}
	if out, _ := run("find", map[string]any{"pattern": "**/*_test.go"}); out != "internal/x/x_test.go" {
		t.Errorf("find: %q", out)
	}
	if out, _ := run("list_files", nil); strings.Contains(out, ".git") || !strings.Contains(out, "internal/x/x.go") {
		t.Errorf("find all: %q", out)
	}
	if out, ok := run("read_file", map[string]any{"file_path": "a.go", "offset": "3", "limit": 2.0}); !ok ||
		out != "func Hello() string {\n\treturn \"hello\"\n\n[lines 3-4 of 9. Use offset=5 to continue.]" {
		t.Errorf("read: %q", out)
	}
	if out, _ := run("read", map[string]any{"path": "internal"}); out != "(a directory) x/" {
		t.Errorf("read a directory: %q", out)
	}
	if out, ok := run("read", map[string]any{"path": "nope.go"}); ok || strings.Contains(out, root) {
		t.Errorf("read a missing file: %q", out)
	}
	if out, ok := run("str_replace", map[string]any{"file_path": "a.go", "old_string": "\treturn \"bye\"", "new_string": "\treturn \"ciao\""}); !ok || out != "edited a.go; it has 10 lines now" {
		t.Errorf("edit: %q", out)
	}
	if out, ok := run("replace_in_file", map[string]any{"path": "a.go", "old": "func Hello() string {\n\treturn \"howdy\"\n}", "new": "x"}); ok ||
		!strings.Contains(out, "3:func Hello() string {") {
		t.Errorf("edit near miss: %q", out)
	}
	if out, ok := run("edit", map[string]any{"path": "new.go", "old_string": "a", "new_string": "b"}); ok || !strings.Contains(out, "use write") {
		t.Errorf("edit a missing file: %q", out)
	}
	if out, ok := run("create_file", map[string]any{"path": "internal/y/y.go", "content": "package y\n"}); !ok || out != "wrote internal/y/y.go (2 lines)" {
		t.Errorf("write: %q", out)
	}
	if _, ok := run("edit", map[string]any{"path": ".git/config", "old_string": "a", "new_string": "b"}); ok {
		t.Error("edit .git")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.go")); !strings.Contains(string(b), "ciao") {
		t.Errorf("a.go: %s", b)
	}
}

func TestIssueTaskLayout(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"cmd/warden", "internal/brain/deep/er", ".github/workflows", "node_modules/x"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	got := layout(root)
	if got != "\n\nThe repository's directories:\ncmd/\ncmd/warden/\ninternal/\ninternal/brain/" {
		t.Errorf("layout: %q", got)
	}
}

// A brain that gets stuck (out of turns, in a loop) on an issue is given a
// second run on a later tick; after that the issue waits for the owner
// instead of burning a run every tick.
func TestWorkIssueStuckTwice(t *testing.T) {
	w := newWorld(t)
	w.fake.AddIssue("Add a Goodbye function", "Please add Goodbye().", "stranger", "NONE", "warden", "small")
	stuck := stuckBrain{}
	w.env.Brains = brains(&stuck, nil)
	f := fixOf(w.observe("github-issues"), "work-issue")
	if f == nil {
		t.Fatal("no work-issue fix")
	}
	r := Act(context.Background(), "work-issue", nil, *f, w.env)
	if !r.Deferred || !strings.Contains(r.Summary, "max_steps") {
		t.Fatalf("first: %+v", r)
	}
	r = Act(context.Background(), "work-issue", nil, *f, w.env)
	if r.Deferred || r.OK || !strings.Contains(r.Summary, "got stuck 2 times today") || !strings.Contains(r.Summary, "max_steps") {
		t.Fatalf("second: %+v", r)
	}
}

type stuckBrain struct{}

func (stuckBrain) Do(context.Context, string, string, []brain.Tool) (string, error) {
	return "", fmt.Errorf("stopped after 50 steps (max_steps): %w", brain.ErrStuck)
}
