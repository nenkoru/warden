package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// State is <state>/update/state.json: the install in flight (pending until
// the new binary ends a tick healthy), the versions rolled back (never
// installed again), the last check and the fleet beacon.
type State struct {
	From     string    `json:"from,omitempty"`
	To       string    `json:"to,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Pending  bool      `json:"pending"`
	Attempts int       `json:"attempts"` // ticks the new binary began while pending
	// Restarted: a restart was asked for once, the installed binary not running yet.
	Restarted bool `json:"restarted,omitempty"`

	Running      string    `json:"running,omitempty"` // the version that ran the last tick
	HealthySince time.Time `json:"healthy_since,omitzero"`
	TicksOK      int       `json:"ticks_ok"`

	Bad        []string  `json:"bad,omitempty"`         // rolled back here: never again
	RolledBack string    `json:"rolled_back,omitempty"` // the last one
	RolledAt   time.Time `json:"rolled_at,omitzero"`

	Checked   time.Time `json:"checked,omitzero"`
	Available string    `json:"available,omitempty"` // the newest verified release above the running one
	Waiting   string    `json:"waiting,omitempty"`   // why a newer one is not taken yet
	Error     string    `json:"error,omitempty"`     // the last check's failure

	BeaconIssue   int       `json:"beacon_issue,omitempty"`
	BeaconComment int64     `json:"beacon_comment,omitempty"`
	BeaconAt      time.Time `json:"beacon_at,omitzero"`
	BeaconSent    string    `json:"beacon_sent,omitempty"` // what the last beacon said (version · rolled back)
}

func loadState(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) bad(v string) bool {
	for _, b := range s.Bad {
		if b == v {
			return true
		}
	}
	return false
}
