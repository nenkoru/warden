package builtin

// Quality gates on what Warden merges by itself: the revert of its own merge
// when main turns red right after it, and an optional second model's review
// before it merges its own work. CI carries the rest (coverage must not
// drop, fuzz targets, govulncheck; see docs/usecases/github-maintainer.md).

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
)

const (
	refRevert   = "warden/revert-"
	labelRevert = "revert"
)

// revertable: the PR whose merge commit is sha (main's red head), when a
// revert is Warden's to make: Warden merged it (merged_by is its login; own,
// Dependabot or trusted), it is no revert itself, a push run failed on it (a
// scheduled run going red — a new advisory — is no fault of the last merge),
// and main was green on its one parent. nil: only the red-main finding; the
// owner's own pushes and merges are never reverted.
func revertable(ctx context.Context, c *github.Client, env *Env, sha string, red []github.WorkflowRun) (*github.PR, error) {
	pushed := false
	for _, r := range red {
		pushed = pushed || r.Event == "push"
	}
	if !pushed {
		return nil, nil
	}
	closed, err := c.ClosedPRs(ctx, mainBranch(env))
	if err != nil {
		return nil, err
	}
	n := 0
	for _, p := range closed {
		if p.MergedAt != nil && p.MergeCommitSHA == sha {
			n = p.Number
			break
		}
	}
	if n == 0 {
		return nil, nil
	}
	pr, err := c.PR(ctx, n) // merged_by: only in a single PR
	if err != nil {
		return nil, err
	}
	login, err := c.Login(ctx)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(pr.Head.Ref, refRevert) || pr.MergedBy == nil || pr.MergedBy.Login != login ||
		prClass(pr.User.Login, login, listParam(env, "trusted_authors")) == "external" {
		return nil, nil
	}
	parents, err := c.Parents(ctx, sha)
	if err != nil || len(parents) != 1 {
		return nil, err
	}
	runs, err := c.Runs(ctx, mainBranch(env))
	if err != nil {
		return nil, err
	}
	if st, _ := runsState(runs, parents[0]); st != "green" {
		return nil, nil
	}
	return &pr, nil
}

// failedJobs: the failed jobs of red runs, "workflow (conclusion)" when a
// run's jobs can't be read.
func failedJobs(ctx context.Context, c *github.Client, red []github.WorkflowRun) string {
	var out []string
	for _, r := range red {
		js, err := c.FailedJobs(ctx, r.ID)
		if err != nil || len(js) == 0 {
			out = append(out, r.Name+" ("+r.Conclusion+")")
			continue
		}
		out = append(out, js...)
	}
	return noMention(oneLine(strings.Join(out, ", "), 300))
}

// revertPR: a branch warden/revert-<sha> from main with `git revert` of the
// merge commit, a PR (not a draft: CI runs, then merge-pr merges it as an
// own PR), and a comment on the original PR for the owner. Everything is
// checked again first: main still red at that commit, still Warden's merge.
func revertPR(ctx context.Context, c *github.Client, f domain.Finding, env *Env) Result {
	p := f.Fix.Params
	n, sha := atoi(p["pr"]), p["sha"]
	head, state, red, err := mainState(ctx, c, env)
	if err != nil {
		return Result{Summary: "revert-pr: " + err.Error()}
	}
	if head != sha || state != "red" {
		return Result{OK: true, Summary: fmt.Sprintf("revert-pr: main moved on (%s, %s): nothing to revert", short7(head), state)}
	}
	pr, err := revertable(ctx, c, env, sha, red)
	if err != nil {
		return Result{Summary: "revert-pr: " + err.Error()}
	}
	if pr == nil || pr.Number != n {
		return refused(fmt.Sprintf("refused: %s is not PR #%d merged by Warden onto a green main", short7(sha), n))
	}
	jobs := failedJobs(ctx, c, red)
	root, _ := filepath.Abs(param(nil, env, "root", "."))
	work := filepath.Join(env.State, "work")
	wt := filepath.Join(work, "revert-"+short7(sha))
	branch := refRevert + short7(sha)
	if err := worktreeFrom(ctx, c, env, root, wt, branch, mainBranch(env), "FETCH_HEAD"); err != nil {
		return Result{Summary: "revert-pr: " + err.Error()}
	}
	defer dropWorktree(ctx, root, work, wt)
	if _, err := git(ctx, wt, "revert", "--no-edit", sha); err != nil {
		return Result{Summary: "revert-pr: " + err.Error()}
	}
	out, _ := git(ctx, wt, "diff", "--name-only", "HEAD~1", "HEAD")
	if r, failed := pushOrHandOff(ctx, c, env, wt, branch, n, splitList(strings.ReplaceAll(out, "\n", ",")), false); failed {
		return r
	}
	title := revertTitle(pr.Title, n)
	body := fmt.Sprintf("Main went red after #%d (failed: %s). This reverts it.\n\nReverts %s. Opened by Warden; merged under its own rules once CI is green.\n", n, jobs, sha)
	rp, err := c.CreatePR(ctx, branch, mainBranch(env), title, body, false)
	if err != nil {
		return Result{Summary: "revert-pr: pushed " + branch + " but the PR failed: " + err.Error()}
	}
	res := Result{OK: true, Branch: branch, Summary: fmt.Sprintf("revert PR #%d for #%d (main red at %s: %s)", rp.Number, n, short7(sha), jobs)}
	if err := c.AddLabels(ctx, rp.Number, labelWarden, labelRevert); err != nil {
		res.Log = append(res.Log, "labels: "+err.Error())
	}
	if err := c.Comment(ctx, n, fmt.Sprintf("%s: main went red after this PR (failed: %s). Warden opened #%d to revert it; it merges once CI is green.",
		ownerMention(env), jobs, rp.Number)); err != nil {
		res.Log = append(res.Log, "comment: "+err.Error())
	}
	return res
}

// ---- the optional second review

var reVerdict = regexp.MustCompile(`(?i)VERDICT:\s*(looks-good|changes-needed)`)

// secondReview: with params.review_brain set, Warden's own issue PRs
// (warden/issue-*) get that brain's review of the diff before the merge
// (the reviewer prompt; the diff fenced as data). A verdict other than
// looks-good stops the merge: refused, with the review as a comment on the
// PR. Unset (the default), nothing happens and nothing is called. stop:
// the merge does not go on; otherwise r.Summary notes the review.
func secondReview(ctx context.Context, c *github.Client, env *Env, pr github.PR, class string) (r Result, stop bool) {
	name := param(nil, env, "review_brain", "")
	if name == "" || class != "own" || !strings.HasPrefix(pr.Head.Ref, refIssue) {
		return Result{}, false
	}
	if env.Brains == nil || env.Brains.Get == nil {
		return refused(fmt.Sprintf("refused: PR #%d: review_brain %q is set but no named brains are configured", pr.Number, name)), true
	}
	nb, err := env.Brains.Get(name)
	if err != nil {
		return deferred(fmt.Sprintf("PR #%d: waiting for the review brain %s (%v)", pr.Number, name, err)), true
	}
	diff, err := c.Diff(ctx, pr.Number)
	if err != nil {
		return Result{Summary: "merge-pr: " + err.Error()}, true
	}
	task := fmt.Sprintf("Pull request #%d, written by another model for an issue of this repository; CI is green. Review it before it merges by itself.\n\n%s\n\nThe diff:\n\n%s",
		pr.Number, untrusted(pr.User.Login, "PR title and description", pr.Title+"\n\n"+short(pr.Body, 4000)), untrusted(pr.User.Login, "diff", short(diff, 60000)))
	ans, err := nb.Brain.Do(ctx, systemPrompt(reviewerPrompt, env), task, nil)
	if err != nil {
		return deferred(fmt.Sprintf("PR #%d: the review brain %s failed (%v); tried again next tick", pr.Number, name, err)), true
	}
	review := strings.TrimSpace(ans)
	verdict := "none"
	if m := reVerdict.FindStringSubmatch(review); m != nil {
		verdict = strings.ToLower(m[1])
	}
	if verdict == "looks-good" {
		return Result{Summary: "second review (" + name + "): looks-good"}, false
	}
	review = reMention.ReplaceAllString(review, "@\u200b$1")
	_ = c.Comment(ctx, pr.Number, fmt.Sprintf("**Second review by Warden** (brain: %s) at %s: %s, so this is not merged automatically. Over to %s.\n\n%s\n",
		name, short7(pr.Head.SHA), verdict, ownerMention(env), short(review, 6000)))
	return refused(fmt.Sprintf("refused: PR #%d: the second review (%s) says %s; commented", pr.Number, name, verdict)), true
}

// revertTitle: `Revert "<title>" (#N)`, as git would name it.
func revertTitle(title string, n int) string {
	return "Revert \"" + noMention(oneLine(title, 72)) + "\" (#" + strconv.Itoa(n) + ")"
}
