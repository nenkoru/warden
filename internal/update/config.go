// Package update: Warden updating itself from its GitHub releases — check,
// verify (checksum, then the release workflow's Sigstore build provenance:
// an unverifiable release is never installed), install next to the running
// binary with one previous kept, and roll back when the new binary does not
// reach a healthy tick end. On Kubernetes it never touches the binary: it
// proposes the image bump. See docs/usecases/self-update.md.
package update

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Config is [update] in warden.toml.
type Config struct {
	Mode string `toml:"mode"` // off (default) | install | propose (only report the verified version)
	Repo string `toml:"repo"` // default nenkoru/warden
	// Every: between checks (default 6h, at least 1h); the check runs inside a tick.
	Every string `toml:"every"`
	// Channel: stable (default) or canary (prereleases too, and no waiting).
	Channel string `toml:"channel"`
	// CanaryHours: a stable instance installs a release only this long after
	// it was published (default 24), and only while no open issue labeled
	// warden-rollback names it.
	CanaryHours *int   `toml:"canary_hours"`
	TokenFile   string `toml:"token_file"` // optional: reads with a token (else anonymous)
	Bin         string `toml:"bin"`        // the binary to replace (default: this executable)
	API         string `toml:"api"`        // default https://api.github.com
	AllowDev    bool   `toml:"allow_dev"`  // a "dev" build updates too (to any release)
	// Workflow and SignerRef: the identity the provenance must carry —
	// https://github.com/<repo>/<workflow>@<ref>. SignerRef is a regexp;
	// {tag} stands for the release's tag (default refs/tags/{tag}: exactly it).
	Workflow  string `toml:"workflow"`
	SignerRef string `toml:"signer_ref"`

	// Kubernetes (detected by KUBERNETES_SERVICE_HOST, or forced): the binary
	// is never touched; the update is proposed as an image bump.
	Kubernetes     bool   `toml:"kubernetes"`
	Image          string `toml:"image"`      // repo/name, without tag or digest
	Deployment     string `toml:"deployment"` // its own Deployment (grant_image_bump)
	Container      string `toml:"container"`  // default warden
	GrantImageBump bool   `toml:"grant_image_bump"`

	// BeaconTokenFile: a token that may comment on the repository: one
	// comment per instance on the "Warden fleet" issue, and a warden-rollback
	// issue when a release is rolled back here.
	BeaconTokenFile string `toml:"beacon_token_file"`
}

var reRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Normalize fills the defaults and checks the values; abs resolves a path
// relative to warden.toml.
func (c *Config) Normalize(abs func(string) string) error {
	switch c.Mode {
	case "":
		c.Mode = "off"
	case "off", "install", "propose":
	default:
		return fmt.Errorf("update.mode must be off, install or propose, not %q", c.Mode)
	}
	if c.Repo == "" {
		c.Repo = "nenkoru/warden"
	}
	if !reRepo.MatchString(c.Repo) {
		return fmt.Errorf("update.repo must be owner/name, not %q", c.Repo)
	}
	if c.Every == "" {
		c.Every = "6h"
	}
	if d, err := time.ParseDuration(c.Every); err != nil || d < time.Hour {
		return fmt.Errorf("update.every: %q (a Go duration ≥ 1h)", c.Every)
	}
	switch c.Channel {
	case "":
		c.Channel = "stable"
	case "stable", "canary":
	default:
		return fmt.Errorf("update.channel must be stable or canary, not %q", c.Channel)
	}
	if c.CanaryHours == nil {
		h := 24
		c.CanaryHours = &h
	}
	if *c.CanaryHours < 0 {
		return fmt.Errorf("update.canary_hours must not be negative")
	}
	if c.API == "" {
		c.API = "https://api.github.com"
	}
	if !strings.HasPrefix(c.API, "https://") && !strings.HasPrefix(c.API, "http://127.0.0.1") && !strings.HasPrefix(c.API, "http://localhost") {
		return fmt.Errorf("update.api must be https, not %q", c.API)
	}
	c.API = strings.TrimRight(c.API, "/")
	if c.Workflow == "" {
		c.Workflow = ".github/workflows/release.yml"
	}
	if c.SignerRef == "" {
		c.SignerRef = "refs/tags/{tag}"
	}
	if _, err := regexp.Compile(strings.ReplaceAll(c.SignerRef, "{tag}", "v1")); err != nil {
		return fmt.Errorf("update.signer_ref: %w", err)
	}
	if c.Container == "" {
		c.Container = "warden"
	}
	for _, p := range []*string{&c.TokenFile, &c.BeaconTokenFile, &c.Bin} {
		if *p != "" {
			*p = abs(*p)
		}
	}
	if c.GrantImageBump && (c.Image == "" || c.Deployment == "") {
		return fmt.Errorf("update.grant_image_bump needs image and deployment")
	}
	return nil
}

func (c *Config) every() time.Duration { d, _ := time.ParseDuration(c.Every); return d }
