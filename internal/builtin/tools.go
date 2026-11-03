package builtin

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nenkoru/warden/internal/brain"
)

// fileTools: the brain's tools for a worktree, confined to it. Shaped after
// pi / oh-my-pi, where the same local model works well: few tools, reads in
// windows, a search instead of whole-file reads, exact-text edits whose
// misses show the closest text. A call by another harness's name or
// argument names (read_file, file_path, old_string, …) works too.
func fileTools(root string, env *Env) []brain.Tool {
	obj := func(req []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	boolean := func(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
	path := str("File path, relative to the repository root.")
	return []brain.Tool{
		{Name: "read", Aliases: []string{"read_file", "view", "cat", "open_file"},
			Description: fmt.Sprintf("Read a text file. Shows up to %d lines from offset; the end of the output says how to continue. "+
				"Use grep first to find the lines you need, then read that range.", readLines),
			Schema: obj([]string{"path"}, map[string]any{"path": path, "offset": num("First line to show, from 1 (default 1)."),
				"limit": num(fmt.Sprintf("How many lines (default %d, at most %d).", readLines, readMaxLines))}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				p, err := confine(root, argStr(args, "path", "file_path", "file", "filename"))
				if err != nil {
					return err.Error(), false
				}
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					return listDir(root, p), true // a directory: its entries
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return relErr(root, err), false
				}
				return readWindow(string(b), argInt(args, 1, "offset", "start_line", "line"), argInt(args, readLines, "limit", "lines", "count")), true
			}},
		{Name: "grep", Aliases: []string{"search", "search_files", "grep_search", "rg", "find_in_files"},
			Description: fmt.Sprintf("Search file contents with a regular expression (Go RE2 syntax). Prints path:line:text, at most %d matches. "+
				"The way to find where something is defined or used.", grepMax),
			Schema: obj([]string{"pattern"}, map[string]any{"pattern": str("Regular expression, or literal text with literal=true."),
				"path": str("File or directory to search (default: the whole repository)."), "glob": str("Only files whose name matches, e.g. *.go."),
				"ignore_case": boolean("Case-insensitive."), "literal": boolean("Treat pattern as plain text.")}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				return grepTool(root, args)
			}},
		{Name: "find", Aliases: []string{"list_files", "ls", "glob", "list_dir", "list_directory", "find_files"},
			Description: fmt.Sprintf("List files whose path matches a glob (** matches any directories), at most %d. Without a pattern: the files under path.", findMax),
			Schema: obj(nil, map[string]any{"pattern": str("Glob, e.g. **/*_test.go or internal/brain/*.go (default: every file)."),
				"path": str("Directory to look in (default: the repository root).")}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				return findTool(root, args)
			}},
		{Name: "edit", Aliases: []string{"replace_in_file", "str_replace", "str_replace_editor", "edit_file", "replace", "apply_edit"},
			Description: "Replace text in a file. old_string must be copied exactly from the file (without line numbers) and be unique in it: " +
				"include a few surrounding lines. Keep it short. Several changes to one file: one call each. If it is not found, " +
				"the error shows the closest lines; copy from them.",
			Schema: obj([]string{"path", "old_string", "new_string"}, map[string]any{"path": path,
				"old_string": str("The exact text to replace."), "new_string": str("The text to put in its place."),
				"replace_all": boolean("Replace every occurrence (default: exactly one).")}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				rel := argStr(args, "path", "file_path", "file", "filename")
				p, err := confineWrite(root, rel)
				if err != nil {
					return err.Error(), false
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return relErr(root, err) + " (to create a file, use write)", false
				}
				out, how, err := edit(string(b), argStr(args, "old_string", "old", "oldText", "old_text", "search", "find"),
					argStr(args, "new_string", "new", "newText", "new_text", "replace", "replacement"), argBool(args, "replace_all"))
				if err != nil {
					return rel + ": " + err.Error(), false
				}
				if out == string(b) {
					return rel + ": the edit changes nothing (new_string equals the text it replaces)", false
				}
				st, _ := os.Stat(p)
				if err := os.WriteFile(p, []byte(out), st.Mode().Perm()); err != nil {
					return err.Error(), false
				}
				if how != "exact" {
					how = " (matched " + how + ")"
				} else {
					how = ""
				}
				return fmt.Sprintf("edited %s%s; it has %d lines now", rel, how, strings.Count(out, "\n")+1), true
			}},
		{Name: "write", Aliases: []string{"write_file", "create_file", "create"},
			Description: "Create a file, or replace a whole file. For a change to an existing file use edit: rewriting a file risks losing lines.",
			Schema:      obj([]string{"path", "content"}, map[string]any{"path": path, "content": str("The whole file.")}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				rel := argStr(args, "path", "file_path", "file", "filename")
				p, err := confineWrite(root, rel)
				if err != nil {
					return err.Error(), false
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err.Error(), false
				}
				content := argStr(args, "content", "text", "contents", "file_text")
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return err.Error(), false
				}
				return fmt.Sprintf("wrote %s (%d lines)", rel, strings.Count(content, "\n")+1), true
			}},
		{Name: "run_checks", Aliases: []string{"check", "run_tests", "test"}, Description: "Run the domain's checks in the worktree.",
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				out, err := checks(ctx, env, root)
				if err != nil {
					return tail(out, 6000) + "\n" + err.Error(), false
				}
				return tail(out, 6000), true
			}},
	}
}

const (
	readLines    = 300 // a read's default window (oh-my-pi's)
	readMaxLines = 2000
	readMaxBytes = 50 << 10
	lineMax      = 500 // characters of one line shown by read and grep
	grepMax      = 100
	findMax      = 200
)

// readWindow: limit lines of text from line offset (1-based), within
// readMaxBytes, with a note on how to go on.
func readWindow(text string, offset, limit int) string {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if total == 0 {
		return "(empty file)"
	}
	offset = max(offset, 1)
	if offset > total {
		return fmt.Sprintf("offset %d is past the end of the file (%d lines)", offset, total)
	}
	limit = min(max(limit, 1), readMaxLines)
	var b strings.Builder
	end := offset - 1
	for ; end < total && end < offset-1+limit; end++ {
		l := lines[end]
		if len(l) > lineMax {
			l = l[:lineMax] + "…(line cut)\n"
		}
		if b.Len()+len(l) > readMaxBytes {
			break
		}
		b.WriteString(l)
	}
	out := b.String()
	if end < total {
		out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n\n[lines %d-%d of %d. Use offset=%d to continue.]", offset, end, total, end+1)
	}
	return out
}

// grepTool: pattern's matches under path, as path:line:text.
func grepTool(root string, args map[string]any) (string, bool) {
	pat := argStr(args, "pattern", "query", "regex", "q")
	if pat == "" {
		return "pattern is empty", false
	}
	note := ""
	if argBool(args, "literal", "fixed_strings") {
		pat = regexp.QuoteMeta(pat)
	}
	flags := ""
	if argBool(args, "ignore_case", "case_insensitive", "i") {
		flags = "(?i)"
	}
	re, err := regexp.Compile(flags + pat)
	if err != nil { // a pattern in another regex dialect: search for it as text
		re, note = regexp.MustCompile(flags+regexp.QuoteMeta(pat)), "(not a valid RE2 pattern, searched as plain text)\n"
	}
	base, err := confine(root, argStr(args, "path", "dir", "directory"))
	if err != nil {
		return err.Error(), false
	}
	glob := argStr(args, "glob", "include", "file_pattern")
	var out []string
	more := false
	_ = walkFiles(root, base, func(rel string, d fs.DirEntry) bool {
		if glob != "" && !globMatch(glob, rel) {
			return true
		}
		if info, err := d.Info(); err != nil || info.Size() > 2<<20 {
			return true
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return true // binary
		}
		for i, l := range strings.Split(string(b), "\n") {
			if !re.MatchString(l) {
				continue
			}
			if len(out) == grepMax {
				more = true
				return false
			}
			if len(l) > lineMax {
				l = l[:lineMax] + "…"
			}
			out = append(out, fmt.Sprintf("%s:%d:%s", rel, i+1, l))
		}
		return true
	})
	if len(out) == 0 {
		return note + "no matches", true
	}
	if more {
		out = append(out, fmt.Sprintf("[first %d matches; narrow the pattern, path or glob for the rest]", grepMax))
	}
	return note + strings.Join(out, "\n"), true
}

// findTool: the files under path matching pattern.
func findTool(root string, args map[string]any) (string, bool) {
	base, err := confine(root, argStr(args, "path", "dir", "directory"))
	if err != nil {
		return err.Error(), false
	}
	glob := argStr(args, "pattern", "glob", "name")
	var out []string
	more := false
	_ = walkFiles(root, base, func(rel string, d fs.DirEntry) bool {
		if glob != "" && !globMatch(glob, rel) {
			return true
		}
		if len(out) == findMax {
			more = true
			return false
		}
		out = append(out, rel)
		return true
	})
	if len(out) == 0 {
		return "no files match", true
	}
	if more {
		out = append(out, fmt.Sprintf("[first %d files; narrow the pattern or path]", findMax))
	}
	return strings.Join(out, "\n"), true
}

// listDir: a directory's entries (directories with a slash), for a read of one.
func listDir(root, dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return relErr(root, err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() == ".git" {
			continue
		}
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	return "(a directory) " + strings.Join(out, "  ")
}

// walkFiles calls fn with every file under base (path relative to root),
// skipping .git and vendored dependencies, in lexical order, until fn says stop.
func walkFiles(root, base string, fn func(rel string, d fs.DirEntry) bool) error {
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); p != base && (n == ".git" || n == "node_modules" || n == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" { // a worktree's .git file
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !fn(filepath.ToSlash(rel), d) {
			return fs.SkipAll
		}
		return nil
	})
}

// walkDirs calls fn with every directory under root up to depth levels
// (relative, slash-separated), skipping hidden and vendored ones.
func walkDirs(root string, depth int, fn func(rel string)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if n := d.Name(); strings.HasPrefix(n, ".") || n == "node_modules" || n == "vendor" || n == "testdata" {
			return filepath.SkipDir
		}
		fn(filepath.ToSlash(rel))
		if strings.Count(rel, string(filepath.Separator)) >= depth-1 {
			return filepath.SkipDir
		}
		return nil
	})
}

// globMatch: a glob against a slash path; ** spans directories, and a
// pattern without a slash matches the file name anywhere.
func globMatch(glob, rel string) bool {
	glob = strings.TrimPrefix(glob, "./")
	if !strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, path.Base(rel))
		return ok
	}
	return globParts(strings.Split(glob, "/"), strings.Split(rel, "/"))
}

func globParts(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if globParts(g[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], p[0]); !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}

// relErr: an error without the worktree's absolute path in it.
func relErr(root string, err error) string {
	return strings.ReplaceAll(err.Error(), root+string(filepath.Separator), "")
}

// argStr: the first of keys present as a string (models name arguments
// after the harness they were trained on).
func argStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := args[k].(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, def int, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return def
}

func argBool(args map[string]any, keys ...string) bool {
	for _, k := range keys {
		switch v := args[k].(type) {
		case bool:
			return v
		case string:
			return v == "true"
		}
	}
	return false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// confine: a path inside root, also after resolving symlinks.
func confine(root, p string) (string, error) {
	full := filepath.Join(root, p)
	if filepath.IsAbs(p) && within(root, filepath.Clean(p)) {
		full = filepath.Clean(p)
	}
	if !within(root, full) {
		return "", fmt.Errorf("%s: outside the worktree", p)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	for probe := full; ; probe = filepath.Dir(probe) {
		if r, err := filepath.EvalSymlinks(probe); err == nil {
			if !within(realRoot, r) {
				return "", fmt.Errorf("%s: outside the worktree (symlink)", p)
			}
			break
		}
		if filepath.Dir(probe) == probe {
			break
		}
	}
	return full, nil
}

func confineWrite(root, p string) (string, error) {
	full, err := confine(root, p)
	if err != nil {
		return "", err
	}
	if rel, _ := filepath.Rel(root, full); rel == "." || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: not writable", p)
	}
	return full, nil
}
