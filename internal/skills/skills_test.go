package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRootAndSingle(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "pdf"), "---\nname: pdf\ndescription: \"Read and fill PDF forms\"\nlicense: 'MIT'\n---\n\n# PDF\nUse pdftotext.\n")
	write(t, filepath.Join(root, "triage"), "---\r\nname: triage\r\ndescription: >\r\n  When to tell\r\n  the human.\r\nversion: 2\r\n---\r\nBody\r\n")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	single := filepath.Join(t.TempDir(), "k8s")
	write(t, single, "---\nname: k8s\ndescription: Kubernetes know-how: rollouts\n---\nkubectl\n")

	sk, err := Load(root, single)
	if err != nil {
		t.Fatal(err)
	}
	if len(sk) != 3 {
		t.Fatalf("got %d skills: %+v", len(sk), sk)
	}
	pdf := sk[0]
	if pdf.Name != "pdf" || pdf.Description != "Read and fill PDF forms" || pdf.Meta["license"] != "MIT" || pdf.Body != "# PDF\nUse pdftotext.\n" || !filepath.IsAbs(pdf.Dir) {
		t.Fatalf("pdf = %+v", pdf)
	}
	if tr := sk[1]; tr.Description != "When to tell the human." || tr.Meta["version"] != "2" || tr.Body != "Body\n" {
		t.Fatalf("triage = %+v", tr)
	}
	if sk[2].Description != "Kubernetes know-how: rollouts" {
		t.Fatalf("k8s = %+v", sk[2])
	}

	want := "- k8s: Kubernetes know-how: rollouts\n- pdf: Read and fill PDF forms\n- triage: When to tell the human.\n"
	if got := Index(sk); got != want {
		t.Fatalf("Index =\n%s\nwant\n%s", got, want)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, text := range map[string]string{
		"nofront": "# just markdown\n",
		"noclose": "---\nname: x\ndescription: y\n",
		"noname":  "---\ndescription: y\n---\n",
		"nodesc":  "---\nname: x\n---\n",
	} {
		dir := filepath.Join(t.TempDir(), name)
		write(t, dir, text)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "SKILL.md") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing dir: no error")
	}
}

func TestLaterOverrides(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "s"), filepath.Join(t.TempDir(), "s")
	write(t, a, "---\nname: s\ndescription: first\n---\n")
	write(t, b, "---\nname: s\ndescription: second\n---\n")
	sk, err := Load(a, b)
	if err != nil || len(sk) != 1 || sk[0].Description != "second" {
		t.Fatalf("%+v %v", sk, err)
	}
}
