package core

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/nenkoru/warden/internal/domain"
)

// Triage answers one question: is this worth the human's attention? It is
// the part of Warden that improves itself (improve.go). It never decides
// whether to act: that is the policy engine's, with floors in code.

type Disposition string

const (
	Report Disposition = "report" // tell the human now
	Digest Disposition = "digest" // a line in the report, no ping
	Ignore Disposition = "ignore" // journal only
)

type Triage struct {
	Version int          `toml:"version" json:"version"`
	Note    string       `toml:"note" json:"note,omitempty"`
	Rules   []TriageRule `toml:"rules" json:"rules"`
}

// TriageRule: first match wins. Empty fields match anything; Subject is a
// path.Match glob.
type TriageRule struct {
	Step        string      `toml:"step,omitempty" json:"step,omitempty"`
	Kind        string      `toml:"kind,omitempty" json:"kind,omitempty"`
	Subject     string      `toml:"subject,omitempty" json:"subject,omitempty"`
	MinSeverity string      `toml:"min_severity,omitempty" json:"min_severity,omitempty"`
	MaxSeverity string      `toml:"max_severity,omitempty" json:"max_severity,omitempty"`
	Then        Disposition `toml:"then" json:"then"`
	Why         string      `toml:"why,omitempty" json:"why,omitempty"`
}

var sevRank = domain.Severities

func sevAtLeast(s, min string) bool { return sevRank[s] >= sevRank[min] }

func DefaultTriage() Triage {
	return Triage{Version: 1, Note: "built-in", Rules: []TriageRule{
		{Kind: "vuln", MinSeverity: "medium", Then: Report, Why: "a vulnerability in a component"},
		{Kind: "vuln", Then: Digest},
		{Kind: "low_disk", Then: Report, Why: "the disk is nearly full"},
		{Kind: "check", Then: Report, Why: "a health check fails"},
		{Kind: "health", MinSeverity: "medium", Then: Report},
		{Kind: "incident", MinSeverity: "medium", Then: Report, Why: "something is failing"},
		{Kind: "news", MinSeverity: "medium", Then: Report, Why: "news about something this warden guards"},
		{Kind: "release", MinSeverity: "medium", Then: Report, Why: "a release that touches this domain"},
		{Then: Digest},
	}}
}

func (r TriageRule) matches(f domain.Finding) bool {
	if r.Step != "" && r.Step != f.Step || r.Kind != "" && r.Kind != f.Kind {
		return false
	}
	if r.MinSeverity != "" && !sevAtLeast(f.Severity, r.MinSeverity) {
		return false
	}
	if r.MaxSeverity != "" && sevRank[f.Severity] > sevRank[r.MaxSeverity] {
		return false
	}
	if r.Subject != "" {
		if ok, _ := path.Match(r.Subject, f.Subject); !ok {
			return false
		}
	}
	return true
}

// Decide: the disposition and why. Below the rules is a floor in code: a
// high/critical vulnerability, low disk, a failing check and a rollback of
// Warden itself are always reported; a high incident is never ignored.
func (t Triage) Decide(f domain.Finding) (Disposition, string) {
	d, why := Digest, "default"
	for i, r := range t.Rules {
		if r.matches(f) {
			d, why = r.Then, fmt.Sprintf("triage rule %d", i+1)
			if r.Why != "" {
				why += ": " + r.Why
			}
			break
		}
	}
	switch {
	case (f.Kind == "vuln" && sevAtLeast(f.Severity, "high")) || f.Kind == "low_disk" || f.Kind == "check" || f.Kind == "rollback":
		if d != Report {
			d, why = Report, "floor: always told (high vulnerability / low disk / failing check / rollback)"
		}
	case f.Kind == "incident" && sevAtLeast(f.Severity, "high") && d == Ignore:
		d, why = Digest, "floor: a severe incident is never hidden"
	}
	return d, why
}

func (t Triage) validate() error {
	if len(t.Rules) == 0 || len(t.Rules) > 200 {
		return fmt.Errorf("triage needs 1..200 rules")
	}
	for i, r := range t.Rules {
		switch r.Then {
		case Report, Digest, Ignore:
		default:
			return fmt.Errorf("rule %d: then must be report, digest or ignore", i+1)
		}
		for _, sv := range []string{r.MinSeverity, r.MaxSeverity} {
			if _, ok := sevRank[sv]; sv != "" && !ok {
				return fmt.Errorf("rule %d: unknown severity %q", i+1, sv)
			}
		}
		if r.Subject != "" {
			if _, err := path.Match(r.Subject, ""); err != nil {
				return fmt.Errorf("rule %d: bad subject glob", i+1)
			}
		}
	}
	return nil
}

// ---- semver, just enough for watch lists (v1.2.3, 1.2.3-pre)

func semverLess(a, b string) bool { return semverCmp(a, b) < 0 }

func semverCmp(a, b string) int {
	pa, qa := splitVer(a)
	pb, qb := splitVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case qa == qb:
		return 0
	case qa == "":
		return 1
	case qb == "":
		return -1
	case qa < qb:
		return -1
	}
	return 1
}

func splitVer(v string) ([3]int, string) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, _ := strings.Cut(v, "-")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out, pre
}

func vprefix(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}
