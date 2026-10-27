package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nenkoru/warden/internal/domain"
)

// cellarRoots: where Homebrew keeps formulas (a var for tests).
var cellarRoots = []string{"/opt/homebrew/Cellar", "/usr/local/Cellar"}

// Inventory runs one built-in inventory source.
func Inventory(ctx context.Context, name string, with map[string]any, env *Env) ([]domain.Component, error) {
	switch name {
	case "lockfiles":
		root := param(with, env, "root", ".")
		if _, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("lockfiles: %w", err)
		}
		return lockfiles(root), nil
	case "homebrew":
		return homebrew(), nil
	}
	return nil, fmt.Errorf("builtin inventory %q: unknown", name)
}

// lockfiles: go.mod, requirements.txt (== pins only), package-lock.json.
func lockfiles(root string) []domain.Component {
	var out []domain.Component
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		out = append(out, parseGoMod(b)...)
	}
	if b, err := os.ReadFile(filepath.Join(root, "requirements.txt")); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line, _, _ := strings.Cut(sc.Text(), "#")
			line, _, _ = strings.Cut(line, ";") // environment markers
			if n, v, ok := strings.Cut(strings.TrimSpace(line), "=="); ok {
				n, _, _ = strings.Cut(n, "[") // extras
				out = append(out, domain.Component{Name: strings.TrimSpace(n), Version: strings.TrimSpace(v), Ecosystem: "PyPI", Where: "requirements.txt"})
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "package-lock.json")); err == nil {
		var lock struct {
			Packages map[string]struct {
				Version string `json:"version"`
			} `json:"packages"`
		}
		if json.Unmarshal(b, &lock) == nil {
			seen := map[string]bool{}
			for p, v := range lock.Packages {
				i := strings.LastIndex(p, "node_modules/")
				if i < 0 || v.Version == "" {
					continue
				}
				c := domain.Component{Name: p[i+len("node_modules/"):], Version: v.Version, Ecosystem: "npm", Where: "package-lock.json"}
				if k := c.Name + "@" + c.Version; !seen[k] {
					seen[k] = true
					out = append(out, c)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

func parseGoMod(b []byte) []domain.Component {
	var out []domain.Component
	add := func(n, v string) {
		out = append(out, domain.Component{Name: n, Version: v, Ecosystem: "Go", Where: "go.mod"})
	}
	block := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case f[0] == "require" && len(f) >= 2 && f[1] == "(":
			block = true
		case block && f[0] == ")":
			block = false
		case block && len(f) >= 2:
			add(f[0], f[1])
		case f[0] == "require" && len(f) >= 3:
			add(f[1], f[2])
		}
	}
	return out
}

// homebrew: installed formulas from the Cellar folders (never runs brew).
func homebrew() []domain.Component {
	var out []domain.Component
	for _, root := range cellarRoots {
		formulas, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, fm := range formulas {
			if !fm.IsDir() || strings.HasPrefix(fm.Name(), ".") {
				continue
			}
			vers, err := os.ReadDir(filepath.Join(root, fm.Name()))
			if err != nil {
				continue
			}
			best := ""
			for _, v := range vers {
				if v.IsDir() && !strings.HasPrefix(v.Name(), ".") && (best == "" || semverLess(best, v.Name())) {
					best = v.Name()
				}
			}
			if best != "" {
				out = append(out, domain.Component{Name: fm.Name(), Version: best, Ecosystem: "Homebrew", Where: filepath.Join(root, fm.Name(), best)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
