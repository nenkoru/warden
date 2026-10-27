package builtin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/version"
)

const (
	labelDigest = "warden-digest" // the pinned "Warden digest" issue Warden writes to the owner on
	labelLog    = "warden-log"    // its older "Warden log" issue: left as it is (Warden can't edit or close issues)
)

// GitHubNote is the github-repo pack's notifier, run as a hook command
// (`warden github-note --config … --event Report`, the event JSON on
// stdin). It needs no new credential: it comments on a pinned "Warden
// digest" issue of the repository with the same token, and every comment
// that mentions the owner reaches their mail through GitHub's own
// notifications (Watch → Custom → Releases + Mentions).
//
//	Report  a comment mentioning the owner, only when the report has
//	        something urgent (payload "urgent": security, a failed release
//	        or a rollback, budget alerts, anything waiting for them); the
//	        rest of "attention" waits for the digest
//	Ask     a comment mentioning the owner: what waits, and the answer
//	Digest  one comment a day mentioning the owner: spend, the running
//	        version and update state, counts, what needs them (asks, kernel
//	        PRs, needs-human), extra lines (the 27B experiment's day), the
//	        day's lines (never twice a day)
//
// Release notes (the CHANGELOG.md section of a new version) come from the
// release act and the github-main observer: releaseNote.
//
// Text from the payload can come from outsiders (issue titles): every @ in
// it is defused, so the only mention is the owner's.
func GitHubNote(ctx context.Context, params map[string]string, event string, payload map[string]any, web *Web) (string, error) {
	env := &Env{Params: params, Web: web}
	c, err := ghClient(env)
	if err != nil {
		return "", err
	}
	str := func(k string) string { s, _ := payload[k].(string); return defuse(s) }
	list := func(k string) []string {
		var out []string
		if xs, ok := payload[k].([]any); ok {
			for _, x := range xs {
				if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, defuse(s))
				}
			}
		}
		return out
	}
	num := func(k string) int { f, _ := payload[k].(float64); return int(f) }
	ver := or(str("version"), version.Version)
	mention := ownerMention(env)
	var b strings.Builder
	marker, n := "", 0
	switch event {
	case "Report":
		urgent := list("urgent")
		if len(urgent) == 0 {
			return "", nil // nothing urgent for the owner: it goes into the daily digest
		}
		fmt.Fprintf(&b, "%s — needs your attention:\n\n", mention)
		for _, a := range urgent {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		if rest := len(list("attention")) - len(urgent); rest > 0 {
			fmt.Fprintf(&b, "\n%d more for the daily digest.\n", rest)
		}
		b.WriteString(footer(ver, str("summary"), str("spend")))
	case "Ask":
		fmt.Fprintf(&b, "%s — waiting for you: **%s**\n\n- proposed: %s\n- why not alone: %s\n- `%s` · `%s`\n", mention, str("title"), str("what"), str("why"), str("approve"), str("decline"))
		b.WriteString(footer(ver))
	case "Digest":
		day := str("day")
		marker = "<!-- warden-digest " + day + " -->"
		if n, err = digestIssue(ctx, c, env, true); err != nil {
			return "", err
		}
		done, err := noted(ctx, c, n, marker, env.now().Add(-7*24*time.Hour))
		if err != nil {
			return "", err
		}
		if done {
			return fmt.Sprintf("the digest for %s is on issue #%d already", day, n), nil
		}
		fmt.Fprintf(&b, "%s — **Warden daily digest, %s**\n\n", mention, day)
		if s := str("spend"); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if u := str("update"); u != "" { // the running version and the self-update state
			fmt.Fprintf(&b, "Warden %s\n\n", u)
		}
		fmt.Fprintf(&b, "✅ %d done · ⛔ %d failed · ❓ %d waiting\n\n### Needs you\n\n", num("done"), num("failed"), num("waiting"))
		// the owner's asks, then the kernel PRs only the owner merges (with
		// their kernel files), then the rest labeled needs-human
		needs := list("needs")
		if kl, err := KernelPRs(ctx, c); err != nil {
			needs = append(needs, "(kernel PRs could not be listed: "+defuse(oneLine(err.Error(), 120))+")")
		} else {
			for _, l := range kl {
				needs = append(needs, inert(l)) // PR titles are anyone's (KernelPRs defused the mentions)
			}
		}
		needs = append(needs, needsHuman(ctx, c)...)
		if len(needs) == 0 {
			b.WriteString("Nothing waits for you.\n")
		}
		for _, l := range needs {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		if extra := list("extra"); len(extra) > 0 {
			b.WriteString("\n### Also\n\n")
			for _, l := range extra {
				fmt.Fprintf(&b, "- %s\n", l)
			}
		}
		b.WriteString("\n### The day\n\n")
		lines := list("lines")
		if len(lines) == 0 {
			b.WriteString("A quiet day.\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		b.WriteString(footer(ver))
	default:
		fmt.Fprintf(&b, "**%s** %s\n\n%s\n", event, str("title"), str("summary"))
		b.WriteString(footer(ver))
	}
	if n == 0 {
		if n, err = digestIssue(ctx, c, env, true); err != nil {
			return "", err
		}
	}
	if marker != "" {
		b.WriteString(marker + "\n")
	}
	if err := c.Comment(ctx, n, b.String()); err != nil {
		return "", err
	}
	return fmt.Sprintf("noted on issue #%d", n), nil
}

// defuse: payload or repository text made inert for a comment — no
// mention, no fence marker, no HTML comment (a forged dedupe marker).
func defuse(s string) string { return noMention(inert(s)) }

// inert: no fence marker, no HTML comment (for text already without mentions).
func inert(s string) string { return reFence.Replace(strings.ReplaceAll(s, "<!--", "&lt;!--")) }

// footer: "<sub>… · warden VERSION</sub>".
func footer(ver string, parts ...string) string {
	var ps []string
	for _, p := range parts {
		if p != "" {
			ps = append(ps, p)
		}
	}
	return fmt.Sprintf("\n<sub>%s</sub>\n", strings.Join(append(ps, "warden "+ver), " · "))
}

// needsHuman: the open issues and PRs labeled needs-human, one linked line
// each (but kernel PRs: KernelPRs lists them with their kernel files).
func needsHuman(ctx context.Context, c *github.Client) []string {
	var out []string
	is, err := c.IssuesLabeled(ctx, labelNeedsHuman)
	if err != nil {
		return []string{"(needs-human could not be listed: " + defuse(oneLine(err.Error(), 120)) + ")"}
	}
	for _, i := range is {
		out = append(out, fmt.Sprintf("issue [#%d](%s) %s", i.Number, i.HTMLURL, defuse(oneLine(i.Title, 100))))
	}
	ps, err := c.OpenPRs(ctx)
	if err != nil {
		return append(out, "(needs-human PRs could not be listed: "+defuse(oneLine(err.Error(), 120))+")")
	}
	for _, p := range ps {
		if p.Has(labelNeedsHuman) && !p.Has(labelKernel) { // kernel PRs have their own lines (KernelPRs)
			out = append(out, fmt.Sprintf("PR [#%d](%s) %s", p.Number, p.HTMLURL, defuse(oneLine(p.Title, 100))))
		}
	}
	return out
}

// noted: whether a comment of the token's own on issue n (updated since
// `since`) carries marker.
func noted(ctx context.Context, c *github.Client, n int, marker string, since time.Time) (bool, error) {
	login, err := c.Login(ctx)
	if err != nil {
		return false, err
	}
	cs, err := c.CommentsSince(ctx, n, since)
	if err != nil {
		return false, err
	}
	for _, cm := range cs {
		if cm.User.Login == login && strings.Contains(cm.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// digestIssue: the open "Warden digest" issue (label warden-digest) by the
// token's login, made and pinned on first use when create (else 0 when
// there is none). An open "Warden log" issue from before stays as it is.
func digestIssue(ctx context.Context, c *github.Client, env *Env, create bool) (int, error) {
	login, err := c.Login(ctx)
	if err != nil {
		return 0, err
	}
	is, err := c.IssuesLabeled(ctx, labelDigest)
	if err != nil {
		return 0, err
	}
	for _, i := range is {
		if i.User.Login == login {
			return i.Number, nil
		}
	}
	if !create {
		return 0, nil
	}
	body := "Warden's notes to the owner of this repository: one daily digest (what it did, what failed, what waits for you), " +
		"the release notes of each new version, and a ping right away only for what can't wait: security findings, a failed release " +
		"or a rollback, budget alerts, and anything waiting for a human. Each of these comments mentions " + ownerMention(env) + ".\n\n" +
		"**To get them by email** (no other service, no SMTP): on the repository page, **Watch → Custom → Releases + Mentions** " +
		"(and keep email on for participating and @mentions in your GitHub notification settings).\n\n" +
		"To pause Warden, label any issue `warden-pause` or add a `PAUSE` file at the root of the default branch."
	i, err := c.CreateIssue(ctx, "Warden digest", body, labelDigest)
	if err != nil {
		return 0, err
	}
	_ = c.Pin(ctx, i.NodeID) // best effort: pinning is GraphQL-only and cosmetic
	return i.Number, nil
}

// releaseNote: the CHANGELOG.md section of version (read at ref) as a
// comment mentioning the owner on the digest issue, once per version (a
// marker in the comments since `since`). Only when the digest issue exists:
// the owner reads Warden there. The section is repository text (from PR
// titles): defused, and never fenced as anything to run. 0 = not posted.
func releaseNote(ctx context.Context, c *github.Client, env *Env, ver, ref, what string, since time.Time) (int, error) {
	n, err := digestIssue(ctx, c, env, false)
	if err != nil || n == 0 {
		return 0, err
	}
	marker := "<!-- warden-release " + ver + " -->"
	if done, err := noted(ctx, c, n, marker, since); err != nil || done {
		return 0, err
	}
	notes := "(CHANGELOG.md has no section for " + ver + ")"
	if text, err := c.FileContent(ctx, "CHANGELOG.md", ref); err == nil {
		if s := changelogSection(text, ver); s != "" {
			notes = defuse(short(s, 6000))
		}
	}
	body := fmt.Sprintf("%s — **%s** %s.\n\n%s\n\n[The release](https://github.com/%s/releases/tag/%s)\n%s%s\n",
		ownerMention(env), ver, what, notes, param(nil, env, "repo", ""), ver, footer(version.Version), marker)
	if err := c.Comment(ctx, n, body); err != nil {
		return 0, err
	}
	return n, nil
}

var reLinkDef = regexp.MustCompile(`^\[[^\]]+\]: `)

// changelogSection: the body of "## [X.Y.Z]" (or "## vX.Y.Z") in a Keep a
// Changelog file, without its heading and the link definitions.
func changelogSection(text, ver string) string {
	v := strings.TrimPrefix(ver, "v")
	var out []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			if in {
				break
			}
			h = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(h), "["), "v")
			in = h == v || strings.HasPrefix(h, v+"]") || strings.HasPrefix(h, v+" ")
			continue
		}
		if in {
			if reLinkDef.MatchString(l) {
				break
			}
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
