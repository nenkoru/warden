// Package skills loads Agent Skills: folders with a SKILL.md whose
// frontmatter names and describes the skill.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Skill struct {
	Name, Description, Dir, Body string
	Meta                         map[string]string // other frontmatter keys
}

// Load reads each dir as a single skill folder (it has SKILL.md) or as a
// skills root whose subfolders hold SKILL.md; subfolders without one are
// skipped. A later skill with the same name replaces an earlier one.
func Load(dirs ...string) ([]Skill, error) {
	var out []Skill
	add := func(s Skill) {
		for i := range out {
			if out[i].Name == s.Name {
				out[i] = s
				return
			}
		}
		out = append(out, s)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "SKILL.md")); err == nil {
			s, err := loadOne(d)
			if err != nil {
				return nil, err
			}
			add(s)
			continue
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("skills: %w", err)
		}
		for _, e := range ents {
			sub := filepath.Join(d, e.Name())
			if fi, err := os.Stat(sub); err != nil || !fi.IsDir() { // follows symlinked skill folders
				continue
			}
			if _, err := os.Stat(filepath.Join(sub, "SKILL.md")); errors.Is(err, os.ErrNotExist) {
				continue
			}
			s, err := loadOne(sub)
			if err != nil {
				return nil, err
			}
			add(s)
		}
	}
	return out, nil
}

func loadOne(dir string) (Skill, error) {
	path := filepath.Join(dir, "SKILL.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, fmt.Errorf("skills: %w", err)
	}
	s, err := parse(string(b))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: %s: %w", path, err)
	}
	if s.Dir, err = filepath.Abs(dir); err != nil {
		s.Dir = dir
	}
	return s, nil
}

// parse reads simple "key: value" frontmatter between --- lines. Values may
// be quoted; "key: >" or "key: |" takes the indented lines that follow.
func parse(text string) (Skill, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Skill{}, errors.New("no frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Skill{}, errors.New("frontmatter not closed")
	}
	meta := map[string]string{}
	for i := 1; i < end; i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") || l[0] == ' ' || l[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if block := strings.TrimRight(v, "-+"); block == ">" || block == "|" {
			var parts []string
			for i+1 < end && (lines[i+1] == "" || lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			sep := " "
			if block == "|" {
				sep = "\n"
			}
			v = strings.TrimSpace(strings.Join(parts, sep))
		} else {
			v = unquote(v)
		}
		meta[k] = v
	}
	s := Skill{Name: meta["name"], Description: meta["description"]}
	delete(meta, "name")
	delete(meta, "description")
	if s.Name == "" {
		return Skill{}, errors.New("frontmatter: name missing")
	}
	if s.Description == "" {
		return Skill{}, errors.New("frontmatter: description missing")
	}
	s.Meta = meta
	s.Body = strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
	return s, nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Index renders "- name: description" lines sorted by name, for a system prompt.
func Index(sk []Skill) string {
	sorted := append([]Skill(nil), sk...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, s := range sorted {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, strings.Join(strings.Fields(s.Description), " "))
	}
	return b.String()
}
