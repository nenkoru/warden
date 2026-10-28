package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

// The recorded fixture (testdata/README.md): sigstore-js 2.0.0's npm
// provenance — an SLSA provenance v1 statement signed in GitHub Actions on
// the public-good Sigstore instance — and the public-good trusted root.
const (
	fixtureSAN    = "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main"
	fixtureSHA512 = "46d4e2f74c4877316640000a6fdf8a8b59f1e0847667973e9859f774dd31b8f1e0937813b777fb66a2ac67d50540fe34640966eee9fc2ccca387082b4c85cd3c"
)

func fixture(t *testing.T) (root.TrustedMaterial, []byte) {
	t.Helper()
	tr, err := root.NewTrustedRootFromPath("testdata/trusted-root-public-good.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("testdata/sigstore-js-2.0.0-provenance.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	return tr, b
}

func TestVerifyBundleRecorded(t *testing.T) {
	tm, raw := fixture(t)
	digest, _ := hex.DecodeString(fixtureSHA512)
	san := SAN("sigstore/sigstore-js", ".github/workflows/release.yml", "refs/heads/main", "")
	if err := VerifyBundle(tm, raw, "sha512", digest, san); err != nil {
		t.Fatalf("the recorded bundle must verify: %v", err)
	}
	if !strings.Contains(fixtureSAN, "sigstore-js") {
		t.Fatal("fixture")
	}
	other := bytes.Clone(digest)
	other[0] ^= 1
	for name, err := range map[string]error{
		"another digest":   VerifyBundle(tm, raw, "sha512", other, san),
		"another repo":     VerifyBundle(tm, raw, "sha512", digest, SAN("nenkoru/warden", ".github/workflows/release.yml", "refs/heads/main", "")),
		"another workflow": VerifyBundle(tm, raw, "sha512", digest, SAN("sigstore/sigstore-js", ".github/workflows/ci.yml", "refs/heads/main", "")),
		"a tag ref":        VerifyBundle(tm, raw, "sha512", digest, SAN("sigstore/sigstore-js", ".github/workflows/release.yml", "refs/tags/{tag}", "v2.0.0")),
		"a forged payload": VerifyBundle(tm, forge(t, raw), "sha512", digest, san),
		"not a bundle":     VerifyBundle(tm, []byte(`{}`), "sha512", digest, san),
	} {
		if err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	empty, _ := root.NewTrustedRootFromJSON([]byte(`{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}`))
	if empty != nil && VerifyBundle(empty, raw, "sha512", digest, san) == nil {
		t.Error("verified without a trusted root")
	}
}

// forge: the bundle with its statement's payload changed (the signature no longer covers it).
func forge(t *testing.T, raw []byte) []byte {
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	env := b["dsseEnvelope"].(map[string]any)
	p, _ := base64.StdEncoding.DecodeString(env["payload"].(string))
	env["payload"] = base64.StdEncoding.EncodeToString(bytes.Replace(p, []byte("sigstore-js"), []byte("sigstore-jz"), 1))
	out, _ := json.Marshal(b)
	return out
}

func TestSAN(t *testing.T) {
	san := SAN("nenkoru/warden", ".github/workflows/release.yml", "refs/tags/{tag}", "v0.3.0")
	for s, want := range map[string]bool{
		"https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/tags/v0.3.0":   true,
		"https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/tags/v0.3.01":  false,
		"https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/tags/v0x3x0":   false,
		"https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/heads/main":    false,
		"https://github.com/nenkoru/wardenx/.github/workflows/release.yml@refs/tags/v0.3.0":  false,
		"https://github.com/evil/warden/.github/workflows/release.yml@refs/tags/v0.3.0":      false,
		"https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/tags/v0.3.0\n": false,
	} {
		if got := mustMatch(san, s); got != want {
			t.Errorf("%s ~ %s = %v", san, s, got)
		}
	}
	loose := SAN("nenkoru/warden", ".github/workflows/release.yml", `refs/tags/v\d+\.\d+\.\d+`, "v0.3.0")
	if !mustMatch(loose, "https://github.com/nenkoru/warden/.github/workflows/release.yml@refs/tags/v1.2.3") {
		t.Error("signer_ref regexp")
	}
}

// TestSigstoreFailsClosed: no attestation, a junk one, or one for another
// identity — never a pass.
func TestSigstoreFailsClosed(t *testing.T) {
	tm, raw := fixture(t)
	f := githubtest.New(t, "tok", "bot", "acme/warden")
	f.Anonymous = true
	gh, _ := github.Anonymous(f.URL(), "acme/warden")
	v := &Sigstore{GH: gh, Workflow: ".github/workflows/release.yml", SignerRef: "refs/tags/{tag}", Material: tm}
	d := strings.Repeat("ab", 32)
	ctx := context.Background()
	s := Subject{Repo: "acme/warden", Tag: "v0.3.0", Name: "warden_0.3.0_linux_amd64.tar.gz", Digest: d}
	if err := v.Verify(ctx, s); err == nil || !strings.Contains(err.Error(), "no attestation") {
		t.Fatalf("no attestation: %v", err)
	}
	f.With(func(f *githubtest.Fake) {
		f.Attests["sha256:"+d] = []github.Attestation{{Bundle: []byte(`{"mediaType":"junk"}`)}, {Bundle: raw}, {BundleURL: ""}}
	})
	if err := v.Verify(ctx, s); err == nil || !strings.Contains(err.Error(), "verifies") {
		t.Fatalf("junk and foreign attestations: %v", err)
	}
	if err := v.Verify(ctx, Subject{Digest: "xyz"}); err == nil {
		t.Fatal("a bad digest")
	}
}

func TestBundleURLSnappy(t *testing.T) {
	js := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","x":"` + strings.Repeat("a", 300) + `"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plain" {
			_, _ = w.Write(js)
			return
		}
		_, _ = w.Write(snappyLiterals(js))
	}))
	defer srv.Close()
	v := &Sigstore{HTTP: srv.Client()}
	for _, p := range []string{"/plain", "/snappy"} {
		got, err := v.bundleBytes(context.Background(), github.Attestation{Bundle: []byte("null"), BundleURL: srv.URL + p})
		if err != nil || !bytes.Equal(got, js) {
			t.Errorf("%s: %v %q", p, err, got)
		}
	}
	if _, err := (&Sigstore{}).bundleBytes(context.Background(), github.Attestation{BundleURL: "http://example.com/x"}); err == nil {
		t.Error("a plain-http bundle_url was fetched")
	}
}

func TestUnsnappy(t *testing.T) {
	// "abcd" literal, then a 1-byte-offset copy (8 from 4 back) and a 2-byte-offset copy (2 from 12 back).
	src := []byte{14, 0x0c, 'a', 'b', 'c', 'd', 0x11, 0x04, 0x06, 0x0c, 0x00}
	got, err := unsnappy(src, 100)
	if err != nil || string(got) != "abcdabcdabcdab" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range [][]byte{{}, {5, 0x0c, 'a'}, {4, 0x11, 0x09}, {200, 1}, {3, 0x08, 'a', 'b', 'c', 0x01}} {
		if _, err := unsnappy(bad, 100); err == nil {
			t.Errorf("%v decoded", bad)
		}
	}
	if _, err := unsnappy([]byte{0xff, 0xff, 0x03}, 100); err == nil {
		t.Error("over the cap")
	}
}

// snappyLiterals: a snappy block of literals only (enough for a test).
func snappyLiterals(b []byte) []byte {
	var out []byte
	for n := len(b); ; n >>= 7 {
		if n < 0x80 {
			out = append(out, byte(n))
			break
		}
		out = append(out, byte(n)|0x80)
	}
	for len(b) > 0 {
		n := min(len(b), 250)
		if n <= 60 {
			out = append(out, byte(n-1)<<2)
		} else {
			out = append(out, 60<<2, byte(n-1))
		}
		out = append(out, b[:n]...)
		b = b[n:]
	}
	return out
}

func TestChecksum(t *testing.T) {
	d := strings.Repeat("0a", 32)
	sums := []byte(d + "  warden_0.3.0_linux_amd64.tar.gz\n" + strings.Repeat("1b", 32) + " *other.tar.gz\nnot a line\n")
	if got, err := checksum(sums, "warden_0.3.0_linux_amd64.tar.gz"); err != nil || got != d {
		t.Fatal(got, err)
	}
	if got, err := checksum(sums, "other.tar.gz"); err != nil || got != strings.Repeat("1b", 32) {
		t.Fatal(got, err)
	}
	if _, err := checksum(sums, "missing"); err == nil {
		t.Fatal("missing")
	}
	if _, err := checksum([]byte("zz  x\n"), "x"); err == nil {
		t.Fatal("bad hex")
	}
}

func mustMatch(re, s string) bool { return regexp.MustCompile(re).MatchString(s) }
