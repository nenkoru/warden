package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	npmName  = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]*$`)
	pypiName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

type mcpArg struct {
	Type, Name, Value, Default, Placeholder, ValueHint string
	IsRequired                                         bool
}

type mcpPackage struct {
	RegistryType, Identifier, Version, RuntimeHint string
	Transport                                      struct{ Type, URL string }
	RuntimeArguments, PackageArguments             []mcpArg
	EnvironmentVariables                           []struct {
		Name       string
		IsRequired bool
	}
}

type mcpServer struct {
	Name, Version string
	Packages      []mcpPackage
	Remotes       []struct{ Type, URL string }
}

func (c *Client) mcp(ctx context.Context, r Ref, cacheDir string) (Locked, error) {
	ver := r.Version
	if ver == "" {
		ver = "latest"
	}
	var raw json.RawMessage
	if err := c.getJSON(ctx, c.MCPRegistry+"/v0/servers/"+url.PathEscape(r.Name)+"/versions/"+url.PathEscape(ver), &raw); err != nil {
		return Locked{}, err
	}
	var wrap struct{ Server *mcpServer }
	var s mcpServer
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.Server != nil {
		s = *wrap.Server
	} else if err := json.Unmarshal(raw, &s); err != nil {
		return Locked{}, err
	}
	if s.Name != r.Name || s.Version == "" {
		return Locked{}, fmt.Errorf("registry returned %q@%q", s.Name, s.Version)
	}
	r.Version = s.Version
	var pkg *mcpPackage
	for _, t := range []string{"npm", "pypi", "oci"} {
		for i := range s.Packages {
			if pkg == nil && s.Packages[i].RegistryType == t {
				pkg = &s.Packages[i]
			}
		}
	}
	if pkg == nil {
		if len(s.Remotes) == 0 {
			return Locked{}, errors.New("no npm, pypi, oci package or remote")
		}
		return Locked{Ref: r, Resolved: s.Remotes[0].URL, URL: s.Remotes[0].URL,
			Notes: []string{"remote " + s.Remotes[0].Type + " server: nothing to pin"}}, nil
	}
	pv := pkg.Version
	if pv == "" {
		pv = s.Version
	}
	var l Locked
	var err error
	pargs, notes := mcpArgs(pkg.PackageArguments)
	switch pkg.RegistryType {
	case "npm":
		l, _, err = c.npm(ctx, pkg.Identifier, pv, cacheDir)
		l.Command = append([]string{"npx", "-y", pkg.Identifier + "@" + pv}, pargs...)
	case "pypi":
		l, err = c.pypi(ctx, pkg.Identifier, pv, cacheDir)
		l.Command = append([]string{"uvx", pkg.Identifier + "==" + pv}, pargs...)
	case "oci":
		rargs, rnotes := mcpArgs(pkg.RuntimeArguments)
		notes = append(notes, rnotes...)
		image := pkg.Identifier
		l.Resolved = image
		if pinned, digest, derr := c.ociDigest(ctx, image); derr == nil {
			image, l.Resolved, l.Integrity = pinned, pinned, digest
		} else {
			notes = append(notes, "unpinned digest: resolve with docker ("+derr.Error()+")")
		}
		l.Command = append(append(append([]string{"docker", "run", "--rm", "-i"}, rargs...), image), pargs...)
	}
	if err != nil {
		return Locked{}, err
	}
	if t := pkg.Transport; t.Type != "" && t.Type != "stdio" {
		l.URL = t.URL
		notes = append(notes, "package serves "+t.Type+" at "+t.URL)
	}
	for _, e := range pkg.EnvironmentVariables {
		if e.IsRequired {
			l.Env = append(l.Env, e.Name)
		}
	}
	l.Ref, l.Notes = r, append(l.Notes, notes...)
	return l, nil
}

// mcpArgs keeps arguments that have a value; required ones without get a note.
func mcpArgs(as []mcpArg) (out, notes []string) {
	for _, a := range as {
		v := a.Value
		if v == "" {
			v = a.Default
		}
		if v == "" {
			if a.IsRequired {
				notes = append(notes, fmt.Sprintf("needs argument %s %s", a.Name, a.Placeholder+a.ValueHint))
			}
			continue
		}
		if a.Type == "named" {
			out = append(out, a.Name)
		}
		out = append(out, v)
	}
	return
}

// npm fetches, verifies (sha512 SRI) and unpacks a package; returns the version.
func (c *Client) npm(ctx context.Context, name, ver, cacheDir string) (Locked, string, error) {
	if !npmName.MatchString(name) {
		return Locked{}, "", fmt.Errorf("bad npm name %q", name)
	}
	if ver == "" {
		ver = "latest"
	}
	var m struct {
		Name, Version        string
		Dist                 struct{ Tarball, Integrity string }
		OptionalDependencies map[string]string
		Scripts              map[string]string
	}
	if err := c.getJSON(ctx, c.NPM+"/"+name+"/"+url.PathEscape(ver), &m); err != nil {
		return Locked{}, "", err
	}
	if m.Name != name || m.Version == "" {
		return Locked{}, "", fmt.Errorf("npm returned %q@%q", m.Name, m.Version)
	}
	integ := pickSRI(m.Dist.Integrity)
	if integ == "" {
		return Locked{}, "", fmt.Errorf("%s@%s: no sha512 integrity", name, m.Version)
	}
	base := filepath.Join(cacheDir, "npm", safe(name)+"@"+safe(m.Version))
	arch := filepath.Join(base, "pkg.tgz")
	if err := c.download(ctx, m.Dist.Tarball, arch, integ); err != nil {
		return Locked{}, "", err
	}
	root, err := unpack(arch, filepath.Join(base, "pkg"))
	if err != nil {
		return Locked{}, "", err
	}
	l := Locked{Resolved: m.Dist.Tarball, Integrity: integ, Archive: arch, Path: root}
	if len(m.OptionalDependencies) > 0 {
		l.Notes = append(l.Notes, "optional (per-platform) dependencies are not pinned: "+strings.Join(sortedKeys(m.OptionalDependencies), ", "))
	}
	for _, s := range []string{"preinstall", "install", "postinstall"} {
		if m.Scripts[s] != "" {
			l.Notes = append(l.Notes, "npm "+s+" script present (not run by warden)")
		}
	}
	return l, m.Version, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// pypi pins a py3-none-any wheel, else the sdist, by its sha256.
func (c *Client) pypi(ctx context.Context, name, ver, cacheDir string) (Locked, error) {
	if !pypiName.MatchString(name) {
		return Locked{}, fmt.Errorf("bad PyPI name %q", name)
	}
	var m struct {
		URLs []struct {
			Filename, Packagetype, URL string
			Digests                    struct{ Sha256 string }
		} `json:"urls"`
	}
	if err := c.getJSON(ctx, c.PyPI+"/pypi/"+url.PathEscape(name)+"/"+url.PathEscape(ver)+"/json", &m); err != nil {
		return Locked{}, err
	}
	pick := -1
	for i, u := range m.URLs {
		if u.Packagetype == "bdist_wheel" && strings.HasSuffix(u.Filename, "-py3-none-any.whl") {
			pick = i
			break
		}
		if u.Packagetype == "sdist" && pick < 0 {
			pick = i
		}
	}
	if pick < 0 || len(m.URLs[pick].Digests.Sha256) != 64 {
		return Locked{}, fmt.Errorf("%s==%s: no sdist or py3-none-any wheel with sha256", name, ver)
	}
	u := m.URLs[pick]
	integ := "sha256:" + strings.ToLower(u.Digests.Sha256)
	dst := filepath.Join(cacheDir, "pypi", safe(name)+"@"+safe(ver), safe(u.Filename))
	if err := c.download(ctx, u.URL, dst, integ); err != nil {
		return Locked{}, err
	}
	return Locked{Resolved: u.URL, Integrity: integ, Archive: dst,
		Notes: []string{"uvx resolves dependencies at run time; only " + u.Filename + " is pinned"}}, nil
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// ociDigest resolves image:tag to name@sha256:… via the registry v2 API with an
// anonymous bearer token (ghcr, Docker Hub, …). Returns pinned ref and digest.
func (c *Client) ociDigest(ctx context.Context, image string) (string, string, error) {
	if name, d, ok := strings.Cut(image, "@"); ok {
		if !strings.HasPrefix(d, "sha256:") {
			return "", "", fmt.Errorf("unsupported digest %q", d)
		}
		return name + "@" + d, d, nil
	}
	name, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	host, repo, ok := strings.Cut(name, "/")
	if !ok || !strings.ContainsAny(host, ".:") && host != "localhost" {
		host, repo = "registry-1.docker.io", name
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	scheme := "https"
	if h := strings.Split(host, ":")[0]; h == "localhost" || h == "127.0.0.1" {
		scheme = "http"
	}
	u := scheme + "://" + host + "/v2/" + repo + "/manifests/" + url.PathEscape(tag)
	hdr := http.Header{"Accept": {"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"}}
	resp, err := c.get(ctx, "HEAD", u, hdr)
	if err != nil {
		return "", "", err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		ps := map[string]string{}
		for _, m := range authParam.FindAllStringSubmatch(resp.Header.Get("Www-Authenticate"), -1) {
			ps[m[1]] = m[2]
		}
		if ps["realm"] == "" {
			return "", "", errors.New("no bearer realm")
		}
		q := url.Values{"service": {ps["service"]}, "scope": {ps["scope"]}}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := c.getJSON(ctx, ps["realm"]+"?"+q.Encode(), &tok); err != nil {
			return "", "", err
		}
		bearer := tok.Token
		if bearer == "" {
			bearer = tok.AccessToken
		}
		hdr.Set("Authorization", "Bearer "+bearer)
		if resp, err = c.get(ctx, "HEAD", u, hdr); err != nil {
			return "", "", err
		}
		resp.Body.Close()
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if resp.StatusCode != 200 || !strings.HasPrefix(d, "sha256:") || len(d) != 71 {
		return "", "", fmt.Errorf("HEAD %s: %s, digest %q", u, resp.Status, d)
	}
	return name + "@" + d, d, nil
}
