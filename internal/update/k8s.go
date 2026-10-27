package update

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/domain"
)

// Kubernetes: the pod's image is pinned by digest and its root filesystem is
// read-only, so the binary is never touched. The update is proposed as an
// image bump; with grant_image_bump the instance may run `kubectl set image`
// on its own Deployment — and only with an image digest the release itself
// publishes and that passed the same verification as the archive.

var (
	reDigest  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	reK8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// imageAsset: the optional release asset holding the image's digest (one line, sha256:…).
func imageAsset(tag string) string {
	return "warden_" + strings.TrimPrefix(tag, "v") + "_image-digest.txt"
}

// imageFinding: the proposed image bump.
func (u *Updater) imageFinding(c *candidate) domain.Finding {
	img := or(u.Cfg.Image, "<image>")
	title := fmt.Sprintf("update available: warden %s — set image to %s@sha256:… after building it", c.tag, img)
	detail := fmt.Sprintf("The release is verified (%s). Build the image for %s (scripts/build-image.sh at the tag), push it, and pin its digest in the Deployment.",
		strings.Join(c.notes, "; "), c.tag)
	if c.image != "" {
		title = fmt.Sprintf("update available: warden %s — set image to %s@%s", c.tag, img, c.image)
		detail = fmt.Sprintf("The release is verified (%s) and publishes this image digest (verified too). Pin it in the Deployment.", strings.Join(c.notes, "; "))
	}
	if u.Cfg.GrantImageBump && u.Cfg.Mode == "install" {
		detail += " grant_image_bump is set: the bump runs at the end of a healthy tick when the digest is published."
	}
	key := "update|" + c.tag
	return domain.Finding{ID: domain.ID("update", key), Key: key, Step: "update", Kind: "update", Severity: "low", Subject: "warden",
		Title: title, Detail: detail, Links: []string{c.rel.HTMLURL}}
}

// bumpImage: the grant path — what happened, in words.
//
// TODO(grant path): this is the minimal form: one `kubectl set image` on the
// instance's own Deployment. Not yet: a policy gate (an ask before the first
// bump), watching the rollout and `kubectl rollout undo` when the new pod
// never reports healthy. Until then it refuses unless every condition holds.
func (u *Updater) bumpImage(ctx context.Context, c *candidate) string {
	switch {
	case !u.Cfg.GrantImageBump:
		return "image bump not granted (grant_image_bump)"
	case c.image == "":
		return c.tag + ": the release publishes no image digest — image bump not granted"
	case !reK8sName.MatchString(u.Cfg.Deployment) || !reK8sName.MatchString(u.Cfg.Container) || strings.ContainsAny(u.Cfg.Image, " @\n"):
		return "image bump not granted: deployment, container or image is not a plain name"
	}
	kubectl, err := u.LookPath("kubectl")
	if err != nil {
		return "image bump not granted: no kubectl in PATH"
	}
	ns, err := os.ReadFile(u.NSFile)
	if err != nil || !reK8sName.MatchString(strings.TrimSpace(string(ns))) {
		return "image bump not granted: the service account's namespace is unknown"
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ref := u.Cfg.Image + "@" + c.image
	out, err := u.Exec(cctx, kubectl, "-n", strings.TrimSpace(string(ns)), "set", "image", "deployment/"+u.Cfg.Deployment, u.Cfg.Container+"="+ref)
	if err != nil {
		return fmt.Sprintf("image bump not granted: kubectl set image failed: %v %s", err, short(string(out), 200))
	}
	return "image bumped to " + ref + " (" + c.tag + ")"
}
