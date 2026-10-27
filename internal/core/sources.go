package core

import (
	"fmt"
	"sort"
	"strings"
)

// The source flywheel: the owner's labels (`warden feedback ID noise|digest|
// important`) on what a signal source or feed brought score that source. News
// items are each new, so a triage rule learned from one label rarely matches
// the next; a source's score generalizes across them. Once a source has
// enough labels:
//
//   - mostly noise → its items go one step quieter (report → digest → ignore),
//     and the brain no longer spends a call classifying or explaining them;
//   - mostly important → its digest lines become reports.
//
// The floors stay: vulnerabilities, low disk, failing checks and anything
// critical are never made quieter, and asks and done work are not touched.
// It needs no gate: it is a function of the owner's own labels, one step at a
// time, and `warden status` shows every score.

const (
	minLabels = 3    // labels before a source's score counts
	noisyAt   = 0.25 // at or below: quieter
	usefulAt  = 0.75 // at or above: louder
)

// SourceScore: the owner's labels on one source's items.
type SourceScore struct {
	Important, Digest, Noise int
}

func (s SourceScore) labels() int { return s.Important + s.Digest + s.Noise }

// Score: how often the source was worth it, smoothed (1 important, 1 noise
// to start with: a source is not judged on one label).
func (s SourceScore) Score() float64 {
	return (float64(s.Important) + 0.5*float64(s.Digest) + 1) / float64(s.labels()+2)
}

func (s SourceScore) verdict() string {
	switch {
	case s.labels() < minLabels:
		return ""
	case s.Score() <= noisyAt:
		return "noisy"
	case s.Score() >= usefulAt:
		return "useful"
	}
	return ""
}

// SourceScores: per source, from the owner's labels (latest label per finding).
func SourceScores(cases []Case) map[string]SourceScore {
	out := map[string]SourceScore{}
	for _, c := range cases {
		src := c.Finding.Source
		if src == "" {
			continue
		}
		s := out[src]
		switch c.Want {
		case Report:
			s.Important++
		case Digest:
			s.Digest++
		case Ignore:
			s.Noise++
		}
		out[src] = s
	}
	return out
}

func (in *Instance) sourceScores() map[string]SourceScore {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.scores == nil {
		in.scores = SourceScores(in.Triage.Cases())
	}
	return in.scores
}

// noisy: the owner labeled this source's items mostly noise.
func (in *Instance) noisy(src string) bool {
	return in.sourceScores()[src].verdict() == "noisy"
}

// bySource: one step quieter or louder by the item's source score (once).
func (in *Instance) bySource(it *Item, t *Tick) {
	if it.scored || it.Source == "" || it.Pending != "" || it.Result != nil {
		return
	}
	it.scored = true
	if it.Severity == "critical" || it.Kind == "vuln" || it.Kind == "low_disk" || it.Kind == "check" {
		return // the floors
	}
	s := in.sourceScores()[it.Source]
	was := it.Disp
	switch s.verdict() {
	case "noisy":
		it.Disp = map[Disposition]Disposition{Report: Digest, Digest: Ignore, Ignore: Ignore}[it.Disp]
	case "useful":
		if it.Disp == Digest {
			it.Disp = Report
		}
	}
	if it.Disp != was {
		why := fmt.Sprintf("source %s: score %.2f (%d important, %d digest, %d noise)", it.Source, s.Score(), s.Important, s.Digest, s.Noise)
		it.Why = strings.TrimSpace(it.Why + "; " + why)
		if t != nil {
			in.noteOnce(fmt.Sprintf("source %s: score %.2f from your labels → its items go %s", it.Source, s.Score(),
				map[string]string{"noisy": "quieter", "useful": "louder"}[s.verdict()]))
		}
	}
}

// sourcesLine: every scored source, for `warden status`.
func sourcesLine(cases []Case) string {
	sc := SourceScores(cases)
	if len(sc) == 0 {
		return "sources: no labels yet (`warden feedback ID noise|digest|important` on a signal or feed item)"
	}
	names := make([]string, 0, len(sc))
	for n := range sc {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("sources:")
	for _, n := range names {
		s := sc[n]
		v := s.verdict()
		if v == "" {
			v = "neutral"
			if s.labels() < minLabels {
				v = fmt.Sprintf("needs %d labels", minLabels)
			}
		}
		fmt.Fprintf(&b, " %s %.2f (%d↑ %d· %d↓, %s);", n, s.Score(), s.Important, s.Digest, s.Noise, v)
	}
	return strings.TrimSuffix(b.String(), ";")
}

func srcName(o Order) string {
	if o.Src == nil {
		return ""
	}
	return o.Src.Name
}
