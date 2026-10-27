package builtin

// The 27B experiment log's publication: the github-experiment observer
// proposes publish-log once a day when the state has task records;
// publish-log renders them (internal/experiment) into params.experiment_doc
// on a warden/experiment-YYYY-MM-DD branch and opens a PR that touches only
// that file, which merge-pr merges on green CI like any of Warden's own PRs.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/experiment"
	"github.com/nenkoru/warden/internal/github"
)

const (
	refExperiment   = "warden/experiment-"
	labelExperiment = "experiment"
)

func experimentDoc(env *Env) string {
	return param(nil, env, "experiment_doc", "docs/experiment-27b.md")
}

// experimentTZ: the zone days and weeks are counted in (params.experiment_tz;
// default the machine's).
func experimentTZ(env *Env) *time.Location {
	if tz := param(nil, env, "experiment_tz", ""); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.Local
}

func observeExperiment(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	if !experimentOn(env) || env.State == "" {
		return nil, nil
	}
	evs, err := experiment.Load(TasksLog(env.State))
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	prs, err := c.OpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		if strings.HasPrefix(p.Head.Ref, refExperiment) {
			return []domain.Finding{ghFinding("github-experiment", "experiment-open|"+strconv.Itoa(p.Number), "experiment", "info", "#"+strconv.Itoa(p.Number),
				fmt.Sprintf("the experiment log's PR #%d is open", p.Number), "")}, nil
		}
	}
	if dailyCount(env, "publish-log") > 0 {
		return nil, nil
	}
	day := env.now().In(experimentTZ(env)).Format(time.DateOnly)
	doc := experimentDoc(env)
	f := ghFinding("github-experiment", "experiment|"+day, "experiment", "info", doc, fmt.Sprintf("the experiment log for %s (%d records)", day, len(evs)), "")
	f.Fix = &domain.Fix{Act: "publish-log", Target: doc, What: "publish " + doc + " for " + day + " in its own PR",
		Params: map[string]string{"day": day, "tainted": "false"}}
	return []domain.Finding{f}, nil
}

// publishLog: the log rendered from the state, in a PR of its own.
func publishLog(ctx context.Context, c *github.Client, f domain.Finding, env *Env) Result {
	if !experimentOn(env) {
		return refused("refused: params.experiment_log is off")
	}
	doc := filepath.ToSlash(filepath.Clean(experimentDoc(env)))
	if !filepath.IsLocal(doc) || !strings.HasSuffix(doc, ".md") {
		return refused("refused: experiment_doc must be a relative .md path in the repository: " + doc)
	}
	if prot := protectedFor(env, []string{doc}); len(prot) > 0 {
		return refused("refused: experiment_doc is a protected path: " + doc)
	}
	evs, err := experiment.Load(TasksLog(env.State))
	if err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	if len(evs) == 0 {
		return Result{OK: true, Summary: "publish-log: nothing recorded yet"}
	}
	loc := experimentTZ(env)
	now := env.now()
	day := now.In(loc).Format(time.DateOnly)
	md := experiment.Aggregate(evs, loc).Markdown(now)
	root, _ := filepath.Abs(param(nil, env, "root", "."))
	work := filepath.Join(env.State, "work")
	wt := filepath.Join(work, "experiment")
	branch := refExperiment + day
	if err := worktreeFrom(ctx, c, env, root, wt, branch, mainBranch(env), "FETCH_HEAD"); err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	defer dropWorktree(ctx, root, work, wt)
	p := filepath.Join(wt, filepath.FromSlash(doc))
	if old, err := os.ReadFile(p); err == nil && string(old) == md {
		dailyAdd(env, "publish-log")
		return Result{OK: true, Summary: fmt.Sprintf("publish-log: no change (%s on %s is current)", doc, mainBranch(env))}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	if _, err := git(ctx, wt, "add", "--", doc); err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	title := "docs: 27B experiment log for " + day
	if _, err := git(ctx, wt, "commit", "-q", "--no-verify", "-m", title); err != nil {
		return Result{Summary: "publish-log: " + err.Error()}
	}
	if r, failed := pushOrHandOff(ctx, c, env, wt, branch, 0, []string{doc}, false); failed {
		return r
	}
	body := fmt.Sprintf("The 27B experiment log, rendered from Warden's task records (%d) without a model. It touches only `%s`, "+
		"so it merges under `merge-pr`'s rules once CI is green.\n", len(evs), doc)
	pr, err := c.CreatePR(ctx, branch, mainBranch(env), title, body, false)
	if err != nil {
		return Result{Summary: "publish-log: pushed " + branch + " but the PR failed: " + err.Error()}
	}
	_ = c.AddLabels(ctx, pr.Number, labelWarden, labelExperiment)
	dailyAdd(env, "publish-log")
	return Result{OK: true, Branch: branch, Summary: fmt.Sprintf("experiment log PR #%d for %s (%d records)", pr.Number, day, len(evs))}
}
