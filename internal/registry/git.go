package registry

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// git runs git with repo hooks, symlinks, fsmonitor and ext:: transport disabled.
func (c *Client) git(ctx context.Context, args ...string) (string, error) {
	a := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.symlinks=false",
		"-c", "core.fsmonitor=false", "-c", "protocol.ext.allow=never", "-c", "advice.detachedHead=false"}, args...)
	cmd := exec.CommandContext(ctx, c.Git, a...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// gitURL maps a source to a clone URL: URLs and paths pass through,
// "github.com/o/r" and "o/r" go to c.GitHub, other hosts get https://.
func (c *Client) gitURL(src string) (string, error) {
	switch {
	case strings.HasPrefix(src, "-"):
		return "", fmt.Errorf("bad git source %q", src)
	case strings.Contains(src, "://"), strings.HasPrefix(src, "/"), strings.HasPrefix(src, "git@"):
		return src, nil
	case strings.HasPrefix(src, "github.com/"):
		return c.GitHub + "/" + strings.TrimPrefix(src, "github.com/"), nil
	case strings.Count(src, "/") == 1 && !strings.Contains(strings.Split(src, "/")[0], "."):
		return c.GitHub + "/" + src, nil
	}
	return "https://" + src, nil
}

// rev resolves a tag, branch or "" (HEAD) to a commit SHA via ls-remote.
func (c *Client) rev(ctx context.Context, url, ref string) (string, error) {
	if fullSHA.MatchString(ref) {
		return strings.ToLower(ref), nil
	}
	out, err := c.git(ctx, "ls-remote", url)
	if err != nil {
		return "", err
	}
	refs := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(ln, "\t"); ok {
			refs[name] = sha
		}
	}
	q := ref
	if q == "" {
		q = "HEAD"
	}
	for _, k := range []string{"refs/tags/" + q + "^{}", "refs/tags/" + q, "refs/heads/" + q, q} {
		if sha, ok := refs[k]; ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%s: ref %q not found (short SHAs are not accepted)", url, ref)
}

// checkout clones url at sha into cacheDir/git/… without .git and returns the dir.
func (c *Client) checkout(ctx context.Context, url, sha, cacheDir string) (string, error) {
	dst := filepath.Join(cacheDir, "git", safe(url)+"@"+sha)
	tmp := dst + ".tmp"
	for _, d := range []string{dst, tmp} {
		if err := os.RemoveAll(d); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if _, err := c.git(ctx, "clone", "-q", "--filter=blob:none", "--no-checkout", "--", url, tmp); err != nil {
		return "", err
	}
	if _, err := c.git(ctx, "-C", tmp, "checkout", "-q", "--detach", sha); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	head, err := c.git(ctx, "-C", tmp, "rev-parse", "HEAD")
	if err == nil && !strings.EqualFold(head, sha) {
		err = fmt.Errorf("checked out %s, want %s", head, sha)
	}
	if err == nil {
		err = os.RemoveAll(filepath.Join(tmp, ".git"))
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return dst, nil
}

// gitTree resolves ref, checks out and hashes sub (relative) of the repo.
func (c *Client) gitTree(ctx context.Context, src, ref, sub, cacheDir string) (Locked, error) {
	url, err := c.gitURL(src)
	if err != nil {
		return Locked{}, err
	}
	sha, err := c.rev(ctx, url, ref)
	if err != nil {
		return Locked{}, err
	}
	root, err := c.checkout(ctx, url, sha, cacheDir)
	if err != nil {
		return Locked{}, err
	}
	p, err := within(root, sub)
	if err != nil {
		return Locked{}, err
	}
	h, err := treeHash(p)
	if err != nil {
		return Locked{}, err
	}
	return Locked{Resolved: sha, Integrity: h, Path: p}, nil
}
