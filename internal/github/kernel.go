package github

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// The human-only kernel: the parts of a repository Warden maintains that
// only ever change by the owner's hand. A pull request touching any of them
// is labeled needs-human and kernel, listed in the daily digest, and never
// merged by Warden, whoever wrote it (Warden itself, a delegate, a trusted
// author, Dependabot). The list is code, not configuration: params.protected
// and params.protected_names can add to it but never remove from it, and
// this file is in its own list, so an automatic change can't loosen it.

// Kernel: path patterns (path.Match per segment, "**" spans folders, a
// pattern without "/" matches the base name anywhere; see Glob).
var Kernel = []string{
	".github/**",       // CI workflows, Dependabot, templates: what runs with which permissions, what CI checks
	".goreleaser.yaml", // how releases are built, signed and published
	".goreleaser.yml",
	"LICENSE", // the terms the project is under
	"NOTICE",
	"SECURITY.md",                    // how vulnerabilities reach the owner
	"warden.toml",                    // Warden's own instance config: params, policy, budget, hooks
	"*.warden.toml",                  // …and any other instance config
	"warden.lock",                    // the pinned packs
	"packs/github-repo/**",           // the maintainer's own pack: its policy floors and rules
	"internal/policy/**",             // the policy engine: what may act alone
	"internal/github/github.go",      // send(): every request passes the guard here
	"internal/github/guard.go",       // the allowlist of GitHub writes and pushes
	"internal/github/guard_test.go",  // …and what pins it
	"internal/github/kernel.go",      // this list, its matcher, the merge's last check
	"internal/github/kernel_test.go", // …and what pins them
	"internal/meter/**",              // the model budget and its cap
	"internal/box/**",                // the sandbox around commands and agents
	"internal/update/verify*.go",     // the self-update's verification (checksums, signatures)
	"internal/update/**/*attest*",    // …and anything about attestation or signing there
	"internal/update/**/*cosign*",
	"internal/update/**/*sign*",
	"scripts/*attest*", // release helper scripts that attest or sign
	"scripts/*cosign*",
	"scripts/*sign*",
}

// KernelNames: a file whose base name contains one of these (any case) is
// kernel wherever it is: credentials never change automatically.
var KernelNames = []string{"secret", "credential", "token"}

// maxPRFiles: GitHub lists at most this many files of a PR; a PR that
// reaches it can't be checked, so it counts as kernel.
const maxPRFiles = 3000

// ErrKernel: a merge refused because the PR touches the kernel.
var ErrKernel = errors.New("the PR touches the human-only kernel: only the owner merges it")

// InKernel says whether a repository path is in the kernel.
func InKernel(p string) bool {
	p = cleanPath(p)
	for _, pat := range Kernel {
		if Glob(pat, p) {
			return true
		}
	}
	base := strings.ToLower(path.Base(p))
	for _, n := range KernelNames {
		if strings.Contains(base, n) {
			return true
		}
	}
	return false
}

// KernelFiles: the kernel paths among changed, sorted, without repeats.
func KernelFiles(changed []string) []string {
	var out []string
	for _, f := range changed {
		if f = cleanPath(f); InKernel(f) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func cleanPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(p, "\\", "/")), "/")
}

// Glob: path.Match per segment; "**" matches any number of segments; a
// pattern without "/" matches the base name anywhere ("*.warden.toml").
func Glob(pat, p string) bool {
	pat = strings.TrimPrefix(pat, "/")
	if !strings.Contains(pat, "/") {
		ok, _ := path.Match(pat, path.Base(p))
		return ok
	}
	return globSegs(strings.Split(pat, "/"), strings.Split(p, "/"))
}

func globSegs(ps, ss []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			for i := 0; i <= len(ss); i++ {
				if globSegs(ps[1:], ss[i:]) {
					return true
				}
			}
			return false
		}
		if len(ss) == 0 {
			return false
		}
		if ok, _ := path.Match(ps[0], ss[0]); !ok {
			return false
		}
		ps, ss = ps[1:], ss[1:]
	}
	return len(ss) == 0
}

// KernelTouched: the kernel files a PR changes at its head (renames count
// on both sides). A PR too large to list fully counts as kernel.
func (c *Client) KernelTouched(ctx context.Context, n int) ([]string, error) {
	files, err := c.PRFiles(ctx, n)
	if err != nil {
		return nil, err
	}
	if len(files) >= maxPRFiles {
		return []string{fmt.Sprintf("(%d+ files: too many to check)", maxPRFiles)}, nil
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Filename)
		if f.PreviousFilename != "" {
			names = append(names, f.PreviousFilename)
		}
	}
	return KernelFiles(names), nil
}

// Merge squash-merges a PR whose head is still sha. It lives here, in the
// kernel, so its last check can't be edited away by an automatic change: a
// PR touching the kernel is refused (ErrKernel) whoever calls Merge.
func (c *Client) Merge(ctx context.Context, n int, sha, title string) (string, error) {
	kern, err := c.KernelTouched(ctx, n)
	if err != nil {
		return "", fmt.Errorf("github: PR #%d: the kernel check failed: %w", n, err)
	}
	if len(kern) > 0 {
		return "", fmt.Errorf("github: PR #%d (%s): %w", n, strings.Join(kern, ", "), ErrKernel)
	}
	var r struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	if err := c.do(ctx, "PUT", c.repoPath("/pulls/%d/merge", n), map[string]string{"sha": sha, "merge_method": "squash", "commit_title": title}, &r); err != nil {
		return "", err
	}
	if !r.Merged {
		return "", fmt.Errorf("github: PR #%d was not merged", n)
	}
	return r.SHA, nil
}
