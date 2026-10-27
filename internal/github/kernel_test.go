package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

func TestKernel(t *testing.T) {
	in := []string{
		".github/workflows/ci.yml", ".github/dependabot.yml", ".github/ISSUE_TEMPLATE/bug.md", ".goreleaser.yaml", ".goreleaser.yml",
		"LICENSE", "NOTICE", "SECURITY.md", "warden.toml", "deploy/prod.warden.toml", "warden.lock",
		"packs/github-repo/domain.toml", "packs/github-repo/rules/x.toml", "internal/policy/policy.go", "internal/policy/sub/x.go",
		"internal/github/github.go", "internal/github/guard.go", "internal/github/guard_test.go", "internal/github/kernel.go", "internal/github/kernel_test.go",
		"internal/meter/meter.go", "internal/box/box.go", "internal/update/verify.go", "internal/update/verify_sig.go",
		"internal/update/attest.go", "internal/update/x/cosign_bundle.go", "internal/update/signature.go", "scripts/sign-release.sh",
		"scripts/attest.sh", "docs/secrets.md", "internal/x/credentials.go", "pkg/Tokens.go",
		"./.github/workflows/ci.yml", "a/../internal/policy/p.go",
	}
	for _, p := range in {
		if !github.InKernel(p) {
			t.Errorf("%s must be kernel", p)
		}
	}
	out := []string{
		"hello.go", "internal/github/githubtest/fake.go", "internal/core/signals.go", "DESIGN.md", "docs/signing.md", "packs/go-project/domain.toml",
		"internal/update/update.go", "internal/builtin/github_acts.go", "github/LICENSE.md", "deploy/x.yaml", "docs/SECURITY.md.bak",
		"internal/policyx/p.go", "scripts/build-image.sh",
	}
	for _, p := range out {
		if github.InKernel(p) {
			t.Errorf("%s must not be kernel", p)
		}
	}
	got := github.KernelFiles([]string{"hello.go", "internal/meter/meter.go", ".github/x.yml", "./internal/meter/meter.go"})
	if strings.Join(got, ",") != ".github/x.yml,internal/meter/meter.go" {
		t.Fatalf("KernelFiles: %v", got)
	}
	// this file and the guard are in their own list
	for _, p := range []string{"internal/github/kernel.go", "internal/github/guard.go"} {
		if len(github.KernelFiles([]string{p})) != 1 {
			t.Fatalf("%s must protect itself", p)
		}
	}
}

func TestMergeRefusesKernel(t *testing.T) {
	f := githubtest.New(t, tok, "warden-bot", "acme/proj")
	c, err := github.New(f.URL(), "acme/proj", tokenFile(t, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := f.AddPR("chore: loosen the kernel", "warden-bot", "warden/issue-1-x", "k1", []string{"hello.go", "internal/github/kernel.go"}, "green")
	if _, err := c.Merge(ctx, k, "k1", "x"); !errors.Is(err, github.ErrKernel) || !strings.Contains(err.Error(), "internal/github/kernel.go") {
		t.Fatalf("a kernel PR merged or the wrong error: %v", err)
	}
	ok := f.AddPR("fix: a bug", "warden-bot", "warden/issue-2-y", "o1", []string{"hello.go"}, "green")
	if _, err := c.Merge(ctx, ok, "o1", "y"); err != nil {
		t.Fatal(err)
	}
	f.With(func(f *githubtest.Fake) {
		if len(f.Merged) != 1 || f.Merged[0] != ok {
			t.Fatalf("merged %v", f.Merged)
		}
	})
}
