package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/nenkoru/warden/internal/github"
)

// ---- the human-only kernel (github.Kernel): never merged by Warden

const labelKernel = "kernel"

const kernelWhy = "human-only kernel: CI and release configuration, licensing, Warden's own config, pack, policy, budget, sandbox and GitHub guard, " +
	"and anything about secrets change only by the owner's hand"

// markKernel labels a PR needs-human and kernel and, once per head commit,
// tells the owner which kernel files it changes.
func markKernel(ctx context.Context, c *github.Client, env *Env, n int, sha string, files []string) {
	_ = c.AddLabels(ctx, n, labelNeedsHuman, labelKernel)
	mark := "<!-- warden:kernel " + sha + " -->"
	cs, err := c.Comments(ctx, n)
	if err != nil {
		return
	}
	for _, cm := range cs {
		if strings.Contains(cm.Body, mark) {
			return
		}
	}
	_ = c.Comment(ctx, n, fmt.Sprintf("%s — %s. This PR changes %s, so Warden never merges it, whoever wrote it: the merge is yours.\n\n%s",
		ownerMention(env), kernelWhy, noMention(strings.Join(files, ", ")), mark))
}

// kernelHold: markKernel, and the merge refused.
func kernelHold(ctx context.Context, c *github.Client, env *Env, n int, sha string, files []string) Result {
	markKernel(ctx, c, env, n, sha, files)
	return refused(fmt.Sprintf("PR #%d touches the human-only kernel (%s): labeled needs-human + kernel, never merged by Warden", n, strings.Join(files, ", ")))
}

// mergeAllowed: the kernel checked again right before a merge, from the
// PR's files at that moment (the client's Merge checks once more).
func mergeAllowed(ctx context.Context, c *github.Client, env *Env, pr github.PR) (Result, bool) {
	kern, err := c.KernelTouched(ctx, pr.Number)
	if err != nil {
		return Result{Summary: "merge-pr: the kernel check failed: " + err.Error()}, false
	}
	if len(kern) > 0 {
		return kernelHold(ctx, c, env, pr.Number, pr.Head.SHA, kern), false
	}
	return Result{}, true
}

// KernelPRs: one line per open PR labeled kernel, for the digest's "needs
// you": "kernel PR #N: title — human-only: files".
func KernelPRs(ctx context.Context, c *github.Client) ([]string, error) {
	prs, err := c.OpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range prs {
		if !p.Has(labelKernel) {
			continue
		}
		files := "(no kernel file at its head now)"
		if kern, err := c.KernelTouched(ctx, p.Number); err != nil {
			files = "(its files could not be listed)"
		} else if len(kern) > 0 {
			files = strings.Join(kern, ", ")
		}
		out = append(out, noMention(fmt.Sprintf("kernel PR #%d: %s — human-only: %s", p.Number, oneLine(p.Title, 100), files)))
	}
	return out, nil
}

// ownerWrote: the login is the owner's (params.owner).
func ownerWrote(env *Env, login string) bool {
	o := strings.TrimPrefix(param(nil, env, "owner", ""), "@")
	return o != "" && strings.EqualFold(login, o)
}
