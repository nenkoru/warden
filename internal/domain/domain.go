// Package domain: what a warden is bound to, described by a manifest
// (domain.toml) instead of code: what to watch (observe, inventory), what it
// may change (act, each with its floors), how to verify (check), where news
// comes from (feeds), which MCP servers and skills it brings. A project, a
// Mac, a Kubernetes namespace are packs of this one shape.
package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/nenkoru/warden/internal/policy"
)

type Domain struct {
	Name        string            `toml:"name"`
	Description string            `toml:"description"`
	Params      map[string]string `toml:"params"` // defaults; the instance overrides
	// Pause: the owner's kill switch, checked first every tick. Any finding
	// from a pause step (or an error running one) pauses that tick: it only
	// observes and reports; every act is refused.
	Pause      []Step            `toml:"pause"`
	Observe    []Step            `toml:"observe"`
	Inventory  []Step            `toml:"inventory"`
	Act        []Step            `toml:"act"`
	Check      []Step            `toml:"check"`
	Feeds      Feeds             `toml:"feeds"`
	MCP        map[string]MCPRef `toml:"mcp"`
	Extensions []string          `toml:"extensions"` // registry references ("skill:…", "mcp:…") the pack needs
	Dir        string            `toml:"-"`
}

// Step: one observer, inventory source, action or check.
type Step struct {
	Name      string        `toml:"name"`
	Run       Run           `toml:"run"`
	Map       *Map          `toml:"map"`
	Interpret bool          `toml:"interpret"` // the brain turns raw output into findings
	Floors    policy.Floors `toml:"floors"`
	Kind      string        `toml:"kind"`     // the finding kind an observe step reports (default: its name)
	Severity  string        `toml:"severity"` // default severity of its findings
	Fix       string        `toml:"fix"`      // the act its findings propose (observe steps)
	// Foreach: run the step once per value of a list, e.g. { namespace = "{namespaces}" }:
	// the list (comma or space separated, after params) binds the one key in turn.
	// An empty list runs the step once with the params as they are.
	Foreach map[string]string `toml:"foreach"`
}

// Each: the param sets a step runs with (one per foreach value), and the
// bound values for each (to stamp on what that run produced).
func (s Step) Each(params map[string]string) (sets []map[string]string, bound []map[string]string) {
	for k, list := range s.Foreach {
		for _, v := range strings.FieldsFunc(expand(list, params), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			ps := make(map[string]string, len(params)+1)
			for pk, pv := range params {
				ps[pk] = pv
			}
			ps[k] = v
			sets, bound = append(sets, ps), append(bound, map[string]string{k: v})
		}
	}
	if len(sets) == 0 {
		return []map[string]string{params}, []map[string]string{nil}
	}
	return sets, bound
}

// Run: exactly one of Cmd, MCP+Tool, Builtin.
type Run struct {
	Cmd     []string       `toml:"cmd"`
	MCP     string         `toml:"mcp"`
	Tool    string         `toml:"tool"`
	Args    map[string]any `toml:"args"`
	Builtin string         `toml:"builtin"`
	With    map[string]any `toml:"with"` // builtin options
}

type Feeds struct {
	RSS      []string `toml:"rss"`
	Releases []string `toml:"releases"` // GitHub owner/repo
	OSV      bool     `toml:"osv"`
	// Fix: the act that remedies an advisory, per ecosystem ({"Go" = "go-bump"}).
	Fix map[string]string `toml:"fix"`
}

// MCPRef: a server the domain uses: a registry reference or an inline
// .mcp.json-shaped entry.
type MCPRef struct {
	Registry string            `toml:"registry"` // "mcp" (the MCP Registry)
	Name     string            `toml:"name"`
	Version  string            `toml:"version"`
	Command  []string          `toml:"command"`
	Args     []string          `toml:"args"`
	Env      map[string]string `toml:"env"`
	URL      string            `toml:"url"`
	Net      []string          `toml:"net"` // hosts it may reach when sandboxed
}

// Finding: one thing an observer noticed (the native line format of steps).
type Finding struct {
	ID       string            `json:"id"` // stable across ticks (from key)
	Key      string            `json:"key,omitempty"`
	Step     string            `json:"step,omitempty"`
	Kind     string            `json:"kind"`
	Severity string            `json:"severity"`
	Subject  string            `json:"subject"`
	Title    string            `json:"title"`
	Detail   string            `json:"detail,omitempty"`
	Links    []string          `json:"links,omitempty"`
	Count    int               `json:"count,omitempty"`
	Source   string            `json:"source,omitempty"` // the signal source or feed it came from (scored by the owner's labels)
	Fix      *Fix              `json:"fix,omitempty"`
	Data     map[string]string `json:"data,omitempty"`
}

// Fix: the act that would remedy a finding, and its parameters.
type Fix struct {
	Act    string            `json:"act"`
	What   string            `json:"what"`
	Target string            `json:"target,omitempty"` // what the policy learns on (a module, a cache, a deployment)
	Params map[string]string `json:"params,omitempty"`
}

// Component: something the domain consists of (inventory), for news and advisories.
type Component struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Ecosystem string `json:"ecosystem,omitempty"` // Go, npm, PyPI, OCI, Homebrew …
	Where     string `json:"where,omitempty"`
}

var Severities = map[string]int{"info": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}

// Load reads dir/domain.toml; params (from the instance) override the pack's.
func Load(dir string, params map[string]string) (*Domain, error) {
	b, err := os.ReadFile(filepath.Join(dir, "domain.toml"))
	if err != nil {
		return nil, err
	}
	var d Domain
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s/domain.toml: %w", dir, err)
	}
	d.Dir, _ = filepath.Abs(dir)
	if d.Params == nil {
		d.Params = map[string]string{}
	}
	for k, v := range params {
		d.Params[k] = v
	}
	return &d, d.validate()
}

func (d *Domain) validate() error {
	if d.Name == "" {
		return fmt.Errorf("domain: name")
	}
	acts := map[string]bool{}
	for _, s := range d.Act {
		acts[s.Name] = true
	}
	for gi, group := range [][]Step{d.Pause, d.Observe, d.Inventory, d.Act, d.Check} {
		for _, s := range group {
			if len(s.Foreach) > 0 && (len(s.Foreach) > 1 || (gi != 1 && gi != 2) || s.Run.Builtin != "" || s.Interpret) {
				return fmt.Errorf("step %s: foreach binds one key, on an observe or inventory step that runs a cmd or an mcp tool", s.Name)
			}
			n := 0
			if len(s.Run.Cmd) > 0 {
				n++
			}
			if s.Run.MCP != "" || s.Run.Tool != "" {
				n++
				if _, ok := d.MCP[s.Run.MCP]; !ok {
					return fmt.Errorf("step %s: unknown mcp server %q", s.Name, s.Run.MCP)
				}
			}
			if s.Run.Builtin != "" {
				n++
			}
			if s.Name == "" || n != 1 {
				return fmt.Errorf("step %q: a name and exactly one of run.cmd, run.mcp+tool, run.builtin", s.Name)
			}
			if s.Fix != "" && !acts[s.Fix] {
				return fmt.Errorf("step %s: fix %q is not an act of this domain", s.Name, s.Fix)
			}
		}
	}
	for eco, a := range d.Feeds.Fix {
		if !acts[a] {
			return fmt.Errorf("feeds.fix %s: %q is not an act of this domain", eco, a)
		}
	}
	return nil
}

// ActStep: the act named so, if the domain has it.
func (d *Domain) ActStep(name string) (Step, bool) {
	for _, s := range d.Act {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// Expand fills {param} placeholders in argv (and in string args).
func Expand(argv []string, params map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expand(a, params)
	}
	return out
}

func ExpandArgs(args map[string]any, params map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		if s, ok := v.(string); ok {
			v = expand(s, params)
		}
		out[k] = v
	}
	return out
}

var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_.\[\]-]+)\}`)

func expand(s string, params map[string]string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := params[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// ---- mapping any output into findings or components

// Map: JSON (or text lines) → findings / components.
//
//	items     where the records are: "" or "[]" = the top-level array,
//	          "items[].spec.containers[]" flattens nested arrays
//	where     field filters (dotted paths), exact string match
//	lines     a regex: text output, one record per matching line (named groups become fields)
//	key, title, detail, subject, severity, component, version: templates over a record's fields
type Map struct {
	Items     string            `toml:"items"`
	Where     map[string]string `toml:"where"`
	Lines     string            `toml:"lines"`
	Key       string            `toml:"key"`
	Title     string            `toml:"title"`
	Detail    string            `toml:"detail"`
	Subject   string            `toml:"subject"`
	Severity  string            `toml:"severity"`
	Component string            `toml:"component"` // inventory: "{image}" → name + version parsed
	Version   string            `toml:"version"`
	Ecosystem string            `toml:"ecosystem"`
}

// Records: the records of raw output under the map.
func (m Map) Records(raw []byte) ([]map[string]any, error) {
	if m.Lines != "" {
		re, err := regexp.Compile(m.Lines)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, line := range strings.Split(string(raw), "\n") {
			sm := re.FindStringSubmatch(line)
			if sm == nil {
				continue
			}
			rec := map[string]any{"line": strings.TrimSpace(line)}
			for i, n := range re.SubexpNames() {
				if n != "" {
					rec[n] = sm[i]
				}
			}
			out = append(out, rec)
		}
		return out, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("not JSON (set map.lines for text): %w", err)
	}
	var out []map[string]any
	for _, x := range walk(v, m.Items) {
		rec, ok := x.(map[string]any)
		if !ok {
			rec = map[string]any{"value": x}
		}
		if m.match(rec) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// walk follows a path like "items[].spec.containers[]".
func walk(v any, path string) []any {
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		if arr, ok := v.([]any); ok {
			return arr
		}
		return []any{v}
	}
	if path == "[]" {
		arr, _ := v.([]any)
		return arr
	}
	seg, rest, _ := strings.Cut(path, ".")
	flat := strings.HasSuffix(seg, "[]")
	seg = strings.TrimSuffix(seg, "[]")
	cur := v
	if seg != "" {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	if !flat {
		if rest == "" {
			return []any{cur}
		}
		return walk(cur, rest)
	}
	arr, _ := cur.([]any)
	var out []any
	for _, x := range arr {
		if rest == "" {
			out = append(out, x)
		} else {
			out = append(out, walk(x, rest)...)
		}
	}
	return out
}

func (m Map) match(rec map[string]any) bool {
	for k, want := range m.Where {
		if field(rec, k) != want {
			return false
		}
	}
	return true
}

// field: a dotted path in a record, as text.
func field(rec map[string]any, path string) string {
	var cur any = rec
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	switch x := cur.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprint(int64(x))
		}
		return fmt.Sprint(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// Fill: a template over a record's fields and the params.
func Fill(tpl string, rec map[string]any, params map[string]string) string {
	return strings.TrimSpace(placeholder.ReplaceAllStringFunc(tpl, func(m string) string {
		k := m[1 : len(m)-1]
		if v := field(rec, k); v != "" {
			return v
		}
		if v, ok := params[k]; ok {
			return v
		}
		return ""
	}))
}

// Findings: raw output → findings. Output that already is Finding JSON lines
// (the native format) needs no map.
func (m *Map) Findings(raw []byte, step Step, params map[string]string) ([]Finding, error) {
	if m == nil {
		return nativeFindings(raw, step)
	}
	recs, err := m.Records(raw)
	if err != nil {
		return nil, err
	}
	seen := map[string]*Finding{}
	var out []Finding
	for _, r := range recs {
		f := Finding{Step: step.Name, Kind: or(step.Kind, step.Name), Severity: or(Fill(m.Severity, r, params), step.Severity, "medium"),
			Title: Fill(or(m.Title, "{line}"), r, params), Detail: Fill(m.Detail, r, params), Subject: Fill(m.Subject, r, params)}
		f.Key = Fill(or(m.Key, m.Title, "{line}"), r, params)
		if _, ok := Severities[f.Severity]; !ok {
			f.Severity = "medium"
		}
		if prev := seen[f.Key]; prev != nil { // the same thing again: counted, not repeated
			prev.Count++
			continue
		}
		f.Count = 1
		if step.Fix != "" {
			f.Fix = &Fix{Act: step.Fix, What: step.Fix + " " + or(f.Subject, f.Title), Target: or(f.Subject, f.Key), Params: stringFields(r)}
		}
		out = append(out, f)
		seen[f.Key] = &out[len(out)-1]
	}
	for i := range out {
		out[i].ID = ID(step.Name, out[i].Key)
	}
	return out, nil
}

func nativeFindings(raw []byte, step Step) ([]Finding, error) {
	var out []Finding
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var f Finding
		if err := json.Unmarshal(line, &f); err != nil {
			return out, fmt.Errorf("step %s: not a finding line (add a map): %s", step.Name, short(string(line), 80))
		}
		f.Step = step.Name
		if f.Kind == "" {
			f.Kind = or(step.Kind, step.Name)
		}
		if _, ok := Severities[f.Severity]; !ok {
			f.Severity = or(step.Severity, "medium")
		}
		f.Key = or(f.Key, f.ID, f.Subject+"|"+f.Title)
		f.ID = ID(step.Name, f.Key)
		out = append(out, f)
	}
	return out, nil
}

// Components: raw output → inventory.
func (m *Map) Components(raw []byte, params map[string]string) ([]Component, error) {
	if m == nil {
		var out []Component
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var c Component
			if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &c) == nil && c.Name != "" {
				out = append(out, c)
			}
		}
		return out, nil
	}
	recs, err := m.Records(raw)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Component
	for _, r := range recs {
		c := ParseComponent(Fill(or(m.Component, "{name}"), r, params))
		if v := Fill(m.Version, r, params); v != "" {
			c.Version = v
		}
		c.Ecosystem = or(Fill(m.Ecosystem, r, params), c.Ecosystem)
		if c.Name == "" || seen[c.Name+"@"+c.Version] {
			continue
		}
		seen[c.Name+"@"+c.Version] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ParseComponent: "nginx:1.25.3", "ghcr.io/org/app:v2@sha256:…", "golang.org/x/text@v0.3.7", "requests==2.31.0", "php 8.3.12".
func ParseComponent(s string) Component {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "@sha256:"); i > 0 {
		s = s[:i]
	}
	for _, sep := range []string{"==", "@", " "} {
		if i := strings.LastIndex(s, sep); i > 0 {
			return Component{Name: s[:i], Version: s[i+len(sep):]}
		}
	}
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "/") { // an image tag, not a registry port
		return Component{Name: s[:i], Version: s[i+1:], Ecosystem: "OCI"}
	}
	return Component{Name: s}
}

func stringFields(rec map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range rec {
		if s, ok := v.(string); ok && len(s) < 300 {
			out[k] = s
		}
	}
	return out
}

func ID(parts ...string) string {
	h := uint64(14695981039346656037)
	for _, p := range parts {
		for i := 0; i < len(p); i++ {
			h ^= uint64(p[i])
			h *= 1099511628211
		}
		h ^= 0xff
		h *= 1099511628211
	}
	return fmt.Sprintf("%012x", h&0xffffffffffff)
}

func or(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
