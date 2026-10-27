package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/nenkoru/warden/internal/domain"
)

// Self-improvement, after Harness^2 (propose → probe → compose, "keep the
// current harness" is a valid outcome) with a hard evaluator outside the
// agent's reach:
//
//   - What may change: only skills/triage.toml — when to tell the human.
//     Not the floors (code), not the autopilot (its own state, the owner's
//     rules), not warden.toml (permissions, notify recipients, mode), not
//     the evaluator (this file and the invariant cases compiled in).
//   - What it learns from: the owner's labels (`warden feedback ID noise`
//     or `important`), kept in evals/cases.jsonl with the finding itself.
//   - The gate: a candidate is replayed against every case. It must pass
//     every invariant (built-in, immutable) and must not do worse on the
//     owner's labels than the current triage; ties keep the current one.
//   - Adopted → skills/history/triage.vN.toml keeps every version, with the
//     score and why (EXPERIENCE), and `warden rollback` restores the last one.
//
// A proposal comes from the deterministic proposer (generalises the owner's
// "noise" labels into ignore/digest rules) or from the agent (Brain.Answer
// gets the current triage and the failing cases, returns TOML). Both go
// through the same gate.

// TriageSkill: the one thing Warden may change about itself, with its labels and history.
type TriageSkill struct {
	dir string
}

func OpenTriage(state string) *TriageSkill { return &TriageSkill{dir: state} }

func (s *TriageSkill) path() string { return filepath.Join(s.dir, "skills", "triage.toml") }

// Triage: the current triage (the built-in one until something was adopted).
func (s *TriageSkill) Triage() Triage {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return DefaultTriage()
	}
	var t Triage
	if toml.Unmarshal(b, &t) != nil || t.validate() != nil {
		return DefaultTriage() // a broken file never silences anything
	}
	return t
}

// Case: one labeled finding.
type Case struct {
	Finding domain.Finding `json:"finding"`
	Want    Disposition    `json:"want"`
	// AtLeast: the disposition may be this or louder (invariants say "at
	// least report"); otherwise it must be exactly Want.
	AtLeast bool   `json:"at_least,omitempty"`
	Source  string `json:"source"` // owner | invariant
	Note    string `json:"note,omitempty"`
}

var loud = map[Disposition]int{Ignore: 0, Digest: 1, Report: 2}

func (c Case) pass(d Disposition) bool {
	if c.AtLeast {
		return loud[d] >= loud[c.Want]
	}
	return d == c.Want
}

// Invariants: compiled in, the agent can't edit them.
func Invariants() []Case {
	mk := func(kind, sev, subj string, want Disposition, note string) Case {
		return Case{Finding: domain.Finding{Kind: kind, Severity: sev, Subject: subj}, Want: want, AtLeast: true, Source: "invariant", Note: note}
	}
	return []Case{
		mk("vuln", "critical", "any/module@v1.0.0", Report, "a critical vulnerability is always told"),
		mk("vuln", "high", "golang.org/x/net@v0.1.0", Report, "a high vulnerability is always told"),
		mk("vuln", "medium", "github.com/some/lib@v1.2.3", Digest, "no vulnerability is hidden"),
		mk("vuln", "low", "github.com/some/lib@v1.2.3", Digest, "no vulnerability is hidden"),
		mk("low_disk", "high", "/System/Volumes/Data", Report, "a nearly full disk is always told"),
		mk("check", "high", "tests", Report, "a failing check is always told"),
		mk("incident", "high", "deploy/api", Digest, "a severe incident is never hidden"),
	}
}

func (s *TriageSkill) casesPath() string { return filepath.Join(s.dir, "evals", "cases.jsonl") }

// Cases: the owner's labels (latest label per finding wins).
func (s *TriageSkill) Cases() []Case {
	b, _ := os.ReadFile(s.casesPath())
	by := map[string]Case{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c Case
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.Source == "owner" {
			if _, ok := by[c.Finding.ID]; !ok {
				order = append(order, c.Finding.ID)
			}
			by[c.Finding.ID] = c
		}
	}
	out := make([]Case, 0, len(order))
	for _, id := range order {
		out = append(out, by[id])
	}
	return out
}

// Label: the owner's feedback on a finding: "noise" (don't tell me about
// this), "digest" (a line in the report is enough), "important" (tell me).
func (s *TriageSkill) Label(f domain.Finding, label string) (Case, error) {
	want := map[string]Disposition{"noise": Ignore, "ignore": Ignore, "digest": Digest, "important": Report, "report": Report}[label]
	if want == "" {
		return Case{}, errors.New("label: noise | digest | important")
	}
	c := Case{Finding: f, Want: want, Source: "owner", Note: label}
	b, _ := json.Marshal(c)
	if err := os.MkdirAll(filepath.Dir(s.casesPath()), 0o700); err != nil {
		return c, err
	}
	fh, err := os.OpenFile(s.casesPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return c, err
	}
	defer fh.Close()
	_, err = fh.Write(append(b, '\n'))
	return c, err
}

// Score of a triage on the cases.
type Score struct {
	Invariants, InvariantsOK int
	Owner, OwnerOK           int
	Failing                  []string
}

func (s Score) String() string {
	return fmt.Sprintf("invariants %d/%d, owner labels %d/%d", s.InvariantsOK, s.Invariants, s.OwnerOK, s.Owner)
}

func Evaluate(t Triage, owner []Case) Score {
	var sc Score
	for _, c := range append(Invariants(), owner...) {
		d, _ := t.Decide(c.Finding)
		ok := c.pass(d)
		if c.Source == "invariant" {
			sc.Invariants++
			if ok {
				sc.InvariantsOK++
			}
		} else {
			sc.Owner++
			if ok {
				sc.OwnerOK++
			}
		}
		if !ok {
			sc.Failing = append(sc.Failing, fmt.Sprintf("%s %s %s: got %s, want %s (%s)", c.Finding.Kind, c.Finding.Severity, c.Finding.Subject, d, c.Want, c.Note))
		}
	}
	return sc
}

// Outcome of one improvement attempt.
type Outcome struct {
	Adopted   bool
	Version   int
	Before    Score
	After     Score
	Why       string
	Candidate Triage
}

// Gate: the only way a triage changes.
func (s *TriageSkill) Gate(cand Triage, source string) (Outcome, error) {
	cur := s.Triage()
	owner := s.Cases()
	o := Outcome{Candidate: cand, Before: Evaluate(cur, owner)}
	if err := cand.validate(); err != nil {
		o.Why = "rejected: " + err.Error()
		return o, nil
	}
	o.After = Evaluate(cand, owner)
	switch {
	case o.After.InvariantsOK < o.After.Invariants:
		o.Why = "rejected: breaks an invariant — " + strings.Join(o.After.Failing, "; ")
	case o.After.OwnerOK < o.Before.OwnerOK:
		o.Why = fmt.Sprintf("rejected: worse on your labels (%d → %d)", o.Before.OwnerOK, o.After.OwnerOK)
	case o.After.OwnerOK == o.Before.OwnerOK:
		o.Why = "kept the current triage: the candidate is no better on your labels"
	default:
		o.Adopted = true
		o.Version = cur.Version + 1
		cand.Version = o.Version
		cand.Note = fmt.Sprintf("v%d from %s at %s: %s → %s", o.Version, source, time.Now().UTC().Format(time.RFC3339), o.Before, o.After)
		o.Candidate = cand
		o.Why = "adopted: " + o.After.String() + " (was " + o.Before.String() + ")"
		if err := s.adopt(cur, cand, o); err != nil {
			return o, err
		}
	}
	s.experience(o, source)
	return o, nil
}

func (s *TriageSkill) adopt(cur, cand Triage, o Outcome) error {
	hist := filepath.Join(s.dir, "skills", "history")
	prev, _ := toml.Marshal(cur)
	if err := writeFile(filepath.Join(hist, fmt.Sprintf("triage.v%d.toml", cur.Version)), prev); err != nil {
		return err
	}
	next, err := toml.Marshal(cand)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(hist, fmt.Sprintf("triage.v%d.toml", cand.Version)), next); err != nil {
		return err
	}
	return writeFile(s.path(), next)
}

// experience: EXPERIENCE.md, one entry per attempt (adopted or not).
func (s *TriageSkill) experience(o Outcome, source string) {
	f, err := os.OpenFile(filepath.Join(s.dir, "skills", "EXPERIENCE.md"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## %s · %s\n\n%s\n\n- before: %s\n- after: %s\n", time.Now().Format("2006-01-02 15:04"), source, o.Why, o.Before, o.After)
	for _, r := range o.Candidate.Rules {
		if r.Why != "" && strings.HasPrefix(r.Why, "learned:") {
			fmt.Fprintf(f, "- rule: %s\n", r.Why)
		}
	}
	_, _ = f.WriteString("\n")
}

// Rollback: back to the previous adopted version (or the built-in one).
func (s *TriageSkill) Rollback() (int, error) {
	cur := s.Triage()
	if cur.Version <= 1 {
		return 0, errors.New("nothing to roll back: this is the built-in triage")
	}
	prev := filepath.Join(s.dir, "skills", "history", fmt.Sprintf("triage.v%d.toml", cur.Version-1))
	b, err := os.ReadFile(prev)
	if err != nil {
		return 0, err
	}
	if cur.Version-1 == 1 {
		os.Remove(s.path())
	} else if err := writeFile(s.path(), b); err != nil {
		return 0, err
	}
	s.experience(Outcome{Why: fmt.Sprintf("rolled back v%d → v%d by the owner", cur.Version, cur.Version-1)}, "owner")
	return cur.Version - 1, nil
}

// ProposeFromLabels: the deterministic proposer. Every owner label the
// current triage gets wrong becomes a narrow rule in front: the finding's
// sensor, kind and subject (versions become a glob) and, when it makes
// things quieter, only up to the labeled severity (a worse state of the
// same thing is still told). A rule that would break an invariant on its
// own is left out; then the gate decides on the whole.
func ProposeFromLabels(cur Triage, owner []Case) Triage {
	cand := Triage{Version: cur.Version}
	seen := map[string]bool{}
	for _, c := range owner {
		d, _ := cur.Decide(c.Finding)
		if c.pass(d) {
			continue
		}
		subj := c.Finding.Subject
		if i := strings.LastIndex(subj, "@"); i > 0 {
			subj = subj[:i] + "@*"
		}
		r := TriageRule{Step: c.Finding.Step, Kind: c.Finding.Kind, Subject: subj, Then: c.Want, Why: "learned: owner said " + c.Note + " about " + subj}
		if loud[c.Want] < loud[d] {
			r.MaxSeverity = c.Finding.Severity
		}
		k := r.Step + "|" + r.Kind + "|" + r.Subject + "|" + r.MaxSeverity
		if seen[k] {
			continue
		}
		seen[k] = true
		trial := Triage{Rules: append(append([]TriageRule{r}, cand.Rules...), cur.Rules...)}
		if sc := Evaluate(trial, nil); sc.InvariantsOK < sc.Invariants {
			continue // it would break an invariant
		}
		if d2, _ := trial.Decide(c.Finding); !c.pass(d2) {
			continue // the floor in code overrides it anyway ("low disk is noise")
		}
		cand.Rules = append(cand.Rules, r)
	}
	sort.SliceStable(cand.Rules, func(i, j int) bool { return cand.Rules[i].Subject > cand.Rules[j].Subject })
	cand.Rules = append(cand.Rules, cur.Rules...)
	return cand
}

// ParseTriage: a candidate from text (the agent's answer, or a file).
func ParseTriage(text string) (Triage, error) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 { // the agent may fence it
		text = text[i+3:]
		text = strings.TrimPrefix(text, "toml")
		if j := strings.Index(text, "```"); j >= 0 {
			text = text[:j]
		}
	}
	var t Triage
	dec := toml.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, err
	}
	return t, t.validate()
}

// AgentPrompt: what the agent gets when asked for a better triage.
func AgentPrompt(cur Triage, sc Score) string {
	b, _ := toml.Marshal(cur)
	return "You maintain the triage of an always-on warden agent: it decides which findings its human hears about now (report), in a digest, or not at all (ignore).\n" +
		"Current triage (TOML; first matching rule wins; fields step, kind, subject (path.Match glob), min_severity, then, why):\n\n" + string(b) +
		"\nCases it gets wrong (the human's labels):\n- " + strings.Join(sc.Failing, "\n- ") +
		"\n\nReply with the complete improved triage as TOML only. Keep it small. Some invariants can't be changed (high/critical vulnerabilities, low disk and failing checks are always reported; severe incidents are never ignored) — don't try."
}
