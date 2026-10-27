package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
)

// checkPause runs the domain's [[pause]] steps first thing in a tick. Any
// finding pauses the tick: it still observes and reports, but every act is
// refused ("paused by the owner: …"). A pause step that fails also pauses:
// a kill switch that can't be read is treated as pulled.
func (in *Instance) checkPause(ctx context.Context, t *Tick) []domain.Finding {
	in.paused = ""
	if len(in.Dom.Pause) == 0 {
		return nil
	}
	errs := len(t.Errors)
	fs := in.observeSteps(ctx, t, in.Dom.Pause, "pause")
	var why []string
	for i := range fs {
		fs[i].Fix = nil
		if fs[i].Kind == "" || fs[i].Kind == fs[i].Step {
			fs[i].Kind = "pause"
		}
		why = append(why, fs[i].Title)
	}
	if len(t.Errors) > errs {
		why = append(why, "the kill switch could not be checked ("+strings.Join(t.Errors[errs:], "; ")+")")
	}
	if len(why) > 0 {
		in.paused = strings.Join(why, "; ")
		in.St.Journal("paused", map[string]any{"why": in.paused})
	}
	return fs
}

// budgetAlerts: the meter's alerts (a threshold crossed, a burn rate, the
// cap) become findings that ping at once.
func (in *Instance) budgetAlerts(t *Tick) {
	if in.Ledger == nil {
		return
	}
	day := in.Now().Format(time.DateOnly)
	for _, a := range in.Ledger.Alerts() {
		sev := "high"
		if a.Kind == "cap" {
			sev = "critical"
		}
		key := fmt.Sprintf("budget|%s|%s|%s", day, a.Kind, a.Text)
		f := domain.Finding{ID: domain.ID("budget", key), Key: key, Step: "budget", Kind: "budget", Severity: sev, Subject: "model budget", Title: a.Text}
		it := &Item{Finding: f, Disp: Report, Why: "a budget alert", New: true}
		if s := in.St.Seen[f.ID]; s != nil && s.Reported > 0 {
			it.New = false
		} else {
			in.St.Seen[f.ID] = &Seen{First: in.Now().Unix(), Last: in.Now().Unix(), Title: f.Title}
		}
		t.Items = append(t.Items, it)
	}
}

// attention: what this tick has for the owner now — the lines a Report
// hook pings with (the rest waits for the daily digest): critical and high
// news (security, a red main, a failed release), budget alerts, asks, and
// work that failed or was left to the owner.
func attention(t *Tick) []string {
	var out []string
	for _, it := range t.Items {
		switch {
		case it.Result != nil && !it.Result.OK:
			out = append(out, "⛔ "+it.Title+" — "+it.Result.Summary)
		case it.Pending != "" && it.New:
			out = append(out, "❓ "+it.Title+" — `warden approve "+it.Pending+"`")
		case it.New && it.Disp != Ignore && (sevAtLeast(it.Severity, "high") || it.Kind == "budget" || it.Data["waiting"] == "owner"):
			out = append(out, "⚠ "+it.Title)
		}
	}
	return out
}

// digest: with digest = "daily", the day's lines are kept, and the first
// tick of a new day fires one Digest hook with the day before.
func (in *Instance) digest(ctx context.Context, t *Tick) {
	if in.Cfg.Digest != "daily" {
		return
	}
	d := &in.St.Digest
	today := t.At.Format(time.DateOnly)
	if d.Day != "" && d.Day != today {
		in.fire(ctx, "Digest", "digest", map[string]any{"name": in.Cfg.Name, "day": d.Day, "lines": d.Lines, "spend": d.Spend,
			"title": fmt.Sprintf("Warden «%s» — %s", in.Cfg.Name, d.Day), "summary": fmt.Sprintf("%d things happened", len(d.Lines))})
		in.St.Journal("digest", map[string]any{"day": d.Day, "lines": len(d.Lines)})
		*d = DigestState{}
	}
	d.Day = today
	if s := in.spendLine(); s != "" {
		d.Spend = s
	}
	add := func(s string) {
		for _, l := range d.Lines {
			if l == s {
				return
			}
		}
		if len(d.Lines) < 300 {
			d.Lines = append(d.Lines, s)
		}
	}
	for _, it := range t.Items {
		switch {
		case it.Result != nil:
			mark := "✅"
			if !it.Result.OK {
				mark = "⛔"
			}
			add(fmt.Sprintf("%s %s — %s", mark, short(it.Title, 140), short(it.Result.Summary, 200)))
		case it.Pending != "" && it.New:
			add("❓ waiting for you: " + short(it.Title, 160))
		case it.New && it.Disp == Report:
			add("• " + short(it.Title, 180))
		}
	}
	if in.paused != "" {
		add("⏸ paused by the owner: " + short(in.paused, 160))
	}
}
