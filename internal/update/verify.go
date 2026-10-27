package update

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/version"
)

// The verification chain, failing closed at every link:
//
//	archive sha256 == its line in checksums.txt
//	→ a GitHub build-provenance attestation for that digest whose Sigstore
//	  bundle verifies against the public-good trusted root (TUF), signed by
//	  https://github.com/<repo>/<workflow>@refs/tags/<tag> through GitHub
//	  Actions' OIDC issuer, an SLSA provenance v1 statement naming the digest
//	→ cosign, additionally, when it is on PATH and the release has a
//	  .bundle (or .sig + .pem) for the archive or checksums.txt.

const (
	// Issuer: GitHub Actions' OIDC issuer — the only signer accepted.
	Issuer = "https://token.actions.githubusercontent.com"
	// SLSAProvenanceV1: the predicate type the statement must carry.
	SLSAProvenanceV1 = "https://slsa.dev/provenance/v1"
)

// Subject: one release file to verify.
type Subject struct {
	Repo, Tag, Name string
	Digest          string // sha256, hex
}

// Verifier proves a release file was built by the release workflow.
type Verifier interface {
	Verify(ctx context.Context, s Subject) error
}

// Sigstore: the build-provenance verifier (GitHub attestations + sigstore-go).
type Sigstore struct {
	GH        *github.Client
	Workflow  string // .github/workflows/release.yml
	SignerRef string // a regexp; {tag} = the tag, quoted
	TUFCache  string // <state>/update/tuf
	// Material: the trusted root (tests: a recorded one); nil = TUF.
	Material root.TrustedMaterial
	HTTP     *http.Client // bundle_url downloads
	mu       sync.Mutex
}

// SAN: the certificate identity a release's provenance must carry, as an anchored regexp.
func SAN(repo, workflow, signerRef, tag string) string {
	ref := strings.ReplaceAll(signerRef, "{tag}", regexp.QuoteMeta(tag))
	return "^" + regexp.QuoteMeta("https://github.com/"+repo+"/"+strings.TrimPrefix(workflow, "/")+"@") + "(?:" + ref + ")$"
}

func (v *Sigstore) Verify(ctx context.Context, s Subject) error {
	digest, err := hex.DecodeString(s.Digest)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("provenance: %q is not a sha256 digest", s.Digest)
	}
	tm, err := v.material(ctx)
	if err != nil {
		return fmt.Errorf("provenance: the Sigstore trusted root: %w", err)
	}
	atts, err := v.GH.Attestations(ctx, "sha256:"+s.Digest)
	if errors.Is(err, github.ErrNotFound) || err == nil && len(atts) == 0 {
		return fmt.Errorf("provenance: no attestation for %s (sha256:%s)", s.Name, s.Digest[:12])
	}
	if err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	san := SAN(s.Repo, v.Workflow, v.SignerRef, s.Tag)
	var errs []string
	for i, a := range atts {
		if i >= 10 {
			break
		}
		raw, err := v.bundleBytes(ctx, a)
		if err == nil {
			err = VerifyBundle(tm, raw, "sha256", digest, san)
		}
		if err == nil {
			return nil
		}
		errs = append(errs, err.Error())
	}
	return fmt.Errorf("provenance: no attestation for %s verifies: %s", s.Name, strings.Join(errs, "; "))
}

// material: the trusted root through TUF (cached under TUFCache, refreshed
// daily). Offline, the cached TUF metadata serves while it is signed and
// unexpired (ForceCache: go-tuf still checks both); never an unverified root.
func (v *Sigstore) material(ctx context.Context) (root.TrustedMaterial, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Material != nil {
		return v.Material, nil
	}
	opts := tuf.DefaultOptions().WithCachePath(v.TUFCache).WithCacheValidity(1).WithContext(ctx)
	tr, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		cached, cerr := root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithCachePath(v.TUFCache).WithForceCache().WithContext(ctx))
		if cerr != nil {
			return nil, err
		}
		tr = cached
	}
	v.Material = tr
	return tr, nil
}

// bundleBytes: the bundle inline, else from bundle_url (snappy-compressed, https only).
func (v *Sigstore) bundleBytes(ctx context.Context, a github.Attestation) ([]byte, error) {
	if len(a.Bundle) > 0 && !bytes.Equal(a.Bundle, []byte("null")) {
		return a.Bundle, nil
	}
	if a.BundleURL == "" {
		return nil, errors.New("an attestation without a bundle")
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
		if !strings.HasPrefix(a.BundleURL, "https://") {
			return nil, errors.New("bundle_url is not https")
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", a.BundleURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bundle_url: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 || len(b) > 4<<20 {
		return nil, fmt.Errorf("bundle_url: status %d, %d bytes", resp.StatusCode, len(b))
	}
	if json.Valid(b) {
		return b, nil
	}
	return unsnappy(b, 16<<20)
}

// VerifyBundle verifies one Sigstore bundle: the signature, the
// certificate chain to the trusted root at the signing time, a transparency
// log entry and an SCT; the signer's identity (san, an anchored regexp, via
// GitHub Actions' issuer); an SLSA provenance v1 statement whose subjects
// include the digest.
func VerifyBundle(tm root.TrustedMaterial, raw []byte, alg string, digest []byte, san string) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	v, err := verify.NewVerifier(tm, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	id, err := verify.NewShortCertificateIdentity(Issuer, "", "", san)
	if err != nil {
		return err
	}
	res, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest(alg, digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	st := res.Statement
	if st == nil || st.GetPredicateType() != SLSAProvenanceV1 {
		return fmt.Errorf("bundle: not an SLSA provenance v1 statement")
	}
	want := hex.EncodeToString(digest)
	for _, sub := range st.GetSubject() {
		if strings.EqualFold(sub.GetDigest()[alg], want) {
			return nil
		}
	}
	return fmt.Errorf("bundle: the statement does not name %s:%s", alg, want[:12])
}

// checksum: the sha256 that checksums.txt lists for name.
func checksum(sums []byte, name string) (string, error) {
	for _, l := range strings.Split(string(sums), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if b, err := hex.DecodeString(f[0]); err != nil || len(b) != 32 {
				return "", fmt.Errorf("checksums.txt: a bad sha256 for %s", name)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt does not list %s", name)
}

// cosign: the extra check when cosign is installed and the release carries
// a cosign bundle (or a signature and certificate) for one of the files.
// Nothing to check is fine; a check that runs and fails is not.
func (u *Updater) cosign(ctx context.Context, rel github.Release, files map[string][]byte) (string, error) {
	bin, err := u.LookPath("cosign")
	if err != nil {
		return "", nil
	}
	assets := map[string]github.Asset{}
	for _, a := range rel.Assets {
		assets[a.Name] = a
	}
	dir, err := os.MkdirTemp("", "warden-cosign-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	san := SAN(u.Cfg.Repo, u.Cfg.Workflow, u.Cfg.SignerRef, rel.TagName)
	var done []string
	for name, content := range files {
		var args []string
		get := func(suffix string) (string, bool) {
			a, ok := assets[name+suffix]
			if !ok {
				return "", false
			}
			b, err := u.GH.AssetBytes(ctx, a.ID, 1<<20)
			if err != nil {
				return "", false
			}
			p := filepath.Join(dir, filepath.Base(name+suffix))
			return p, os.WriteFile(p, b, 0o600) == nil
		}
		if p, ok := get(".bundle"); ok {
			args = []string{"--bundle", p}
		} else if sig, ok := get(".sig"); ok {
			if pem, ok := get(".pem"); ok {
				args = []string{"--signature", sig, "--certificate", pem}
			}
		}
		if args == nil {
			continue
		}
		file := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(file, content, 0o600); err != nil {
			return "", err
		}
		args = append(append([]string{"verify-blob"}, args...), "--certificate-identity-regexp", san, "--certificate-oidc-issuer", Issuer, file)
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		out, err := u.Exec(cctx, bin, args...)
		cancel()
		if err != nil {
			return "", fmt.Errorf("cosign verify-blob %s: %v %s", name, err, short(string(out), 300))
		}
		done = append(done, name)
	}
	if len(done) == 0 {
		return "", nil
	}
	return "cosign: " + strings.Join(done, ", "), nil
}

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func short(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// unsnappy decodes the snappy block format (the API's bundle_url), at most max bytes out.
func unsnappy(src []byte, max int) ([]byte, error) {
	bad := errors.New("bundle_url: neither JSON nor snappy")
	n, i := 0, 0
	for shift := 0; ; shift += 7 {
		if i >= len(src) || shift > 28 {
			return nil, bad
		}
		c := src[i]
		i++
		n |= int(c&0x7f) << shift
		if c < 0x80 {
			break
		}
	}
	if n > max {
		return nil, bad
	}
	dst := make([]byte, 0, n)
	for i < len(src) {
		tag := src[i]
		i++
		var length, offset int
		switch tag & 3 {
		case 0: // literal
			length = int(tag >> 2)
			if length >= 60 {
				k := length - 59
				if i+k > len(src) {
					return nil, bad
				}
				length = 0
				for j := 0; j < k; j++ {
					length |= int(src[i+j]) << (8 * j)
				}
				i += k
			}
			length++
			if length <= 0 || i+length > len(src) || len(dst)+length > n {
				return nil, bad
			}
			dst = append(dst, src[i:i+length]...)
			i += length
			continue
		case 1:
			if i >= len(src) {
				return nil, bad
			}
			length = 4 + int(tag>>2)&7
			offset = int(tag>>5)<<8 | int(src[i])
			i++
		case 2:
			if i+2 > len(src) {
				return nil, bad
			}
			length = 1 + int(tag>>2)
			offset = int(src[i]) | int(src[i+1])<<8
			i += 2
		case 3:
			if i+4 > len(src) {
				return nil, bad
			}
			length = 1 + int(tag>>2)
			offset = int(src[i]) | int(src[i+1])<<8 | int(src[i+2])<<16 | int(src[i+3])<<24
			i += 4
		}
		if offset <= 0 || offset > len(dst) || len(dst)+length > n {
			return nil, bad
		}
		for j := 0; j < length; j++ { // copies may overlap
			dst = append(dst, dst[len(dst)-offset])
		}
	}
	if len(dst) != n {
		return nil, bad
	}
	return dst, nil
}
