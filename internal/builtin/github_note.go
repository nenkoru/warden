package builtin

import (
	"context"
	"fmt"
	"strings"
)

// GitHubNote is the github-repo pack's notifier, run as a hook command
// (`warden github-note --config … --event Report`, the event JSON on
// stdin). It needs no new credential: it comments on a pinned "Warden log"
// issue of the repository with the same token.
//
//	Report  a comment mentioning the owner, only when the report has
//	        something for them (payload "attention": security findings, red
//	        main, a failed release, budget alerts, anything waiting); else nothing
//	Ask     a comment mentioning the owner: what waits, and the answer
//	Digest  the day's summary, no mention
//
// Text from the payload can come from outsiders (issue titles): every @ in
// it is defused, so the only mention is the owner's.
func GitHubNote(ctx context.Context, params map[string]string, event string, payload map[string]any, web *Web) (string, error) {
	env := &Env{Params: params, Web: web}
	c, err := ghClient(env)
	if err != nil {
		return "", err
	}
	str := func(k string) string { s, _ := payload[k].(string); return noMention(s) }
	list := func(k string) []string {
		var out []string
		if xs, ok := payload[k].([]any); ok {
			for _, x := range xs {
				if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, noMention(s))
				}
			}
		}
		return out
	}
	mention := ownerMention(env)
	var b strings.Builder
	switch event {
	case "Report":
		att := list("attention")
		if len(att) == 0 {
			return "", nil // nothing for the owner now: it goes into the daily digest
		}
		fmt.Fprintf(&b, "%s — needs your attention:\n\n", mention)
		for _, a := range att {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		if s := str("summary"); s != "" {
			fmt.Fprintf(&b, "\n<sub>%s · %s</sub>\n", s, str("spend"))
		}
	case "Ask":
		fmt.Fprintf(&b, "%s — waiting for you: **%s**\n\n- proposed: %s\n- why not alone: %s\n- `%s` · `%s`\n", mention, str("title"), str("what"), str("why"), str("approve"), str("decline"))
	case "Digest":
		fmt.Fprintf(&b, "**Daily digest — %s**\n\n", str("day"))
		if s := str("spend"); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		lines := list("lines")
		if kl, err := KernelPRs(ctx, c); err == nil { // the kernel PRs waiting for the owner come first
			lines = append(kl, lines...)
		}
		if len(lines) == 0 {
			b.WriteString("A quiet day.\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	default:
		fmt.Fprintf(&b, "**%s** %s\n\n%s\n", event, str("title"), str("summary"))
	}
	n, err := logIssue(ctx, env)
	if err != nil {
		return "", err
	}
	if err := c.Comment(ctx, n, b.String()); err != nil {
		return "", err
	}
	return fmt.Sprintf("noted on issue #%d", n), nil
}

// logIssue: the open "Warden log" issue (label warden-log), made and
// pinned on first use.
func logIssue(ctx context.Context, env *Env) (int, error) {
	c, err := ghClient(env)
	if err != nil {
		return 0, err
	}
	login, err := c.Login(ctx)
	if err != nil {
		return 0, err
	}
	is, err := c.IssuesLabeled(ctx, "warden-log")
	if err != nil {
		return 0, err
	}
	for _, i := range is {
		if i.User.Login == login {
			return i.Number, nil
		}
	}
	i, err := c.CreateIssue(ctx, "Warden log", "Warden's notes to the owner: a daily digest, and pings (with a mention) only for what needs you. "+
		"To pause Warden, label any issue `warden-pause` or add a `PAUSE` file at the root of the default branch.", "warden-log")
	if err != nil {
		return 0, err
	}
	_ = c.Pin(ctx, i.NodeID) // best effort: pinning is GraphQL-only and cosmetic
	return i.Number, nil
}
