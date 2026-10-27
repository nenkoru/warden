package builtin

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
)

const (
	labelNeedsHuman = "needs-human"
	labelWarden     = "warden"
	labelDelegated  = "delegated"
	labelFixed      = "warden-fixed" // a draft that had its one fix round
	refIssue        = "warden/issue-"
	refRelease      = "warden/release-"
)

func observeGitHub(ctx context.Context, name string, env *Env) ([]domain.Finding, error) {
	if env == nil {
		env = &Env{}
	}
	c, err := ghClient(env)
	if err != nil {
		return nil, err
	}
	switch name {
	case "github-pause":
		return observePause(ctx, c, env)
	case "github-issues":
		return observeIssues(ctx, c, env)
	case "github-prs":
		return observePRs(ctx, c, env)
	}
	return observeMain(ctx, c, env)
}

func ghFinding(step, key, kind, sev, subject, title, detail string) domain.Finding {
	return finding(step, key, kind, sev, subject, title, detail)
}

func mainBranch(env *Env) string { return param(nil, env, "branch", "main") }

// ---- the kill switch

func observePause(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	label := param(nil, env, "pause_label", "warden-pause")
	var out []domain.Finding
	is, err := c.IssuesLabeled(ctx, label)
	if err != nil {
		return nil, err
	}
	for _, i := range is {
		out = append(out, ghFinding("github-pause", "pause|issue|"+strconv.Itoa(i.Number), "pause", "high", "#"+strconv.Itoa(i.Number),
			fmt.Sprintf("issue #%d carries the %s label", i.Number, label), i.HTMLURL))
	}
	ok, err := c.FileExists(ctx, "PAUSE", mainBranch(env))
	if err != nil {
		return out, err
	}
	if ok {
		out = append(out, ghFinding("github-pause", "pause|file", "pause", "high", "PAUSE", "a PAUSE file exists at the root of "+mainBranch(env), ""))
	}
	return out, nil
}

// ---- issues

var trustedAssoc = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

func observeIssues(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	issues, err := c.OpenIssues(ctx)
	if err != nil {
		return nil, err
	}
	prs, err := c.OpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	open := map[int]int{} // issue → its warden PR
	busy := false         // one issue at a time: a warden/issue PR still in flight
	for _, p := range prs {
		if n, ok := issueOfRef(p.Head.Ref); ok {
			open[n] = p.Number
			if !p.Has(labelNeedsHuman) {
				busy = true
			}
		}
	}
	skip := listParam(env, "skip_labels")
	if len(skip) == 0 {
		skip = []string{labelNeedsHuman, "wontfix", "question", "warden-pause", "warden-log", labelDelegated}
	}
	work := listParam(env, "work_labels")
	if len(work) == 0 {
		work = []string{"good first issue", "bug", "enhancement"}
	}
	delegates := dictParam(env, "delegate_labels")
	canDelegate := len(listParam(env, "delegate_cmd")) > 0
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	var out []domain.Finding
	for _, is := range issues {
		n := strconv.Itoa(is.Number)
		labels := github.LabelNames(is.Labels)
		f := ghFinding("github-issues", "issue|"+n, "issue", "info", "#"+n, "issue #"+n+": "+oneLine(is.Title, 100),
			untrusted(is.User.Login, "issue #"+n, is.Title+"\n\n"+short(is.Body, 2000)))
		f.Links = nonEmptyList(is.HTMLURL)
		f.Data = map[string]string{"number": n, "author": is.User.Login, "association": is.AuthorAssociation, "labels": strings.Join(labels, ","),
			"tainted": "true", "url": is.HTMLURL}
		out = append(out, f)
		fp := &out[len(out)-1]
		if anyLabel(labels, skip) {
			continue
		}
		if pr, ok := open[is.Number]; ok {
			fp.Title += fmt.Sprintf(" — PR #%d open", pr)
			continue
		}
		actionable := is.Has(labelWarden) || anyLabel(labels, work) && trustedAssoc[is.AuthorAssociation]
		if !actionable {
			continue
		}
		fp.Severity = "low"
		small := smallIssue(env, labels, is.Title)
		agent := ""
		for _, l := range labels {
			if a := delegates[l]; a != "" {
				agent = a
				break
			}
		}
		if agent == "" && !small && canDelegate {
			if r := PickBrains(env, "work-issue", false); r.Hard {
				agent = param(nil, env, "delegate_default", "")
			}
		}
		params := map[string]string{"number": n, "title": oneLine(is.Title, 120), "labels": strings.Join(labels, ","), "author": is.User.Login,
			"small": strconv.FormatBool(small), "tainted": "true", "url": is.HTMLURL}
		switch {
		case agent != "" && canDelegate:
			params["agent"] = agent
			fp.Fix = &domain.Fix{Act: "delegate", Target: "#" + n, What: fmt.Sprintf("delegate issue #%d to %s", is.Number, agent), Params: params}
		case busy:
			fp.Title += " (queued: one issue at a time)"
		default:
			busy = true
			fp.Fix = &domain.Fix{Act: "work-issue", Target: "#" + n, What: fmt.Sprintf("work on issue #%d in a branch and open a PR", is.Number), Params: params}
		}
	}
	return out, nil
}

var reIssueRef = regexp.MustCompile(`^` + regexp.QuoteMeta(refIssue) + `([0-9]+)-`)

func issueOfRef(ref string) (int, bool) {
	m := reIssueRef.FindStringSubmatch(ref)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

func anyLabel(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if strings.EqualFold(h, w) {
				return true
			}
		}
	}
	return false
}

func nonEmptyList(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- pull requests

// prClass: own (the token's user), dependabot, trusted (param
// trusted_authors: trusted but verified, merged under Warden's own rules),
// or external (review only).
func prClass(author, login string, trusted []string) string {
	switch author {
	case login:
		return "own"
	case "dependabot[bot]":
		return "dependabot"
	}
	for _, t := range trusted {
		if strings.EqualFold(strings.TrimPrefix(t, "@"), author) {
			return "trusted"
		}
	}
	return "external"
}

func observePRs(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	login, err := c.Login(ctx)
	if err != nil {
		return nil, err
	}
	prs, err := c.OpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	trusted := listParam(env, "trusted_authors")
	required := listParam(env, "required_checks")
	canDelegate := len(listParam(env, "delegate_cmd")) > 0
	heads := map[string]bool{}
	var out []domain.Finding
	for _, p := range prs {
		heads[p.Head.Ref] = true
		n := strconv.Itoa(p.Number)
		class := prClass(p.User.Login, login, trusted)
		title := fmt.Sprintf("PR #%d: %s (@%s, %s)", p.Number, oneLine(p.Title, 100), p.User.Login, class)
		f := ghFinding("github-prs", "pr|"+n+"|"+p.Head.SHA, "pr", "info", "#"+n, title, "")
		f.Links = nonEmptyList(p.HTMLURL)
		f.Data = map[string]string{"number": n, "author": p.User.Login, "class": class, "sha": p.Head.SHA, "ref": p.Head.Ref,
			"draft": strconv.FormatBool(p.Draft), "labels": strings.Join(github.LabelNames(p.Labels), ","), "tainted": strconv.FormatBool(class == "external")}
		if class == "external" {
			f.Detail = untrusted(p.User.Login, "PR #"+n, p.Title+"\n\n"+short(p.Body, 1500))
		}
		params := map[string]string{"number": n, "sha": p.Head.SHA, "class": class, "tainted": f.Data["tainted"]}
		if p.Has(labelNeedsHuman) {
			f.Severity, f.Title = "high", f.Title+" — waits for you (needs-human)"
			f.Data["waiting"] = "owner"
			out = append(out, f)
			continue
		}
		state, names, err := ciState(ctx, c, p.Head.SHA, required)
		if err != nil {
			return out, err
		}
		f.Data["ci"] = state
		switch {
		case class == "external" && state == "pending":
			f.Title += " — waiting for CI before the review"
		case class == "external":
			f.Fix = &domain.Fix{Act: "review-pr", Target: "#" + n, What: fmt.Sprintf("review PR #%d (diff and CI results; its code never runs here)", p.Number), Params: params}
		case state == "pending":
			f.Title += " — CI running"
		case state == "none":
			f.Title += " — no CI checks reported: not merged without them"
		case state == "red":
			f.Severity, f.Title = "medium", f.Title+" — CI red: "+strings.Join(names, ", ")
			issue, ours := issueOfRef(p.Head.Ref)
			switch {
			case class != "own" || !ours:
				if strings.HasPrefix(p.Head.Ref, refRelease) {
					f.Severity = "high"
				}
			case !p.Has(labelFixed):
				params["issue"], params["phase"], params["pr"] = strconv.Itoa(issue), "fix", n
				f.Fix = &domain.Fix{Act: "work-issue", Target: "#" + strconv.Itoa(issue), What: fmt.Sprintf("fix PR #%d from its CI logs (one round)", p.Number), Params: params}
			case canDelegate && param(nil, env, "delegate_default", "") != "":
				agent := param(nil, env, "delegate_default", "")
				params["issue"], params["pr"], params["agent"], params["number"] = strconv.Itoa(issue), n, agent, strconv.Itoa(issue)
				params["tainted"] = "true"
				f.Fix = &domain.Fix{Act: "delegate", Target: "#" + strconv.Itoa(issue), What: fmt.Sprintf("hand issue #%d to %s (PR #%d stayed red after a fix round)", issue, agent, p.Number), Params: params}
			default:
				params["issue"], params["phase"], params["pr"] = strconv.Itoa(issue), "giveup", n
				f.Fix = &domain.Fix{Act: "work-issue", Target: "#" + strconv.Itoa(issue), What: fmt.Sprintf("PR #%d stayed red after a fix round: hand it to the owner", p.Number), Params: params}
			}
		case state == "green":
			f.Title += " — CI green"
			f.Fix = &domain.Fix{Act: "merge-pr", Target: "#" + n, What: fmt.Sprintf("merge PR #%d (squash) if every rule holds", p.Number), Params: params}
		}
		out = append(out, f)
	}
	if prefixes := listParam(env, "pickup_branches"); len(prefixes) > 0 {
		bs, err := c.Branches(ctx)
		if err != nil {
			return out, err
		}
		for _, b := range bs {
			if heads[b.Name] || !hasAnyPrefix(b.Name, prefixes) {
				continue
			}
			f := ghFinding("github-prs", "branch|"+b.Name+"|"+b.Commit.SHA, "branch", "low", b.Name, "branch "+oneLine(b.Name, 80)+" has no PR", "")
			f.Fix = &domain.Fix{Act: "pickup-branch", Target: b.Name, What: "open a draft PR for branch " + b.Name,
				Params: map[string]string{"branch": b.Name, "sha": b.Commit.SHA, "tainted": "false"}}
			out = append(out, f)
		}
	}
	return out, nil
}

func hasAnyPrefix(s string, ps []string) bool {
	for _, p := range ps {
		if p != "" && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ---- main: CI and releases

// mainState: CI on main's head commit (the latest run of each workflow).
func mainState(ctx context.Context, c *github.Client, env *Env) (sha, state string, red []github.WorkflowRun, err error) {
	sha, err = c.BranchSHA(ctx, mainBranch(env))
	if err != nil {
		return "", "", nil, err
	}
	runs, err := c.Runs(ctx, mainBranch(env))
	if err != nil {
		return sha, "", nil, err
	}
	latest := map[string]github.WorkflowRun{}
	for _, r := range runs {
		if r.HeadSHA != sha || r.Event == "pull_request" {
			continue
		}
		k := r.Name
		if r.WorkflowID != 0 {
			k = strconv.FormatInt(r.WorkflowID, 10)
		}
		if old, ok := latest[k]; !ok || r.CreatedAt.After(old.CreatedAt) {
			latest[k] = r
		}
	}
	state = "none"
	if len(latest) > 0 {
		state = "green"
	}
	pending := false
	for _, r := range latest {
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion != "success" && r.Conclusion != "skipped" && r.Conclusion != "neutral":
			red = append(red, r)
		}
	}
	sort.Slice(red, func(i, j int) bool { return red[i].Name < red[j].Name })
	switch {
	case len(red) > 0:
		state = "red"
	case pending:
		state = "pending"
	}
	return sha, state, red, nil
}

var reVersionTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// latestTag: the highest vX.Y.Z tag ("" with none).
func latestTag(tags []github.Tag) (github.Tag, bool) {
	var best github.Tag
	ok := false
	for _, t := range tags {
		if reVersionTag.MatchString(t.Name) && (!ok || semverLess(best.Name, t.Name)) {
			best, ok = t, true
		}
	}
	return best, ok
}

// releaseInfo: what is merged since the latest tag.
type releaseInfo struct {
	Tag      string // the latest vX.Y.Z ("" = none)
	Merged   []github.PR
	Open     *github.PR // an open release PR
	Untagged *github.PR // a merged release PR whose tag does not exist yet
	Version  string     // the next version
	Breaking bool
	// with the canary gate (params.canary; github_canary.go), for Untagged:
	RC     string      // the rc tag on its merge commit ("" = none yet)
	NextRC string      // the rc tag to cut there
	Canary canaryInfo  // RC on the canary
	Fresh  []github.PR // merged after it (what a new attempt waits for)
}

func releaseState(ctx context.Context, c *github.Client, env *Env) (releaseInfo, error) {
	var ri releaseInfo
	tags, err := c.Tags(ctx)
	if err != nil {
		return ri, err
	}
	have := map[string]bool{}
	for _, t := range tags {
		have[t.Name] = true
	}
	var since time.Time
	if t, ok := latestTag(tags); ok {
		ri.Tag = t.Name
		if since, err = c.CommitDate(ctx, t.Commit.SHA); err != nil {
			return ri, err
		}
	}
	closed, err := c.ClosedPRs(ctx, mainBranch(env))
	if err != nil {
		return ri, err
	}
	for i, p := range closed {
		if p.MergedAt == nil {
			continue
		}
		if v, ok := strings.CutPrefix(p.Head.Ref, refRelease); ok {
			if !have[v] && reVersionTag.MatchString(v) && (ri.Tag == "" || semverLess(ri.Tag, v)) && (ri.Untagged == nil || ri.Untagged.MergedAt.Before(*p.MergedAt)) {
				ri.Untagged = &closed[i] // the latest attempt (a failed rc is followed by a new release PR)
			}
			continue
		}
		if p.MergedAt.After(since) {
			ri.Merged = append(ri.Merged, p)
		}
	}
	sort.Slice(ri.Merged, func(i, j int) bool { return ri.Merged[i].MergedAt.Before(*ri.Merged[j].MergedAt) })
	open, err := c.OpenPRs(ctx)
	if err != nil {
		return ri, err
	}
	for i, p := range open {
		if strings.HasPrefix(p.Head.Ref, refRelease) {
			ri.Open = &open[i]
		}
	}
	ri.Version, ri.Breaking = nextVersion(ri.Tag, ri.Merged)
	if canaryName(env) == "" || ri.Untagged == nil {
		return ri, nil
	}
	// rc tags are not releases (latestTag skips them); one in flight holds
	// the next release PR until it fails on the canary
	ri.RC, ri.NextRC = rcOf(tags, strings.TrimPrefix(ri.Untagged.Head.Ref, refRelease), ri.Untagged.MergeCommitSHA)
	for _, p := range ri.Merged {
		if p.MergedAt.After(*ri.Untagged.MergedAt) {
			ri.Fresh = append(ri.Fresh, p)
		}
	}
	if ri.RC != "" {
		ri.Canary, err = canaryState(ctx, c, env, ri.RC, ri.Untagged.MergeCommitSHA)
	}
	return ri, err
}

var (
	reFeat     = regexp.MustCompile(`(?i)^feat(\(.+\))?:`)
	reBreaking = regexp.MustCompile(`(?i)^[a-z]+(\(.+\))?!:|BREAKING[ -]CHANGE`)
)

// nextVersion: feat/enhancement → minor, anything else → patch; breaking
// (a label, "type!:" or BREAKING CHANGE) → a major Warden never cuts.
func nextVersion(tag string, merged []github.PR) (string, bool) {
	if tag == "" {
		return "v0.1.0", false // a placeholder: with no tag the first release is the owner's (firstRelease)
	}
	v, _ := splitVer(tag)
	level := 0 // 0 patch, 1 minor, 2 major
	for _, p := range merged {
		labels := github.LabelNames(p.Labels)
		switch {
		case anyLabel(labels, []string{"breaking", "major", "breaking-change"}) || reBreaking.MatchString(p.Title) || strings.Contains(p.Body, "BREAKING CHANGE"):
			level = max(level, 2)
		case anyLabel(labels, []string{"feat", "feature", "enhancement"}) || reFeat.MatchString(p.Title):
			level = max(level, 1)
		}
	}
	switch level {
	case 2:
		return fmt.Sprintf("v%d.0.0", v[0]+1), true
	case 1:
		return fmt.Sprintf("v%d.%d.0", v[0], v[1]+1), false
	}
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]+1), false
}

func observeMain(ctx context.Context, c *github.Client, env *Env) ([]domain.Finding, error) {
	sha, state, red, err := mainState(ctx, c, env)
	if err != nil {
		return nil, err
	}
	var out []domain.Finding
	if state == "red" {
		var names, links []string
		for _, r := range red {
			names = append(names, r.Name+" ("+r.Conclusion+")")
			links = append(links, r.HTMLURL)
		}
		f := ghFinding("github-main", "main-red|"+sha, "check", "high", mainBranch(env), mainBranch(env)+" is red at "+short7(sha)+": "+strings.Join(names, ", "),
			"No release while main is red.")
		f.Links = links
		f.Data = map[string]string{"sha": sha}
		out = append(out, f)
	}
	if param(nil, env, "release", "auto") == "off" {
		return out, nil
	}
	ri, err := releaseState(ctx, c, env)
	if err != nil {
		return out, err
	}
	switch {
	case ri.Untagged != nil && canaryName(env) != "":
		out = append(out, canaryFindings(env, ri, state)...)
	case ri.Untagged != nil:
		v := strings.TrimPrefix(ri.Untagged.Head.Ref, refRelease)
		f := ghFinding("github-main", "release-tag|"+v, "release", "medium", v, fmt.Sprintf("release %s merged (PR #%d): tag it", v, ri.Untagged.Number), "")
		if state == "green" && ri.Untagged.MergeCommitSHA != "" {
			f.Fix = &domain.Fix{Act: "release", Target: v, What: "create the tag " + v + " on the release PR's merge commit",
				Params: map[string]string{"phase": "tag", "version": v, "sha": ri.Untagged.MergeCommitSHA, "pr": strconv.Itoa(ri.Untagged.Number), "tainted": "false"}}
		} else {
			f.Severity, f.Title = "low", f.Title+" — waiting for green main"
		}
		out = append(out, f)
	case len(ri.Merged) > 0:
		out = append(out, releasePRFinding(ri, state, ri.Merged, or(ri.Tag, "the start"), ""))
	}
	for _, tag := range nonEmptyList(ri.Tag, ri.RC) {
		runs, err := c.Runs(ctx, tag)
		if err != nil {
			return out, err
		}
		if len(runs) > 0 {
			r := runs[0]
			switch {
			case r.Status == "completed" && r.Conclusion != "success" && r.Conclusion != "skipped":
				f := ghFinding("github-main", fmt.Sprintf("release-failed|%s|%d", tag, r.ID), "release", "critical", tag,
					fmt.Sprintf("release %s failed: %s (%s)", tag, r.Name, r.Conclusion), "The tag exists; the release workflow did not finish. Look at the run and re-run it or fix forward.")
				f.Links = nonEmptyList(r.HTMLURL)
				out = append(out, f)
			case r.Status != "completed":
				out = append(out, ghFinding("github-main", "release-running|"+tag, "release", "info", tag, "release "+tag+": the workflow is running", ""))
			}
		}
	}
	return out, nil
}

// releasePRFinding: merged PRs waiting for a release PR (after names a
// failed rc the new attempt follows: its own key, so it is not "done").
func releasePRFinding(ri releaseInfo, state string, merged []github.PR, since, after string) domain.Finding {
	title := fmt.Sprintf("%d merged PR(s) since %s, unreleased → %s", len(merged), since, ri.Version)
	key := "release|" + ri.Version
	if after != "" {
		key += "|after|" + after
	}
	f := ghFinding("github-main", key, "release", "medium", ri.Version, title, prList(merged))
	switch {
	case ri.Open != nil:
		f.Severity, f.Title = "info", title+fmt.Sprintf(" — release PR #%d open", ri.Open.Number)
	case state != "green":
		f.Severity, f.Title = "low", title+" — waiting for green main ("+state+")"
	case ri.Tag == "":
		f.Title = fmt.Sprintf("%d merged PR(s), no release tag yet: the first release is yours", len(merged))
		f.Fix = &domain.Fix{Act: "release", Target: "first", What: "ask the owner to cut the first release (an issue labeled needs-human)",
			Params: map[string]string{"phase": "pr", "version": ri.Version, "first": "true", "tainted": "false"}}
	default:
		f.Fix = &domain.Fix{Act: "release", Target: ri.Version, What: "open the release PR for " + ri.Version + " (CHANGELOG from the merged PR titles)",
			Params: map[string]string{"phase": "pr", "version": ri.Version, "breaking": strconv.FormatBool(ri.Breaking), "tainted": "false"}}
		if after != "" {
			f.Fix.Params["after"] = after
		}
	}
	return f
}

func prList(ps []github.PR) string {
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, "- #%d %s\n", p.Number, oneLine(p.Title, 100))
	}
	return strings.TrimSpace(b.String())
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func or(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
