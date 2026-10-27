Recorded Sigstore fixtures, copied from github.com/sigstore/sigstore-go v1.3.0
(`pkg/testing/data`, Apache-2.0), used offline by verify_test.go:

- `sigstore-js-2.0.0-provenance.sigstore.json` — the npm provenance of
  sigstore@2.0.0: an SLSA provenance v1 statement signed in GitHub Actions
  (`sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main`) on
  the public-good Sigstore instance, with its Rekor entry.
- `trusted-root-public-good.json` — the public-good trusted root it verifies against.
