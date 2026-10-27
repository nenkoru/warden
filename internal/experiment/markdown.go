package experiment

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Title: the log's first line.
const Title = "# The 27B experiment log"

const intro = "Warden maintains this repository with a local model doing all of the work, features included. " +
	"Every task — an issue Warden works, from the first attempt through fix rounds to the merge, a give-up or a revert — " +
	"is recorded in Warden's state and rendered here once a day by its `publish-log` act. The owner and Claude read it " +
	"every two or three days to tune the prompts, tools, context and skills. How to read it: the weekly summary first " +
	"(first-push green is the share of PRs whose CI passed without a fix round; tool-call errors are calls to a tool " +
	"that does not exist or with arguments that do not parse, per tool call; wall time and tokens are the model's work on the task), then each day's tasks and its failures with their categories (tool-call, context-limit, " +
	"wrong-edit, loop, timeout, gave-up, other) and one-line reasons. Idle hours are the model's off-hours, an " +
	"unreachable upstream or a spent budget: time it could not work, never a failure. \"so far\" marks the period " +
	"still running."

// Markdown renders the report; now marks the day and week still running.
// The output depends only on the events and the day now falls on.
func (r Report) Markdown(now time.Time) string {
	loc := r.Loc
	if loc == nil {
		loc = time.UTC
	}
	today, week := dayOf(now, loc), weekOf(now, loc)
	label := func(name, current string) string {
		if name == current {
			return name + " (so far)"
		}
		return name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\nDays and weeks are in %s.\n\n## Weekly summary\n\n", Title, intro, loc)
	if len(r.Weeks) == 0 {
		b.WriteString("Nothing recorded yet.\n")
	} else {
		b.WriteString("| week | tasks | merged | first-push green | avg attempts | avg fix rounds | median wall time | tokens in / out | tool-call errors / call | reverted | top failures | idle hours |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, w := range r.Weeks {
			s := w.Summary()
			green := "—"
			if s.Pushed > 0 {
				green = fmt.Sprintf("%d%% (%d/%d)", s.Green*100/s.Pushed, s.Green, s.Pushed)
			}
			perCall := "—"
			if s.ToolCalls > 0 {
				perCall = fmt.Sprintf("%.2f (%d/%d)", float64(s.CallErrors)/float64(s.ToolCalls), s.CallErrors, s.ToolCalls)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %s | %.1f | %.1f | %s | %s / %s | %s | %d | %s | %.1f |\n", label(w.Name, week), s.Tasks, s.Merged, green,
				s.AvgAttempts, s.AvgFix, dur(s.MedianWallMS), tokens(s.TokensIn), tokens(s.TokensOut), perCall, s.Reverted, or(s.TopFailures, "—"), w.IdleHours())
		}
	}
	if len(r.Days) > 0 {
		b.WriteString("\n## Days\n")
	}
	for _, d := range r.Days {
		fmt.Fprintf(&b, "\n### %s\n\n", label(d.Name, today))
		if len(d.Tasks) == 0 {
			b.WriteString("No task worked.\n")
		} else {
			b.WriteString("| task | PR | size | brain / model | attempts | steps | tool calls | tool-call errors | wall time | tokens in / out | CI first push | fix rounds | merged | reverted | review comments | failures |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			for _, t := range d.Tasks {
				pr := "—"
				if t.PR > 0 {
					pr = fmt.Sprintf("#%d", t.PR)
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %d | %d | %s | %s / %s | %s | %d | %s | %s | %d | %s |\n", t.Name(), pr, or(t.Size, "—"),
					cell(or(strings.Trim(t.Brain+" / "+t.Model, " /"), "—")), t.Attempts, t.Steps, t.ToolCalls, t.ToolCallErrors, dur(t.WallMS), tokens(t.TokensIn), tokens(t.TokensOut),
					or(t.FirstPush, "—"), t.FixRounds, yes(t.Merged), yes(t.Reverted), t.ReviewComments, or(strings.Join(t.Categories, ", "), "—"))
			}
		}
		if len(d.Failures) > 0 {
			b.WriteString("\nFailures:\n\n")
			for _, f := range d.Failures {
				fmt.Fprintf(&b, "- %s %s %s: **%s** — %s\n", f.At.In(loc).Format("15:04"), f.Task, or(f.Phase, "?"), strings.Join(f.Categories, ", "), reason(f.Reason))
			}
		}
		if h := d.IdleHours(); h > 0 {
			var parts []string
			for _, reason := range []string{"off-hours", "unavailable", "budget"} {
				if m := d.Idle[reason]; m > 0 {
					parts = append(parts, fmt.Sprintf("%s %.1f h", reason, m/60))
				}
			}
			fmt.Fprintf(&b, "\nIdle: %.1f h (%s).\n", h, strings.Join(parts, ", "))
		}
	}
	return b.String()
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dur: "42s", "4m 10s", "1h 05m"; "—" for none.
func dur(ms int64) string {
	s := ms / 1000
	switch {
	case ms <= 0:
		return "—"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

var reURL = regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s)"']+`)

// reason: a failure's one line for a public page: no endpoint URLs (an
// error names the model server), at most 200 characters.
func reason(s string) string { return cell(oneLine(reURL.ReplaceAllString(s, "[url]"), 200)) }

// cell: text safe in a table cell and pinging nobody.
func cell(s string) string {
	return strings.NewReplacer("|", "\\|", "@", "@\u200b").Replace(s)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
