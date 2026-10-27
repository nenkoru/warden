package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/version"
)

// Updater: one instance's self-update, inside its ticks. Begin runs at tick
// start (the rollback state machine, then the check at most every
// update.every); End at a healthy tick end (health recorded, then the
// install, the image bump or the beacon).
type Updater struct {
	Cfg        Config
	Name       string // the instance (beacons, rollback issues)
	Dir        string // <state>/update
	ConfigPath string // warden.toml: the new binary's preflight loads it
	Current    string // the running version (version.Version)
	GOOS       string
	GOARCH     string
	K8s        bool
	Now        func() time.Time
	GH         *github.Client // reads: anonymous unless token_file
	Beacon     *github.Client // nil: no beacon
	Verifier   Verifier
	Exec       func(ctx context.Context, name string, args ...string) ([]byte, error)
	LookPath   func(string) (string, error)
	NSFile     string // the service account's namespace (Kubernetes)
	// Hold: why nothing is installed this tick (the owner's pause); the
	// rollback, the health record and the beacon still run.
	Hold string

	// Restart: exit after this tick so the supervisor starts the binary now
	// in place (after an install or a rollback). Installed: the tag installed.
	Restart   bool
	Installed string

	st    State
	ready *candidate // verified this tick: installed at its healthy end
}

// candidate: a verified release.
type candidate struct {
	rel   github.Release
	tag   string
	bin   []byte
	image string // a verified image digest (sha256:…) when the release publishes one
	notes []string
}

// New: an Updater for one instance; no network until Begin.
func New(cfg Config, name, stateDir, configPath string) (*Updater, error) {
	u := &Updater{Cfg: cfg, Name: name, Dir: filepath.Join(stateDir, "update"), ConfigPath: configPath, Current: version.Version,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Now: time.Now, Exec: runCmd, LookPath: exec.LookPath,
		NSFile: "/var/run/secrets/kubernetes.io/serviceaccount/namespace"}
	u.K8s = cfg.Kubernetes || os.Getenv("KUBERNETES_SERVICE_HOST") != ""
	var err error
	if cfg.TokenFile != "" {
		u.GH, err = github.New(cfg.API, cfg.Repo, cfg.TokenFile)
	} else {
		u.GH, err = github.Anonymous(cfg.API, cfg.Repo)
	}
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	if cfg.BeaconTokenFile != "" {
		if u.Beacon, err = github.New(cfg.API, cfg.Repo, cfg.BeaconTokenFile); err != nil {
			return nil, fmt.Errorf("update.beacon_token_file: %w", err)
		}
	}
	if u.Cfg.Bin == "" {
		if exe, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			u.Cfg.Bin = exe
		}
	}
	u.Verifier = &Sigstore{GH: u.GH, Workflow: cfg.Workflow, SignerRef: cfg.SignerRef, TUFCache: filepath.Join(u.Dir, "tuf")}
	return u, nil
}

func (u *Updater) path() string { return filepath.Join(u.Dir, "state.json") }
func (u *Updater) save() error  { return saveState(u.path(), u.st) }

// off: updates are off and nothing is in flight (a manual `warden update` may have left an install pending).
func (u *Updater) off() bool { return u.Cfg.Mode == "off" && !u.st.Pending }

// semverOf: "0.2.0" or "v0.2.0" → "v0.2.0"; "dev" or junk → "".
func semverOf(s string) string {
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	if !semver.IsValid(s) {
		return ""
	}
	return s
}

// Begin: at tick start. A pending install whose binary already began a
// tick and never ended one healthy is rolled back (a finding that pings);
// otherwise the attempt is counted. Then the check, when due.
func (u *Updater) Begin(ctx context.Context) ([]domain.Finding, error) {
	st, err := loadState(u.path())
	if err != nil {
		return nil, fmt.Errorf("update state: %w", err)
	}
	u.st, u.ready, u.Restart, u.Installed = st, nil, false, ""
	if u.off() {
		return nil, nil
	}
	running := semverOf(u.Current)
	var out []domain.Finding
	if u.st.Pending && !u.K8s {
		switch {
		case running != u.st.To: // the new binary has not run yet (installed by hand, or a listen process)
			if !u.st.Restarted {
				u.Restart, u.st.Restarted = true, true
			}
		case u.st.Attempts >= 1:
			out = append(out, u.rollback(ctx))
		default:
			u.st.Attempts++
		}
	}
	if u.st.Running != running {
		u.st.Running, u.st.TicksOK, u.st.HealthySince = running, 0, time.Time{}
	}
	if err := u.save(); err != nil { // the attempt must count before the tick runs
		return out, fmt.Errorf("update state: %w", err)
	}
	if u.Cfg.Mode == "off" || !u.st.Checked.IsZero() && u.Now().Sub(u.st.Checked) < u.Cfg.every() {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	fs, err := u.check(cctx)
	out = append(out, fs...)
	if serr := u.save(); err == nil {
		err = serr
	}
	return out, err
}

// End: at tick end; healthy = the tick returned without error. Health is
// recorded first (a pending install is confirmed), then a release verified
// this tick is installed (or its image bumped), then the beacon.
func (u *Updater) End(ctx context.Context, healthy bool) error {
	if u.off() {
		return nil
	}
	if !healthy {
		u.ready = nil
		return u.save()
	}
	now, running := u.Now(), semverOf(u.Current)
	if u.st.Pending && running == u.st.To && u.st.Attempts >= 1 {
		u.st.Pending, u.st.Attempts, u.st.HealthySince = false, 0, now
	}
	if u.st.HealthySince.IsZero() {
		u.st.HealthySince = now
	}
	u.st.TicksOK++
	var errs []error
	if c := u.ready; c != nil && !u.st.Pending {
		u.ready = nil
		switch {
		case u.Hold != "":
			u.st.Waiting = c.tag + " held: " + u.Hold
		case u.K8s:
			if u.Cfg.Mode == "install" {
				u.st.Waiting = u.bumpImage(ctx, c)
			}
		case u.Cfg.Mode == "install":
			if err := u.install(ctx, c.tag, c.bin); err != nil {
				u.st.Error = err.Error()
				errs = append(errs, err)
				break
			}
			u.st.From, u.st.To, u.st.At, u.st.Pending, u.st.Attempts, u.st.Restarted = or(running, u.Current), c.tag, now, true, 0, false
			u.st.Available, u.st.Waiting = "", ""
			u.Restart, u.Installed = true, c.tag
		}
	}
	if u.Beacon != nil {
		if err := u.beacon(ctx); err != nil {
			errs = append(errs, fmt.Errorf("update beacon: %w", err))
		}
	}
	if err := u.save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// rollback: the previous binary back in place; the bad version is never
// installed again here.
func (u *Updater) rollback(ctx context.Context) domain.Finding {
	bad, from := u.st.To, u.st.From
	detail := fmt.Sprintf("warden %s was installed %s and began a tick, but never ended one healthy; %s is back in place. %s is never installed again on this instance.",
		bad, u.st.At.Format(time.DateTime), or(from, "the previous binary"), bad)
	if err := u.restore(); err != nil {
		detail += " Restoring the previous binary FAILED (" + err.Error() + "): reinstall warden by hand."
	}
	if !u.st.bad(bad) {
		u.st.Bad = append(u.st.Bad, bad)
	}
	u.st.RolledBack, u.st.RolledAt = bad, u.Now()
	u.st.Pending, u.st.Attempts, u.st.Restarted = false, 0, false
	u.Restart = true
	if u.Beacon != nil {
		if err := u.rollbackIssue(ctx, bad); err != nil {
			detail += " The warden-rollback issue could not be opened: " + err.Error()
		}
	}
	key := "rollback|" + bad
	return domain.Finding{ID: domain.ID("update", key), Key: key, Step: "update", Kind: "rollback", Severity: "high", Subject: "warden",
		Title: fmt.Sprintf("rolled back warden %s → %s", bad, or(from, "the previous binary")), Detail: detail}
}

// rolledBack: the version rolled back here, while this instance runs an older one.
func (u *Updater) rolledBack() string {
	if r := u.st.RolledBack; r != "" && semver.Compare(semverOf(u.Current), r) <= 0 {
		return r
	}
	return ""
}

// errVerify: the release failed verification (not a network hiccup).
type errVerify struct{ error }

func (e errVerify) Unwrap() error { return e.error }

// check: the newest release this instance may take, downloaded and
// verified. Findings: a release that fails verification; in propose mode
// and on Kubernetes, the verified update.
func (u *Updater) check(ctx context.Context) ([]domain.Finding, error) {
	u.st.Checked = u.Now()
	pick, waiting, err := u.plan(ctx)
	if err != nil {
		u.st.Error = err.Error()
		return nil, fmt.Errorf("update check: %w", err)
	}
	u.st.Error, u.st.Waiting = "", waiting
	if pick == nil {
		u.st.Available = ""
		return nil, nil
	}
	if pick.TagName == u.st.Available && (u.Cfg.Mode == "propose" || u.K8s && !u.Cfg.GrantImageBump) {
		return nil, nil // verified and proposed before: nothing would use the download
	}
	u.st.Available = ""
	c, err := u.fetch(ctx, *pick)
	var ve errVerify
	if errors.As(err, &ve) {
		u.st.Error = err.Error()
		key := "update-unverified|" + pick.TagName
		return []domain.Finding{{ID: domain.ID("update", key), Key: key, Step: "update", Kind: "update", Severity: "high", Subject: "warden",
			Title:  fmt.Sprintf("warden release %s fails verification — not installed", pick.TagName),
			Detail: err.Error() + "\nAn unverifiable release is never installed. If it is yours, check the release workflow's attestation; if not, someone published it.",
			Links:  []string{pick.HTMLURL}}}, nil
	}
	if err != nil {
		u.st.Error = err.Error()
		return nil, fmt.Errorf("update check: %w", err)
	}
	u.st.Available = c.tag
	if u.Cfg.Mode == "install" {
		u.ready = c
	}
	if u.K8s {
		return []domain.Finding{u.imageFinding(c)}, nil
	}
	if u.Cfg.Mode == "propose" {
		key := "update|" + c.tag
		return []domain.Finding{{ID: domain.ID("update", key), Key: key, Step: "update", Kind: "update", Severity: "low", Subject: "warden",
			Title:  fmt.Sprintf("update available: warden %s (verified; running %s)", c.tag, or(u.Current, "?")),
			Detail: "Checksum and build provenance verified (" + strings.Join(c.notes, "; ") + "). Set [update] mode = \"install\" to take it, or run `warden update`.",
			Links:  []string{c.rel.HTMLURL}}}, nil
	}
	return nil, nil
}

// plan: the newest release above the running one this instance may take,
// and why a newer one is not taken yet.
func (u *Updater) plan(ctx context.Context) (*github.Release, string, error) {
	cur := semverOf(u.Current)
	if cur == "" {
		if !u.Cfg.AllowDev {
			return nil, "a dev build does not update itself (allow_dev = true to let it)", nil
		}
		cur = "v0.0.0"
	}
	rels, err := u.GH.Releases(ctx)
	if err != nil {
		return nil, "", err
	}
	canary := u.Cfg.Channel == "canary"
	var cands []github.Release
	for _, r := range rels {
		v := semverOf(r.TagName)
		if r.Draft || v == "" || v != r.TagName || semver.Compare(v, cur) <= 0 {
			continue
		}
		if (r.Prerelease || semver.Prerelease(v) != "") && !canary {
			continue
		}
		cands = append(cands, r)
	}
	if len(cands) == 0 {
		return nil, "", nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return semver.Compare(cands[i].TagName, cands[j].TagName) > 0 })
	named, err := u.rolledBackElsewhere(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("the warden-rollback issues: %w", err) // unknown: take nothing
	}
	waiting := ""
	for i := range cands {
		r := &cands[i]
		why := ""
		switch {
		case u.st.bad(r.TagName):
			why = "rolled back here before"
		case named(r.TagName):
			why = "an open warden-rollback issue names it"
		case !canary && (r.PublishedAt == nil || u.Now().Sub(*r.PublishedAt) < time.Duration(*u.Cfg.CanaryHours)*time.Hour):
			why = fmt.Sprintf("stable waits %dh after publishing", *u.Cfg.CanaryHours)
			if r.PublishedAt != nil {
				why = fmt.Sprintf("published %s ago; %s", u.Now().Sub(*r.PublishedAt).Round(time.Minute), why)
			}
		case asset(*r, u.archiveName(r.TagName)) == nil || asset(*r, "checksums.txt") == nil:
			why = "no " + u.archiveName(r.TagName) + " or checksums.txt in it"
		}
		if why == "" {
			return r, waiting, nil
		}
		if waiting == "" {
			waiting = r.TagName + " " + why
		}
	}
	return nil, waiting, nil
}

// rolledBackElsewhere: whether an open issue labeled warden-rollback names a version.
func (u *Updater) rolledBackElsewhere(ctx context.Context) (func(tag string) bool, error) {
	is, err := u.GH.IssuesLabeled(ctx, "warden-rollback")
	if err != nil {
		return nil, err
	}
	return func(tag string) bool {
		for _, i := range is {
			if NamesVersion(i.Title, tag) || NamesVersion(i.Body, tag) {
				return true
			}
		}
		return false
	}, nil
}

// NamesVersion: whether a text names exactly this version ("v0.3.0" is not
// named by "v0.3.0-rc.1" or "v0.3.0.1", nor "v0.3.0-rc.1" by "v0.3.0-rc.10").
func NamesVersion(text, tag string) bool {
	re := regexp.MustCompile(`(?:^|[^\w.])v?` + regexp.QuoteMeta(strings.TrimPrefix(tag, "v")) + `(?:$|[^\w.-]|\.(?:$|\D))`)
	return re.MatchString(text)
}

func (u *Updater) archiveName(tag string) string {
	return fmt.Sprintf("warden_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), u.GOOS, u.GOARCH)
}

func asset(r github.Release, name string) *github.Asset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// fetch downloads and verifies a release: checksum, provenance, cosign
// (when there), the binary out of the archive, and the image digest when
// the release publishes one (verified the same way).
func (u *Updater) fetch(ctx context.Context, rel github.Release) (*candidate, error) {
	name := u.archiveName(rel.TagName)
	sums, err := u.GH.AssetBytes(ctx, asset(rel, "checksums.txt").ID, 1<<20)
	if err != nil {
		return nil, err
	}
	arch, err := u.GH.AssetBytes(ctx, asset(rel, name).ID, 512<<20)
	if err != nil {
		return nil, err
	}
	c := &candidate{rel: rel, tag: rel.TagName}
	if err := u.verifyFile(ctx, rel, sums, name, arch); err != nil {
		return nil, err
	}
	c.notes = append(c.notes, "sha256 "+short(sha(arch), 12)+" in checksums.txt", "build provenance: "+u.Cfg.Workflow+"@"+rel.TagName)
	note, err := u.cosign(ctx, rel, map[string][]byte{name: arch, "checksums.txt": sums})
	if err != nil {
		return nil, errVerify{err}
	}
	if note != "" {
		c.notes = append(c.notes, note)
	}
	if c.bin, err = extract(arch); err != nil {
		return nil, errVerify{err}
	}
	if a := asset(rel, imageAsset(rel.TagName)); a != nil {
		b, err := u.GH.AssetBytes(ctx, a.ID, 4<<10)
		if err != nil {
			return nil, err
		}
		if err := u.verifyFile(ctx, rel, sums, a.Name, b); err != nil {
			return nil, err
		}
		d := reDigest.FindString(string(b))
		if d == "" {
			return nil, errVerify{fmt.Errorf("%s holds no sha256:… digest", a.Name)}
		}
		c.image = d
	}
	return c, nil
}

// verifyFile: the file's sha256 is its checksums.txt line, and the release
// workflow's provenance names that digest.
func (u *Updater) verifyFile(ctx context.Context, rel github.Release, sums []byte, name string, content []byte) error {
	want, err := checksum(sums, name)
	if err != nil {
		return errVerify{err}
	}
	if got := sha(content); got != want {
		return errVerify{fmt.Errorf("%s: sha256 %s, checksums.txt says %s", name, got, want)}
	}
	if err := u.Verifier.Verify(ctx, Subject{Repo: u.Cfg.Repo, Tag: rel.TagName, Name: name, Digest: want}); err != nil {
		return errVerify{err}
	}
	return nil
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func or(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// Status: the report's line — version vX (update: …).
func (u *Updater) Status() string {
	v := "version " + or(semverOf(u.Current), u.Current)
	if u.off() {
		return v + " (updates off)"
	}
	s := u.st
	var what string
	switch {
	case s.Pending && semverOf(u.Current) != s.To:
		what = "installed " + s.To + ", not running yet (restart warden)"
	case s.Pending:
		what = fmt.Sprintf("installed %s, pending health (tick %d)", s.To, s.Attempts)
	case u.ready != nil && u.Cfg.Mode == "install" && !u.K8s:
		what = u.ready.tag + " verified, installing at the end of this tick"
	case u.rolledBack() != "" && u.Now().Sub(s.RolledAt) < 7*24*time.Hour:
		what = "rolled back from " + s.RolledBack
	case s.Available != "":
		what = s.Available + " available"
		if u.K8s {
			what += " (image bump proposed)"
		}
	case s.Checked.IsZero():
		what = "not checked yet"
	default:
		what = "up to date"
	}
	if s.Waiting != "" && !s.Pending {
		what += "; " + s.Waiting
	}
	if s.Error != "" {
		what += "; last check: " + short(s.Error, 200)
	}
	return v + " (update: " + what + ")"
}

// Run: `warden update [--check]` — check now (whatever update.every says),
// and unless checkOnly install the verified release (not on Kubernetes;
// not while another install waits for its healthy tick). The canary wait
// and the rollback issues hold as in a tick.
func (u *Updater) Run(ctx context.Context, checkOnly bool) (string, error) {
	st, err := loadState(u.path())
	if err != nil {
		return "", err
	}
	u.st = st
	if u.st.Pending {
		return fmt.Sprintf("%s is installed and waits for its first healthy tick: nothing else is installed until then", u.st.To), nil
	}
	u.st.Checked = u.Now()
	pick, waiting, err := u.plan(ctx)
	if err != nil {
		return "", err
	}
	if pick == nil {
		u.st.Waiting, u.st.Available = waiting, ""
		msg := "up to date (" + or(u.Current, "?") + ")"
		if waiting != "" {
			msg += "; " + waiting
		}
		return msg, u.save()
	}
	c, err := u.fetch(ctx, *pick)
	if err != nil {
		u.st.Error = err.Error()
		_ = u.save()
		return "", err
	}
	u.st.Available, u.st.Waiting, u.st.Error = c.tag, waiting, ""
	msg := fmt.Sprintf("%s verified: %s", c.tag, strings.Join(c.notes, "; "))
	switch {
	case checkOnly:
	case u.K8s:
		f := u.imageFinding(c)
		msg += "\n" + f.Title + "\n" + f.Detail
	default:
		if err := u.install(ctx, c.tag, c.bin); err != nil {
			return msg, err
		}
		running := semverOf(u.Current)
		u.st.From, u.st.To, u.st.At, u.st.Pending, u.st.Attempts, u.st.Restarted = or(running, u.Current), c.tag, u.Now(), true, 0, false
		u.st.Available = ""
		msg += fmt.Sprintf("\ninstalled %s → %s (previous kept as %s.prev); the next tick runs it — a listen/serve process restarts itself after its next tick", c.tag, u.Cfg.Bin, filepath.Base(u.Cfg.Bin))
	}
	return msg, u.save()
}
