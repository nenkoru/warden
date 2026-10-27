package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nenkoru/warden/internal/policy"
)

// The report: reports/<tick>.md when something is worth it, reports/latest.md
// always, and the Report hook only when something is new. Silence is the
// default. Every ask carries its one-line answer: `warden approve ID`.

func (in *Instance) SandboxLabel() string {
	switch in.Runner {
	case "none":
		return "NONE (owner's choice)"
	case "":
		return "unavailable — acting refused"
	}
	return in.Runner
}

func (in *Instance) report(ctx context.Context, t *Tick) error {
	var b strings.Builder
	mode := "act"
	if in.DryRun {
		mode = "dry-run (changing nothing)"
	}
	fmt.Fprintf(&b, "# Warden «%s» · %s\n\n", in.Cfg.Name, t.At.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Domain: **%s** (`%s`) · mode: **%s** · triage v%d · sandbox: %s\n\n", in.Dom.Name, in.target(), mode, in.Triage.Triage().Version, in.SandboxLabel())
	if in.paused != "" {
		fmt.Fprintf(&b, "**⏸ Paused by the owner: %s** — this tick only observes and reports; every act is refused.\n\n", in.paused)
	}
	if sp := in.spendLine(); sp != "" {
		fmt.Fprintf(&b, "%s\n\n", sp)
	}
	if in.FirstRun && in.Cfg.Mode == "" {
		b.WriteString("> First run of this domain: dry-run, because `mode` is not set. Set `mode = \"act\"` or `\"dry-run\"` in warden.toml.\n\n")
	}

	group := func(pred func(*Item) bool) []*Item {
		var out []*Item
		for _, it := range t.Items {
			if pred(it) {
				out = append(out, it)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return sevRank[out[i].Severity] > sevRank[out[j].Severity] })
		return out
	}
	done := group(func(it *Item) bool { return it.Result != nil })
	asks := group(func(it *Item) bool { return it.Pending != "" && it.Result == nil })
	important := group(func(it *Item) bool { return it.Disp == Report && it.Result == nil && it.Pending == "" })
	digest := group(func(it *Item) bool { return it.Disp == Digest && it.Result == nil && it.Pending == "" })
	ignored := group(func(it *Item) bool { return it.Disp == Ignore && it.Result == nil && it.Pending == "" })

	if n := len(done) + len(asks) + len(important); n == 0 {
		b.WriteString("**All quiet.** Nothing worth your attention.\n\n")
	} else {
		fmt.Fprintf(&b, "**Worth your attention: %d** (done: %d, waiting for you: %d, important: %d).\n\n", n, len(done), len(asks), len(important))
	}
	if len(done) > 0 {
		b.WriteString("## Done\n\n")
		for _, it := range done {
			mark := "✅"
			switch {
			case it.Result.Refused:
				mark = "⛔"
			case !it.Result.OK:
				mark = "❌"
			}
			times := ""
			if it.Count > 1 && !strings.Contains(it.Title, "×") {
				times = fmt.Sprintf(" ×%d", it.Count)
			}
			fmt.Fprintf(&b, "- %s **%s**%s — %s\n", mark, it.Title, times, it.Result.Summary)
			if it.Result.File != "" {
				fmt.Fprintf(&b, "  - file: `%s`\n", it.Result.File)
			}
			if !it.Result.OK {
				for _, l := range it.Result.Log {
					fmt.Fprintf(&b, "  - %s\n", indent(l))
				}
			}
		}
		b.WriteString("\n")
	}
	if len(asks) > 0 {
		b.WriteString("## Waiting for you\n\n")
		for _, it := range asks {
			what := it.Title
			if it.Fix != nil {
				what = it.Fix.What
			}
			fmt.Fprintf(&b, "- **%s** — %s\n", it.Title, sevWord(it.Severity))
			if it.Kind == "incident" && it.Detail != "" { // the analysis the decision rests on (the brain's RCA, then the log)
				fmt.Fprintf(&b, "  - %s\n", indent(firstLines(it.Detail, 12)))
			}
			fmt.Fprintf(&b, "  - proposed: %s\n  - why not alone: %s\n  - `warden approve %s` · `warden decline %s`\n",
				what, strings.Join(it.Reasons, "; "), it.Pending, it.Pending)
		}
		b.WriteString("\n")
	}
	section(&b, "Important", important)
	section(&b, "Digest (no ping)", digest)
	b.WriteString("## Under the hood\n\n")
	fmt.Fprintf(&b, "- %s\n", in.versionLine())
	if len(ignored) > 0 {
		fmt.Fprintf(&b, "- hidden by triage: %d (see journal.jsonl)\n", len(ignored))
	}
	fmt.Fprintf(&b, "- components in the inventory: %d\n", t.Components)
	if u := in.usageLine(); u != "" {
		fmt.Fprintf(&b, "- brains: %s\n", u)
	}
	for _, s := range in.Dom.Act {
		if g := gate(s.Floors, in.Cfg.Grants); g != "" {
			fmt.Fprintf(&b, "- act %s: %s\n", s.Name, g)
		}
	}
	for _, name := range sortedKeys(in.Refused) {
		fmt.Fprintf(&b, "- mcp server %s: refused — %s\n", name, in.Refused[name])
	}
	for _, e := range t.Errors {
		fmt.Fprintf(&b, "- error: %s\n", e)
	}
	for _, n := range t.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	fmt.Fprintf(&b, "- state: `%s` · next tick in %s\n", in.Cfg.State, in.Cfg.Wake.Every)

	t.Fresh = 0
	for _, it := range append(append(done, asks...), important...) {
		if it.New || it.Result != nil {
			t.Fresh++
		}
	}
	t.Summary = fmt.Sprintf("%d findings · done %d · asks %d · important %d · new %d", len(t.Items), len(done), len(asks), len(important), t.Fresh)
	t.Held = ""
	if t.Fresh > 0 && t.Worth() {
		if ok, why := in.mayPing(t.At, t.urgent()); !ok {
			t.Held = why
			in.St.Pings.Held++
			fmt.Fprintf(&b, "- 🔕 ping held — %s\n", why)
		}
	}
	latest := filepath.Join(in.Cfg.State, "reports", "latest.md")
	if t.Worth() {
		t.Report = filepath.Join(in.Cfg.State, "reports", t.ID+".md")
		if err := writeFile(t.Report, []byte(b.String())); err != nil {
			return err
		}
	}
	if err := writeFile(latest, []byte(b.String())); err != nil {
		return err
	}
	if t.Fresh > 0 && t.Report != "" && t.Held == "" {
		title := fmt.Sprintf("Warden «%s»: %d new", in.Cfg.Name, t.Fresh)
		if in.hasHook("Report") {
			in.fire(ctx, "Report", "report", map[string]any{"name": in.Cfg.Name, "title": title, "report": t.Report, "summary": t.Summary, "fresh": t.Fresh,
				"attention": attention(t), "spend": in.spendLine(), "paused": in.paused})
			in.pinged(t.At)
		}
	}
	return nil
}

// gate: an act's standing limit in words ("" when the policy decides freely).
func gate(fl policy.Floors, grants []string) string {
	switch {
	case fl.DryRunOnly:
		return "report-only (dry_run_only)"
	case fl.Grant != "" && !slices.Contains(grants, fl.Grant):
		return "report-only — needs the " + fl.Grant + " grant (grants in warden.toml)"
	case fl.AlwaysAsk:
		return "always asks you"
	}
	return ""
}

func (in *Instance) target() string {
	if r := in.Dom.Params["root"]; r != "" {
		return r
	}
	var kv []string
	for _, k := range sortedKeys(in.Dom.Params) {
		if v := in.Dom.Params[k]; v != "" && len(v) < 40 {
			kv = append(kv, k+"="+v)
		}
	}
	if len(kv) > 0 {
		return strings.Join(kv, " ")
	}
	h, _ := os.Hostname()
	return h
}

// section: new items in full first, then the ones already told, one line
// each — the owner has the detail from the report that told them.
func section(b *strings.Builder, title string, items []*Item) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", title)
	var told []*Item
	for _, it := range items {
		if it.New {
			item(b, it)
		} else {
			told = append(told, it)
		}
	}
	if len(told) > 0 {
		if len(told) < len(items) {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "Already told (%d):\n\n", len(told))
		for _, it := range told {
			toldLine(b, it)
		}
	}
	b.WriteString("\n")
}

func head(b *strings.Builder, it *Item) {
	fmt.Fprintf(b, "- **%s** — %s · `%s`", it.Title, sevWord(it.Severity), it.ID[:min(6, len(it.ID))])
	if it.Count > 1 && !strings.Contains(it.Title, "×") {
		fmt.Fprintf(b, " ×%d", it.Count)
	}
}

// toldLine: an item already told, in one line — no advisories or links,
// only what became of it (the branch when done).
func toldLine(b *strings.Builder, it *Item) {
	head(b, it)
	if i := strings.Index(it.Why, "; done earlier: "); i >= 0 {
		done := it.Why[i+len("; done earlier: "):]
		if f := strings.Fields(done); len(f) > 1 && f[0] == "branch" { // "branch X: what, checks pass" → "branch X"
			done = "branch " + strings.TrimRight(f[1], ":,")
		}
		fmt.Fprintf(b, " · done earlier: %s", short(done, 120))
	} else if i := strings.Index(it.Why, "; tried earlier ("); i >= 0 {
		fmt.Fprintf(b, " · tried: %s", short(it.Why[i+2:], 120))
	}
	b.WriteString("\n")
}

func item(b *strings.Builder, it *Item) {
	head(b, it)
	b.WriteString("\n")
	if it.Detail != "" {
		fmt.Fprintf(b, "  - %s\n", indent(short(it.Detail, 1500)))
	}
	if i := strings.Index(it.Why, "; done earlier: "); i >= 0 {
		fmt.Fprintf(b, "  - done earlier: %s\n", it.Why[i+len("; done earlier: "):])
	} else if i := strings.Index(it.Why, "; tried earlier ("); i >= 0 {
		fmt.Fprintf(b, "  - tried: %s\n", it.Why[i+2:])
	}
	if it.Fix != nil && it.Verdict != nil {
		v := it.Verdict
		switch {
		case v.Outcome == "report" || v.Outcome == "deny":
			fmt.Fprintf(b, "  - possible: %s — not doing it: %s\n", it.Fix.What, strings.Join(v.Reasons, "; "))
		case v.DryRun:
			fmt.Fprintf(b, "  - dry-run: %s — %s (%s)\n", it.Fix.What, wouldWord(v.Would), strings.Join(v.Reasons, "; "))
		}
	}
	for _, l := range it.Links {
		fmt.Fprintf(b, "  - %s\n", l)
	}
}

func indent(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ") }

func sevWord(s string) string {
	return map[string]string{"critical": "critical", "high": "high", "medium": "medium", "low": "low", "info": "info"}[s]
}

func wouldWord(o string) string {
	return map[string]string{"auto": "would do it alone", "ask": "would ask you", "deny": "would not do it", "report": "would only report it"}[o]
}
