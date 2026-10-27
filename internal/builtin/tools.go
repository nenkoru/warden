package builtin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nenkoru/warden/internal/brain"
)

// fileTools: the brain's tools for a worktree, confined to it.
func fileTools(root string, env *Env) []brain.Tool {
	str := func(args map[string]any, k string) string { s, _ := args[k].(string); return s }
	obj := func(req []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	s := map[string]any{"type": "string"}
	return []brain.Tool{
		{Name: "list_files", Description: "List files under a directory of the worktree (default: all).", Schema: obj(nil, map[string]any{"dir": s}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				dir, err := confine(root, str(args, "dir"))
				if err != nil {
					return err.Error(), false
				}
				var out []string
				_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if len(out) >= 500 {
						return fs.SkipAll
					}
					if d.Name() == ".git" {
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if !d.IsDir() {
						rel, _ := filepath.Rel(root, p)
						out = append(out, rel)
					}
					return nil
				})
				return strings.Join(out, "\n"), true
			}},
		{Name: "read_file", Description: "Read a file of the worktree.", Schema: obj([]string{"path"}, map[string]any{"path": s}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				p, err := confine(root, str(args, "path"))
				if err != nil {
					return err.Error(), false
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return err.Error(), false
				}
				if len(b) > 200<<10 {
					return string(b[:200<<10]) + "\n…(truncated)", true
				}
				return string(b), true
			}},
		{Name: "write_file", Description: "Write (create or replace) a file of the worktree.", Schema: obj([]string{"path", "content"}, map[string]any{"path": s, "content": s}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				p, err := confineWrite(root, str(args, "path"))
				if err != nil {
					return err.Error(), false
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err.Error(), false
				}
				if err := os.WriteFile(p, []byte(str(args, "content")), 0o644); err != nil {
					return err.Error(), false
				}
				return "written", true
			}},
		{Name: "replace_in_file", Description: "Replace one exact occurrence of old with new in a file of the worktree.",
			Schema: obj([]string{"path", "old", "new"}, map[string]any{"path": s, "old": s, "new": s}),
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				p, err := confineWrite(root, str(args, "path"))
				if err != nil {
					return err.Error(), false
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return err.Error(), false
				}
				old := str(args, "old")
				if n := strings.Count(string(b), old); old == "" || n != 1 {
					return fmt.Sprintf("old text must occur exactly once (found %d)", n), false
				}
				st, _ := os.Stat(p)
				if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, str(args, "new"), 1)), st.Mode().Perm()); err != nil {
					return err.Error(), false
				}
				return "replaced", true
			}},
		{Name: "run_checks", Description: "Run the domain's checks in the worktree.",
			Run: func(ctx context.Context, args map[string]any) (string, bool) {
				out, err := checks(ctx, env, root)
				if err != nil {
					return tail(out, 6000) + "\n" + err.Error(), false
				}
				return tail(out, 6000), true
			}},
	}
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
