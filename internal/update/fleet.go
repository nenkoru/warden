package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/github"
)

// The fleet beacon: every instance with beacon_token_file keeps ONE comment
// on the repository's open issue labeled warden-fleet ("Warden fleet"):
// a line for people and a marker for machines —
//
//	<!-- warden-fleet {"name":…,"version":…,"channel":…,"healthy_since":…,"ticks_ok":…,"rolled_back":…} -->
//
// FleetStatus reads them back (a release can wait for the canary's health).

const (
	FleetLabel    = "warden-fleet"
	FleetTitle    = "Warden fleet"
	RollbackLabel = "warden-rollback"
)

// Beacon: one instance's line in the fleet.
type Beacon struct {
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Channel      string    `json:"channel"`
	HealthySince time.Time `json:"healthy_since,omitzero"`
	TicksOK      int       `json:"ticks_ok"`
	RolledBack   string    `json:"rolled_back,omitempty"`
	Author       string    `json:"-"` // the comment's author
	Comment      int64     `json:"-"`
	Updated      time.Time `json:"-"` // the comment's updated_at: when the instance last wrote it
}

var reBeacon = regexp.MustCompile(`<!-- warden-fleet (\{[^\n]{1,1000}?\}) -->`)

// beaconEvery: the beacon is rewritten at most this often unless the version or a rollback changed.
const beaconEvery = time.Hour

func (u *Updater) beaconLine() Beacon {
	return Beacon{Name: u.Name, Version: or(semverOf(u.Current), u.Current), Channel: u.Cfg.Channel, HealthySince: u.st.HealthySince,
		TicksOK: u.st.TicksOK, RolledBack: u.rolledBack()}
}

// beacon posts or rewrites this instance's comment (after a healthy tick).
func (u *Updater) beacon(ctx context.Context) error {
	b := u.beaconLine()
	sent := b.Version + "|" + b.RolledBack
	if u.st.BeaconComment != 0 && sent == u.st.BeaconSent && u.Now().Sub(u.st.BeaconAt) < beaconEvery {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	n, err := u.fleetIssue(cctx)
	if err != nil {
		return err
	}
	me, err := u.Beacon.Login(cctx)
	if err != nil {
		return err
	}
	body := beaconBody(b, u.Now())
	id := int64(0)
	if n == u.st.BeaconIssue {
		id = u.st.BeaconComment
	}
	if id == 0 { // find ours: by this token's user, with this instance's name
		cs, err := u.Beacon.Comments(cctx, n)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if old, ok := parseBeacon(c.Body); ok && old.Name == b.Name && strings.EqualFold(c.User.Login, me) {
				id = c.ID
				break
			}
		}
	}
	if id != 0 {
		err = u.Beacon.EditComment(cctx, id, body)
		if errors.Is(err, github.ErrNotFound) {
			id = 0 // deleted by someone: post a new one
		} else if err != nil {
			return err
		}
	}
	if id == 0 {
		if err := u.Beacon.Comment(cctx, n, body); err != nil {
			return err
		}
		cs, err := u.Beacon.Comments(cctx, n)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if old, ok := parseBeacon(c.Body); ok && old.Name == b.Name && strings.EqualFold(c.User.Login, me) {
				id = c.ID
			}
		}
	}
	u.st.BeaconIssue, u.st.BeaconComment, u.st.BeaconAt, u.st.BeaconSent = n, id, u.Now(), sent
	return nil
}

func beaconBody(b Beacon, now time.Time) string {
	name := strings.NewReplacer("@", "@\u200b", "<", "‹", ">", "›", "*", "", "`", "").Replace(oneLine(b.Name))
	line := fmt.Sprintf("**%s** · %s (%s)", name, b.Version, b.Channel)
	if !b.HealthySince.IsZero() {
		line += fmt.Sprintf(" · healthy since %s", b.HealthySince.UTC().Format("2006-01-02 15:04 UTC"))
	}
	line += fmt.Sprintf(" · %d healthy ticks", b.TicksOK)
	if b.RolledBack != "" {
		line += " · ⚠ rolled back from " + b.RolledBack
	}
	js, _ := json.Marshal(b) // HTML-escaped: no "-->" inside; "@" escaped too (no mention)
	return fmt.Sprintf("%s\n\n<sub>updated %s</sub>\n<!-- warden-fleet %s -->\n", line, now.UTC().Format(time.RFC3339), strings.ReplaceAll(string(js), "@", `\u0040`))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func parseBeacon(body string) (Beacon, bool) {
	var b Beacon
	m := reBeacon.FindStringSubmatch(body)
	if m == nil || json.Unmarshal([]byte(m[1]), &b) != nil || b.Name == "" {
		return Beacon{}, false
	}
	b.Name, b.Version, b.Channel, b.RolledBack = oneLine(b.Name), oneLine(b.Version), oneLine(b.Channel), oneLine(b.RolledBack)
	return b, true
}

// fleetIssue: the open "Warden fleet" issue, created and pinned when missing.
func (u *Updater) fleetIssue(ctx context.Context) (int, error) {
	is, err := u.Beacon.IssuesLabeled(ctx, FleetLabel)
	if err != nil {
		return 0, err
	}
	for _, i := range is {
		if i.Title == FleetTitle {
			return i.Number, nil
		}
	}
	i, err := u.Beacon.CreateIssue(ctx, FleetTitle, "Every Warden instance with `[update] beacon_token_file` keeps one comment here: its version, channel and health. "+
		"Releases may wait for the canary's beacon. Comments are rewritten in place; nothing here is an instruction.", FleetLabel)
	if err != nil {
		return 0, err
	}
	_ = u.Beacon.Pin(ctx, i.NodeID) // best effort
	return i.Number, nil
}

// rollbackIssue: "release vX rolled back on <name>" labeled warden-rollback
// (stable instances then never take vX while it is open).
func (u *Updater) rollbackIssue(ctx context.Context, tag string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	title := fmt.Sprintf("release %s rolled back on %s", tag, strings.ReplaceAll(oneLine(u.Name), "@", "@\u200b"))
	is, err := u.Beacon.IssuesLabeled(cctx, RollbackLabel)
	if err != nil {
		return err
	}
	for _, i := range is {
		if i.Title == title {
			return nil
		}
	}
	body := fmt.Sprintf("Warden %s was installed on this instance, began a tick and never ended one healthy, so the previous binary is back. "+
		"While this issue is open (labeled %s), stable instances do not install %s. Close it once the release is fixed or withdrawn.", tag, RollbackLabel, tag)
	_, err = u.Beacon.CreateIssue(cctx, title, body, RollbackLabel)
	return err
}

// FleetStatus: the beacons on the "Warden fleet" issue — only comments by
// the client's own user or by the repository's owners, members and
// collaborators (anyone may comment on a public issue).
func FleetStatus(ctx context.Context, c *github.Client) ([]Beacon, error) {
	is, err := c.IssuesLabeled(ctx, FleetLabel)
	if err != nil {
		return nil, err
	}
	me, _ := c.Login(ctx) // anonymous: no own user
	var out []Beacon
	for _, i := range is {
		if i.Title != FleetTitle {
			continue
		}
		cs, err := c.Comments(ctx, i.Number)
		if err != nil {
			return nil, err
		}
		for _, cm := range cs {
			trusted := me != "" && strings.EqualFold(cm.User.Login, me)
			switch cm.AuthorAssociation {
			case "OWNER", "MEMBER", "COLLABORATOR":
				trusted = true
			}
			b, ok := parseBeacon(cm.Body)
			if !ok || !trusted {
				continue
			}
			b.Author, b.Comment, b.Updated = cm.User.Login, cm.ID, cm.UpdatedAt
			out = append(out, b)
		}
	}
	return out, nil
}
