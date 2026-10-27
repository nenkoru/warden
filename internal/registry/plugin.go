package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

func (c *Client) skill(ctx context.Context, r Ref, cacheDir string) (Locked, error) {
	l, err := c.gitTree(ctx, r.Source, r.Version, r.Name, cacheDir)
	if err != nil {
		return Locked{}, err
	}
	l.Ref, l.Skills = r, collectSkills(l.Path, []string{"."})
	if len(l.Skills) == 0 {
		return Locked{}, fmt.Errorf("no SKILL.md in %s", r.Name)
	}
	return l, nil
}

type marketplace struct {
	Metadata struct {
		PluginRoot string `json:"pluginRoot"`
	}
	Plugins []mktPlugin
}

type mktPlugin struct {
	Name           string
	Source, Skills json.RawMessage
}

type pluginSource struct {
	Source, Repo, URL, Path, Ref, SHA, Package, Version, Registry, SHA256 string
}

func (c *Client) plugin(ctx context.Context, r Ref, cacheDir string) (Locked, error) {
	mk, err := c.gitTree(ctx, r.Source, r.Version, ".", cacheDir)
	if err != nil {
		return Locked{}, err
	}
	var m marketplace
	var b []byte
	for _, f := range []string{".claude-plugin/marketplace.json", ".omp-plugin/marketplace.json"} {
		if b, err = os.ReadFile(filepath.Join(mk.Path, filepath.FromSlash(f))); err == nil {
			break
		}
	}
	if err != nil {
		return Locked{}, fmt.Errorf("no .claude-plugin/marketplace.json in %s", r.Source)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return Locked{}, fmt.Errorf("marketplace.json: %w", err)
	}
	i := slices.IndexFunc(m.Plugins, func(p mktPlugin) bool { return p.Name == r.Name })
	if i < 0 {
		return Locked{}, fmt.Errorf("plugin %q not in marketplace", r.Name)
	}
	e := m.Plugins[i]
	var l Locked
	var rel string
	var src pluginSource
	if json.Unmarshal(e.Source, &rel) == nil {
		if !strings.HasPrefix(rel, "./") && !strings.HasPrefix(rel, "../") && m.Metadata.PluginRoot != "" {
			rel = path.Join(m.Metadata.PluginRoot, rel)
		}
		p, err := within(mk.Path, rel)
		if err != nil {
			return Locked{}, err
		}
		h, err := treeHash(p)
		if err != nil {
			return Locked{}, err
		}
		l = Locked{Resolved: mk.Resolved, Integrity: h, Path: p}
	} else if err := json.Unmarshal(e.Source, &src); err != nil {
		return Locked{}, fmt.Errorf("plugin source: %w", err)
	} else {
		switch src.Source {
		case "github", "url", "git-subdir":
			loc := src.URL
			if src.Source == "github" {
				loc = "github.com/" + src.Repo
			}
			ref := src.Ref
			if src.SHA != "" {
				ref = src.SHA
			}
			l, err = c.gitTree(ctx, loc, ref, src.Path, cacheDir)
		case "npm":
			l, _, err = c.npm(ctx, src.Package, src.Version, cacheDir)
			if src.Registry != "" {
				l.Notes = append(l.Notes, "npm registry "+src.Registry+" ignored; used "+c.NPM)
			}
		case "archive":
			if len(src.SHA256) != 64 {
				return Locked{}, errors.New("archive source without sha256")
			}
			integ := "sha256:" + strings.ToLower(src.SHA256)
			base := filepath.Join(cacheDir, "archive", safe(r.Name)+"@"+src.SHA256[:16])
			arch := filepath.Join(base, "archive")
			if err = c.download(ctx, src.URL, arch, integ); err == nil {
				var root string
				root, err = unpack(arch, filepath.Join(base, "pkg"))
				l = Locked{Resolved: src.URL, Integrity: integ, Archive: arch, Path: root}
			}
		case "command":
			err = errors.New("command plugin sources are refused (they execute code)")
		default:
			err = fmt.Errorf("unknown plugin source %q", src.Source)
		}
		if err != nil {
			return Locked{}, err
		}
		l.Notes = append(l.Notes, "marketplace "+r.Source+"@"+mk.Resolved)
	}
	l.Ref = r
	decl := strs(e.Skills)
	var pj struct {
		Skills, Hooks json.RawMessage
		MCPServers    json.RawMessage `json:"mcpServers"`
	}
	if b, err := os.ReadFile(filepath.Join(l.Path, ".claude-plugin", "plugin.json")); err == nil {
		if err := json.Unmarshal(b, &pj); err != nil {
			return Locked{}, fmt.Errorf("plugin.json: %w", err)
		}
		decl = append(decl, strs(pj.Skills)...)
	}
	l.Skills = collectSkills(l.Path, append(decl, "skills"))
	l.Hooks = pathOrNote(&l, l.Path, "hooks/hooks.json", pj.Hooks, "hooks")
	l.MCPJSON = pathOrNote(&l, l.Path, ".mcp.json", pj.MCPServers, "mcpServers")
	return l, nil
}

func (c *Client) pi(ctx context.Context, r Ref, cacheDir string) (Locked, error) {
	var l Locked
	var err error
	if r.Source != "" {
		l, err = c.gitTree(ctx, r.Source, r.Version, ".", cacheDir)
	} else {
		l, r.Version, err = c.npm(ctx, r.Name, r.Version, cacheDir)
	}
	if err != nil {
		return Locked{}, err
	}
	l.Ref = r
	type manifest struct{ Extensions, Skills, Prompts []string }
	var pj struct{ Pi, Omp *manifest }
	if b, err := os.ReadFile(filepath.Join(l.Path, "package.json")); err == nil {
		if err := json.Unmarshal(b, &pj); err != nil {
			return Locked{}, fmt.Errorf("package.json: %w", err)
		}
	}
	m := pj.Pi
	if m == nil {
		m = pj.Omp
	}
	if m == nil {
		m = &manifest{Skills: []string{"skills"}}
		for _, d := range []string{"extensions", "prompts"} {
			if exists(filepath.Join(l.Path, d)) {
				if d == "extensions" {
					m.Extensions = []string{d}
				} else {
					m.Prompts = []string{d}
				}
			}
		}
	}
	l.Skills = collectSkills(l.Path, m.Skills)
	if len(m.Extensions) > 0 {
		l.Notes = append(l.Notes, "pi extensions (TypeScript) are not run by warden: "+strings.Join(m.Extensions, ", "))
	}
	if len(m.Prompts) > 0 {
		l.Notes = append(l.Notes, "pi prompts: "+strings.Join(m.Prompts, ", "))
	}
	return l, nil
}

// strs reads a JSON string or string array.
func strs(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return nil
	}
	return many
}

// collectSkills expands declared paths/globs (relative to base) to dirs with a
// SKILL.md: the path itself or its direct children. "!x" exclusions are dropped.
func collectSkills(base string, decl []string) []string {
	var out []string
	add := func(d string) {
		if exists(filepath.Join(d, "SKILL.md")) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	for _, rel := range decl {
		if rel == "" || strings.HasPrefix(rel, "!") {
			continue
		}
		p, err := within(base, rel)
		if err != nil {
			continue
		}
		ms, _ := filepath.Glob(p)
		for _, m := range ms {
			if filepath.Base(m) == "SKILL.md" {
				m = filepath.Dir(m)
			}
			add(m)
			kids, _ := filepath.Glob(filepath.Join(m, "*", "SKILL.md"))
			for _, k := range kids {
				add(filepath.Dir(k))
			}
		}
	}
	return out
}

// pathOrNote returns the plugin file at a path declared in plugin.json (string)
// or the default path; inline objects are noted, not extracted.
func pathOrNote(l *Locked, base, def string, decl json.RawMessage, field string) string {
	rel := def
	if len(decl) > 0 {
		var s string
		if json.Unmarshal(decl, &s) != nil {
			l.Notes = append(l.Notes, "plugin.json has inline "+field+" (not extracted)")
		} else {
			rel = s
		}
	}
	if p, err := within(base, rel); err == nil && exists(p) {
		return p
	}
	return ""
}
