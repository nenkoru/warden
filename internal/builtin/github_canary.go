package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/update"
)

// The canary gate (params.canary names a fleet instance): a merged release
// PR is tagged vX.Y.Z-rc.N first, a prerelease that only canary-channel
// Wardens install; they report on the "Warden fleet" issue (internal/update).
// Once the canary has run the rc healthy for canary_hours and at least
// canary_min_ticks ticks, and main is still green, the final vX.Y.Z goes on
// the same commit. A rollback of the rc — on the canary, or an open
// warden-rollback issue naming it — stops it: no final tag, an issue for the
// owner, and the next attempt waits for newly merged PRs (a new release PR,
// a new rc number on its merge). Without params.canary nothing changes.

var reRCTag = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+)-rc\.([0-9]+)$`)

func canaryName(env *Env) string { return oneLine(param(nil, env, "canary", ""), 80) }

// canaryHours, canaryMinTicks: how long and how many healthy ticks the
// canary runs an rc before the final tag (canary_hours, canary_min_ticks).
func canaryHours(env *Env) int    { return max(intParam(env, "canary_hours", 24), 0) }
func canaryMinTicks(env *Env) int { return max(intParam(env, "canary_min_ticks", 6), 1) }

// canaryStaleHours: a beacon not rewritten for this long (canary_stale_hours,
// default 3; the beacon is rewritten at least hourly while the canary ticks)
// is a canary that stopped ticking — its health no longer counts.
func canaryStaleHours(env *Env) int { return max(intParam(env, "canary_stale_hours", 3), 1) }

// rcOf: for version v, the rc tag on sha ("" = none) and the rc to cut next
// (rc numbers only go up, across attempts on different commits).
func rcOf(tags []github.Tag, v, sha string) (on, next string) {
	top := 0
	for _, t := range tags {
		m := reRCTag.FindStringSubmatch(t.Name)
		if m == nil || m[1] != v {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if t.Commit.SHA == sha && (on == "" || n > rcNum(on)) {
			on = t.Name
		}
		top = max(top, n)
	}
	return on, fmt.Sprintf("%s-rc.%d", v, top+1)
}

func rcNum(tag string) int {
	m := reRCTag.FindStringSubmatch(tag)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[2])
	return n
}

// rcShort: "rc.N" of vX.Y.Z-rc.N.
func rcShort(tag string) string { return "rc." + strconv.Itoa(rcNum(tag)) }

// canaryInfo: an rc in flight, as the canary's beacon tells it.
type canaryInfo struct {
	Name     string
	RC       string         // the rc tag
	SHA      string         // its commit (the release PR's merge commit)
	Beacon   *update.Beacon // the canary's, from a trusted author (nil = none)
	Running  bool           // the canary runs the rc
	Healthy  time.Duration  // since its healthy_since on the rc
	Hours    int            // canary_hours
	MinTicks int            // canary_min_ticks
	Stale    int            // canary_stale_hours
	Failed   string         // why the rc failed ("" = it did not)
	Silent   bool           // the canary's beacon is older than canary_stale_hours
	Ready    bool           // healthy long enough: the final tag may go on
}

// canaryState reads the fleet beacons — only comments by the token's own
// login or the owner (anyone may comment on a public issue) — and the open
// warden-rollback issues.
func canaryState(ctx context.Context, c *github.Client, env *Env, rc, sha string) (canaryInfo, error) {
	ci := canaryInfo{Name: canaryName(env), RC: rc, SHA: sha, Hours: canaryHours(env), MinTicks: canaryMinTicks(env), Stale: canaryStaleHours(env)}
	rb, err := c.IssuesLabeled(ctx, update.RollbackLabel)
	if err != nil {
		return ci, err
	}
	for _, i := range rb {
		if update.NamesVersion(i.Title, rc) || update.NamesVersion(i.Body, rc) {
			ci.Failed = fmt.Sprintf("an open %s issue (#%d) names it", update.RollbackLabel, i.Number)
			return ci, nil
		}
	}
	bs, err := update.FleetStatus(ctx, c)
	if err != nil {
		return ci, err
	}
	me, _ := c.Login(ctx)
	owner := strings.TrimPrefix(param(nil, env, "owner", ""), "@")
	for i := range bs {
		b := &bs[i]
		trusted := me != "" && strings.EqualFold(b.Author, me) || owner != "" && strings.EqualFold(b.Author, owner)
		if !trusted || !strings.EqualFold(b.Name, ci.Name) {
			continue
		}
		if b.RolledBack != "" && vprefix(b.RolledBack) == rc {
			ci.Beacon, ci.Running, ci.Failed = b, false, fmt.Sprintf("the canary «%s» rolled back from it", ci.Name)
			return ci, nil
		}
		if vprefix(b.Version) != rc {
			if ci.Beacon == nil {
				ci.Beacon = b
			}
			continue
		}
		healthy := time.Duration(0)
		if !b.HealthySince.IsZero() {
			healthy = max(env.now().Sub(b.HealthySince), 0)
		}
		if !ci.Running || healthy < ci.Healthy { // two beacons for one name: the less healthy one counts
			ci.Beacon, ci.Running, ci.Healthy = b, true, healthy
		}
	}
	if ci.Running {
		ci.Silent = ci.Beacon.Updated.IsZero() || env.now().Sub(ci.Beacon.Updated) > time.Duration(ci.Stale)*time.Hour
	}
	ci.Ready = ci.Running && !ci.Silent && ci.Healthy >= time.Duration(ci.Hours)*time.Hour && ci.Beacon.TicksOK >= ci.MinTicks
	return ci, nil
}

// line: "release vX.Y.Z: rc.N on the canary since …, healthy N h of 24".
func (ci canaryInfo) line(v string) string {
	switch {
	case ci.Failed != "":
		return fmt.Sprintf("release %s: %s failed on the canary: %s", v, ci.RC, ci.Failed)
	case ci.Running && ci.Silent:
		since := "an unknown time"
		if !ci.Beacon.Updated.IsZero() {
			since = ci.Beacon.Updated.UTC().Format("2006-01-02 15:04 UTC")
		}
		return fmt.Sprintf("release %s: %s on the canary «%s»: canary silent since %s (no beacon for %dh); no final tag", v, rcShort(ci.RC), ci.Name, since, ci.Stale)
	case ci.Running && ci.Beacon.HealthySince.IsZero():
		return fmt.Sprintf("release %s: %s on the canary «%s», no healthy tick yet", v, rcShort(ci.RC), ci.Name)
	case ci.Running:
		return fmt.Sprintf("release %s: %s on the canary «%s» since %s, healthy %d h of %d (%d of %d ticks)", v, rcShort(ci.RC), ci.Name,
			ci.Beacon.HealthySince.UTC().Format("2006-01-02 15:04 UTC"), int(ci.Healthy.Hours()), ci.Hours, ci.Beacon.TicksOK, ci.MinTicks)
	case ci.Beacon != nil:
		return fmt.Sprintf("release %s: %s tagged on %s; the canary «%s» runs %s, not it yet", v, rcShort(ci.RC), short7(ci.SHA), ci.Name, or(ci.Beacon.Version, "an unknown version"))
	}
	return fmt.Sprintf("release %s: %s tagged on %s; no beacon from the canary «%s» on the %q issue yet", v, rcShort(ci.RC), short7(ci.SHA), ci.Name, update.FleetTitle)
}

// canaryFindings: the merged release PR's way through the canary.
func canaryFindings(env *Env, ri releaseInfo, state string) []domain.Finding {
	u := ri.Untagged
	v := strings.TrimPrefix(u.Head.Ref, refRelease)
	if ri.RC == "" {
		f := ghFinding("github-main", "release-rc|"+ri.NextRC, "release", "medium", v,
			fmt.Sprintf("release %s merged (PR #%d): tag %s for the canary «%s»", v, u.Number, ri.NextRC, canaryName(env)), "")
		if state == "green" && u.MergeCommitSHA != "" {
			f.Fix = &domain.Fix{Act: "release", Target: ri.NextRC, What: "create the tag " + ri.NextRC + " on the release PR's merge commit (a prerelease for the canary)",
				Params: map[string]string{"phase": "rc", "version": v, "rc": ri.NextRC, "sha": u.MergeCommitSHA, "pr": strconv.Itoa(u.Number), "tainted": "false"}}
		} else {
			f.Severity, f.Title = "low", f.Title+" — waiting for green main"
		}
		return []domain.Finding{f}
	}
	ci := ri.Canary
	if ci.Failed != "" {
		next := "The next attempt waits for newly merged PRs: then a new release PR, and a new rc on its merge."
		if len(ri.Fresh) > 0 {
			next = fmt.Sprintf("%d PR(s) merged since; the next attempt is a new release PR.", len(ri.Fresh))
		}
		f := ghFinding("github-main", "release-canary-failed|"+ci.RC, "rollback", "high", v, fmt.Sprintf("release %s failed on the canary: %s", ci.RC, ci.Failed),
			fmt.Sprintf("No %s tag. %s", v, next))
		f.Fix = &domain.Fix{Act: "release", Target: ci.RC, What: "tell the owner (an issue labeled needs-human, once); no final tag",
			Params: map[string]string{"phase": "failed", "version": v, "rc": ci.RC, "sha": ci.SHA, "why": ci.Failed, "tainted": "false"}}
		out := []domain.Finding{f}
		if len(ri.Fresh) > 0 {
			out = append(out, releasePRFinding(ri, state, ri.Fresh, "the failed "+ci.RC, ci.RC))
		}
		return out
	}
	f := ghFinding("github-main", "release-canary|"+ci.RC, "release", "info", v, ci.line(v), "")
	if ci.Silent { // a canary that stopped ticking still looks healthy in its last beacon
		f.Severity = "medium"
		f.Detail = fmt.Sprintf("The canary's beacon on the %q issue was last written more than %dh ago (canary_stale_hours): is «%s» still running?", update.FleetTitle, ci.Stale, ci.Name)
	}
	if ci.Ready {
		f = ghFinding("github-main", "release-final|"+v, "release", "medium", v, ci.line(v), "")
		if state == "green" {
			f.Title += ": tag " + v
			f.Fix = &domain.Fix{Act: "release", Target: v, What: "create the tag " + v + " on the commit of " + ci.RC + " (healthy on the canary)",
				Params: map[string]string{"phase": "final", "version": v, "rc": ci.RC, "sha": ci.SHA, "tainted": "false"}}
		} else {
			f.Severity, f.Title = "low", f.Title+" — the final tag waits for green main ("+state+")"
		}
	}
	return []domain.Finding{f}
}

// tagRC: the release candidate on the release PR's merge commit.
func tagRC(ctx context.Context, c *github.Client, env *Env, ri releaseInfo, p map[string]string) Result {
	if ri.Untagged == nil || strings.TrimPrefix(ri.Untagged.Head.Ref, refRelease) != p["version"] || ri.Untagged.MergeCommitSHA != p["sha"] || ri.RC != "" || ri.NextRC != p["rc"] {
		return Result{OK: true, Summary: "release: nothing to tag (" + p["rc"] + ")"}
	}
	if err := c.CreateTag(ctx, p["rc"], p["sha"]); err != nil {
		return Result{Summary: "release: tag: " + err.Error()}
	}
	return Result{OK: true, Summary: fmt.Sprintf("tagged %s on %s; the release workflow builds a prerelease for the canary «%s»", p["rc"], short7(p["sha"]), canaryName(env))}
}

// tagFinal: vX.Y.Z on the rc's commit, the canary asked again right before.
func tagFinal(ctx context.Context, c *github.Client, ri releaseInfo, p map[string]string) Result {
	if ri.Untagged == nil || strings.TrimPrefix(ri.Untagged.Head.Ref, refRelease) != p["version"] || ri.RC != p["rc"] || ri.Canary.SHA != p["sha"] {
		return Result{OK: true, Summary: "release: nothing to tag (" + p["version"] + ")"}
	}
	ci := ri.Canary
	if ci.Failed != "" {
		return refused(fmt.Sprintf("refused: %s failed on the canary: %s", ci.RC, ci.Failed))
	}
	if !ci.Ready {
		return deferred("release: not yet: " + ci.line(p["version"]))
	}
	if err := c.CreateTag(ctx, p["version"], p["sha"]); err != nil {
		return Result{Summary: "release: tag: " + err.Error()}
	}
	return Result{OK: true, Summary: fmt.Sprintf("tagged %s on %s, the commit of %s (healthy %d h on the canary «%s»); the release workflow builds it",
		p["version"], short7(p["sha"]), ci.RC, int(ci.Healthy.Hours()), ci.Name)}
}

// canaryFailed: the owner hears of a failed rc once (an issue labeled
// needs-human that mentions them), even after closing it.
func canaryFailed(ctx context.Context, c *github.Client, env *Env, ri releaseInfo, p map[string]string) Result {
	rc := p["rc"]
	if ri.RC != rc || ri.Canary.Failed == "" {
		return Result{OK: true, Summary: "release: " + rc + " has not failed on the canary"}
	}
	title := fmt.Sprintf("release %s failed on the canary", rc)
	if told(env, title) {
		return Result{OK: true, Summary: "release: the owner was told about " + rc}
	}
	if is, err := c.IssuesLabeled(ctx, labelNeedsHuman); err == nil {
		for _, i := range is {
			if i.Title == title {
				markTold(env, title)
				return Result{OK: true, Summary: fmt.Sprintf("release: %s failed on the canary (issue #%d)", rc, i.Number)}
			}
		}
	}
	body := fmt.Sprintf("%s: Warden tagged %s on %s for the canary «%s», and it failed there: %s.\n\n"+
		"There is no %s tag, and Warden won't make one from this commit. The next attempt waits for newly merged PRs "+
		"(a fix): then a new release PR, and a new release candidate on its merge.\n", ownerMention(env), rc, short7(ri.Canary.SHA), ri.Canary.Name,
		noMention(ri.Canary.Failed), p["version"])
	i, err := c.CreateIssue(ctx, title, body, labelNeedsHuman)
	if err != nil {
		return Result{Summary: "release: " + err.Error()}
	}
	markTold(env, title)
	return Result{OK: true, Summary: fmt.Sprintf("release: %s failed on the canary; no final tag: issue #%d for the owner", rc, i.Number)}
}

// unrelease: a version that never shipped (its rc failed on the canary)
// goes back under [Unreleased] — its heading and compare link dropped — so
// the next release's section takes its entries along.
func unrelease(cl, version string) string {
	v := strings.TrimPrefix(version, "v")
	var out []string
	inUnreleased, dropped := false, false
	for _, l := range strings.Split(cl, "\n") {
		switch {
		case strings.HasPrefix(l, "## [Unreleased]"):
			inUnreleased = true
		case inUnreleased && strings.HasPrefix(l, "## ["):
			inUnreleased = false
			if strings.HasPrefix(l, "## ["+v+"]") {
				dropped = true
				continue
			}
		case dropped && strings.HasPrefix(l, "["+v+"]: "):
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// told: a local record of what the owner was told once (github/told.json).
func toldPath(env *Env) string { return filepath.Join(env.State, "github", "told.json") }

func toldMap(env *Env) map[string]time.Time {
	m := map[string]time.Time{}
	b, _ := os.ReadFile(toldPath(env))
	_ = json.Unmarshal(b, &m)
	return m
}

func told(env *Env, what string) bool { _, ok := toldMap(env)[what]; return ok }

func markTold(env *Env, what string) {
	m := toldMap(env)
	m[what] = env.now()
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = writeFile(toldPath(env), b)
}
