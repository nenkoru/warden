package builtin

// Task records for the experiment log (internal/experiment): acts fill
// Result.Data, core appends it to <state>/tasks.jsonl; observeReverts
// (github-main) notices a revert of a warden/issue PR and hands core a task
// event to record.

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
)

// experimentOn: params.experiment_log = "on".
func experimentOn(env *Env) bool { return param(nil, env, "experiment_log", "off") == "on" }

// TasksLog: the experiment log's file in a state dir.
func TasksLog(state string) string { return filepath.Join(state, "tasks.jsonl") }

var (
	reRevertTitle  = regexp.MustCompile(`^Revert "(.*)"`)
	reRevertBranch = regexp.MustCompile(`^revert-([0-9]+)-`)
	rePRNumber     = regexp.MustCompile(`#([0-9]+)`)
)

// observeReverts: a merged PR titled `Revert "…"` that undoes a merged
// warden/issue PR (its number in the title or GitHub's revert branch, or its
// title quoted), merged in the last week, is a task event: core records it
// once, the first tick it sees the finding.
func observeReverts(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	closed, err := c.ClosedPRs(ctx, mainBranch(env))
	if err != nil {
		return nil, err
	}
	ours := map[int]github.PR{}
	for _, p := range closed {
		if _, ok := issueOfRef(p.Head.Ref); ok && p.MergedAt != nil {
			ours[p.Number] = p
		}
	}
	var out []domain.Finding
	for _, r := range closed {
		m := reRevertTitle.FindStringSubmatch(r.Title)
		if r.MergedAt == nil || m == nil || r.MergedAt.Before(env.now().Add(-7*24*time.Hour)) { // older reverts predate the log
			continue
		}
		// GitHub's revert branch, then the quoted title, then a number in the
		// title, the last first (a squash commit ends with "(#PR)")
		var cands []github.PR
		if b := reRevertBranch.FindStringSubmatch(r.Head.Ref); b != nil {
			cands = append(cands, ours[atoi(b[1])])
		}
		for _, p := range closed {
			if _, mine := ours[p.Number]; mine && (m[1] == p.Title || strings.HasPrefix(m[1], p.Title+" (#")) {
				cands = append(cands, p)
			}
		}
		nums := rePRNumber.FindAllStringSubmatch(r.Title, -1)
		for i := len(nums) - 1; i >= 0; i-- {
			cands = append(cands, ours[atoi(nums[i][1])])
		}
		orig, found := github.PR{}, false
		for _, p := range cands {
			if p.Number > 0 && p.Number != r.Number {
				orig, found = p, true
				break
			}
		}
		if !found {
			continue
		}
		issue, _ := issueOfRef(orig.Head.Ref)
		f := ghFinding("github-main", "revert|"+strconv.Itoa(r.Number), "revert", "medium", "#"+strconv.Itoa(orig.Number),
			fmt.Sprintf("PR #%d reverted Warden's PR #%d (issue #%d)", r.Number, orig.Number, issue), "")
		f.Links = nonEmptyList(r.HTMLURL)
		f.Data = map[string]string{"task_event": "revert", "issue": strconv.Itoa(issue), "pr": strconv.Itoa(orig.Number), "revert_pr": strconv.Itoa(r.Number)}
		out = append(out, f)
	}
	return out, nil
}
