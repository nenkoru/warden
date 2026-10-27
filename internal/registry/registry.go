// Package registry installs extensions from existing registries (MCP Registry,
// git skill folders, Claude Code plugin marketplaces, pi packages) with pinned
// versions and integrity hashes. Nothing from a package is executed on install.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Ref names an extension. Kind: mcp | skill | plugin | pi.
//   - mcp:    Name = server name, Version optional.
//   - skill:  Source = git repo, Name = subpath ("." = root), Version = sha|tag|branch.
//   - plugin: Name = plugin, Source = marketplace repo, Version = marketplace ref (optional).
//   - pi:     npm: Name = package; git: Source = Name = repo, Version = ref.
type Ref struct {
	Kind    string `toml:"kind"`
	Name    string `toml:"name"`
	Version string `toml:"version,omitempty"`
	Source  string `toml:"source,omitempty"`
}

// Locked is a resolved, verified extension.
type Locked struct {
	Ref       Ref    `toml:"ref"`
	Resolved  string `toml:"resolved"`  // tarball URL | git commit SHA | OCI ref | remote URL
	Integrity string `toml:"integrity"` // sha512-<b64> | sha256:<hex> (archive file, git tree or OCI digest)
	Path      string `toml:"path,omitempty"`
	Archive   string `toml:"archive,omitempty"` // downloaded file kept for Verify

	Command []string `toml:"command,omitempty"`
	URL     string   `toml:"url,omitempty"`
	Env     []string `toml:"env,omitempty"`
	Skills  []string `toml:"skills,omitempty"`
	Hooks   string   `toml:"hooks,omitempty"`
	MCPJSON string   `toml:"mcp_json,omitempty"`
	Notes   []string `toml:"notes,omitempty"`
}

// Client holds endpoints; zero fields default to the public services.
type Client struct {
	HTTP                           *http.Client
	MCPRegistry, NPM, PyPI, GitHub string
	Git                            string
}

func (c *Client) withDefaults() *Client {
	d := Client{}
	if c != nil {
		d = *c
	}
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
		*s = strings.TrimRight(*s, "/")
	}
	def(&d.MCPRegistry, "https://registry.modelcontextprotocol.io")
	def(&d.NPM, "https://registry.npmjs.org")
	def(&d.PyPI, "https://pypi.org")
	def(&d.GitHub, "https://github.com")
	def(&d.Git, "git")
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	return &d
}

// Parse reads "kind:spec" (see package doc on Ref).
func Parse(s string) (Ref, error) {
	kind, rest, _ := strings.Cut(s, ":")
	r := Ref{Kind: kind}
	bad := func(why string) (Ref, error) { return Ref{}, fmt.Errorf("registry: %q: %s", s, why) }
	if rest == "" {
		return bad("want kind:spec")
	}
	switch kind {
	case "mcp":
		r.Name, r.Version = splitVer(rest)
	case "skill":
		repo, sub, ok := cutSub(rest)
		if ok {
			sub, r.Version = splitVer(sub)
		} else {
			repo, r.Version = splitVer(repo)
		}
		r.Source, r.Name = repo, filepath.ToSlash(filepath.Clean("/" + sub))[1:]
		if r.Name == "" {
			r.Name = "."
		}
		if sub != "" && !filepath.IsLocal(sub) {
			return bad("subpath escapes the repo")
		}
	case "plugin":
		name, mkt, ok := strings.Cut(rest, "@")
		if !ok || mkt == "" {
			return bad("want plugin:NAME@MARKETPLACE")
		}
		r.Name = name
		r.Source, r.Version = splitVer(mkt)
	case "pi":
		if g, ok := strings.CutPrefix(rest, "git:"); ok {
			r.Source, r.Version = splitVer(g)
			r.Name = r.Source
		} else {
			r.Name, r.Version = splitVer(rest)
			if !npmName.MatchString(r.Name) {
				return bad("bad npm package name")
			}
		}
	default:
		return bad("unknown kind (mcp|skill|plugin|pi)")
	}
	if r.Name == "" || (r.Kind != "mcp" && r.Kind != "pi" && r.Source == "") {
		return bad("missing name or source")
	}
	if strings.HasPrefix(r.Source, "-") || strings.HasPrefix(r.Version, "-") {
		return bad("leading '-'")
	}
	return r, nil
}

func (r Ref) String() string {
	v := ""
	if r.Version != "" {
		v = "@" + r.Version
	}
	switch r.Kind {
	case "skill":
		return "skill:" + r.Source + "//" + r.Name + v
	case "plugin":
		return "plugin:" + r.Name + "@" + r.Source + v
	case "pi":
		if r.Source != "" {
			return "pi:git:" + r.Source + v
		}
	}
	return r.Kind + ":" + r.Name + v
}

// splitVer splits at the last "@" that is not the first byte (scoped npm names).
func splitVer(s string) (string, string) {
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// cutSub splits "repo//sub" ignoring the "//" of a URL scheme.
func cutSub(s string) (string, string, bool) {
	off := 0
	if i := strings.Index(s, "://"); i >= 0 {
		off = i + 3
	}
	j := strings.Index(s[off:], "//")
	if j < 0 {
		return s, "", false
	}
	return s[:off+j], s[off+j+2:], true
}

// Resolve downloads, verifies and unpacks r under cacheDir. c may be nil.
func Resolve(ctx context.Context, r Ref, cacheDir string, c *Client) (Locked, error) {
	c = c.withDefaults()
	cacheDir, err := filepath.Abs(cacheDir)
	if err != nil {
		return Locked{}, err
	}
	var l Locked
	switch r.Kind {
	case "mcp":
		l, err = c.mcp(ctx, r, cacheDir)
	case "skill":
		l, err = c.skill(ctx, r, cacheDir)
	case "plugin":
		l, err = c.plugin(ctx, r, cacheDir)
	case "pi":
		l, err = c.pi(ctx, r, cacheDir)
	default:
		err = errors.New("unknown kind")
	}
	if err != nil {
		return Locked{}, fmt.Errorf("registry: %s: %w", r, err)
	}
	return l, nil
}

// Verify recomputes the integrity of what is on disk and fails closed.
func Verify(l Locked) error {
	fail := func(err error) error { return fmt.Errorf("registry: verify %s: %w", l.Ref, err) }
	switch {
	case l.Archive != "":
		got, err := fileDigest(l.Archive, l.Integrity)
		if err != nil {
			return fail(err)
		}
		if got != l.Integrity {
			return fail(fmt.Errorf("archive %s: integrity %s, want %s", l.Archive, got, l.Integrity))
		}
		if l.Path != "" {
			return wrapNil(fail, sameTree(l.Archive, l.Path))
		}
		return nil
	case l.Path != "":
		got, err := treeHash(l.Path)
		if err != nil {
			return fail(err)
		}
		if got != l.Integrity {
			return fail(fmt.Errorf("tree %s: hash %s, want %s", l.Path, got, l.Integrity))
		}
		return nil
	case len(l.Command) > 0 && l.Command[0] == "docker":
		if !strings.HasPrefix(l.Integrity, "sha256:") {
			return fail(errors.New("image digest not pinned"))
		}
		for _, a := range l.Command {
			if strings.HasSuffix(a, "@"+l.Integrity) {
				return nil
			}
		}
		return fail(errors.New("command does not run the pinned digest"))
	case l.URL != "" && len(l.Command) == 0:
		return nil // remote server: nothing on disk
	}
	return fail(errors.New("nothing to verify"))
}

func wrapNil(f func(error) error, err error) error {
	if err == nil {
		return nil
	}
	return f(err)
}

// Lock is warden.lock.
type Lock struct {
	Entries []Locked `toml:"extension"`
}

// ReadLock reads a lock file; a missing file is an empty lock.
func ReadLock(path string) (Lock, error) {
	var l Lock
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if l, err = parseLock(b); err != nil {
		return l, fmt.Errorf("registry: %s: %w", path, err)
	}
	return l, nil
}

// parseLock: a lock file's contents.
func parseLock(b []byte) (Lock, error) {
	var l Lock
	return l, toml.Unmarshal(b, &l)
}

func (l Lock) Write(path string) error {
	b, err := toml.Marshal(l)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Put adds e, replacing an entry with the same kind, source and name.
func (l *Lock) Put(e Locked) {
	key := func(x Locked) string { return x.Ref.Kind + "\x00" + x.Ref.Source + "\x00" + x.Ref.Name }
	for i := range l.Entries {
		if key(l.Entries[i]) == key(e) {
			l.Entries[i] = e
			return
		}
	}
	l.Entries = append(l.Entries, e)
	sort.SliceStable(l.Entries, func(i, j int) bool { return key(l.Entries[i]) < key(l.Entries[j]) })
}
