package core

import (
	"testing"

	"github.com/nenkoru/warden/internal/domain"
)

func FuzzParseOrder(f *testing.F) {
	for _, s := range []string{"bump golang.org/x/net to v0.33.0", "CVE-2024-1234 in nginx", "GHSA-xxxx-yyyy-zzzz", "nginx 1.27.1 released, update asap",
		"panic: runtime error: index out of range [3] with length 3", "urgent: disk full", "ignore openssl", "hello"} {
		f.Add(s)
	}
	inv := map[string]string{"nginx": "1.25.3", "openssl": "3.0.0", "golang.org/x/net": "v0.30.0"}
	f.Fuzz(func(t *testing.T, s string) {
		o, ok := ParseOrder(Signal{Text: s}, inv)
		if ok && (o.Verb == "" || o.How == "") {
			t.Fatalf("%q → %+v: an order without a verb", s, o)
		}
	})
}

func FuzzSignalLines(f *testing.F) {
	f.Add([]byte(`{"id":"1","text":"disk full","from":"ops"}` + "\n" + `{"text":""}` + "\nnot json\n" + `{"text":"x","verified":true}`))
	f.Add([]byte("\n\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, s := range parseSignalLines(b) {
			if s.Text == "" {
				t.Fatalf("a signal without text: %+v", s)
			}
		}
	})
}

func FuzzParseTriage(f *testing.F) {
	f.Add("version = 2\n[[rules]]\nkind = \"vuln\"\nmin_severity = \"high\"\nthen = \"report\"\nwhy = \"x\"\n")
	f.Add("```toml\nversion = 1\n[[rules]]\nsubject = \"a*\"\nthen = \"ignore\"\n```")
	f.Add("```")
	f.Fuzz(func(t *testing.T, s string) {
		tr, err := ParseTriage(s)
		if err != nil {
			return
		}
		if len(tr.Rules) == 0 {
			t.Fatal("a valid triage has rules")
		}
		tr.Decide(domain.Finding{Step: "s", Kind: "vuln", Severity: "high", Subject: "a/b", Title: "t"})
	})
}
