package core

import (
	"context"

	"github.com/nenkoru/warden/internal/domain"
	"github.com/nenkoru/warden/internal/version"
)

// Self-update inside the tick (internal/update): at its start the rollback
// state machine and, at most every update.every, the check (a rollback or a
// proposed update is a finding of this tick); at a healthy end the install.

func (in *Instance) updateBegin(ctx context.Context, t *Tick) []domain.Finding {
	if in.Upd == nil {
		if in.updErr != nil {
			t.Errors = append(t.Errors, in.updErr.Error())
		}
		return nil
	}
	fs, err := in.Upd.Begin(ctx)
	if err != nil {
		t.Errors = append(t.Errors, "update: "+err.Error())
	}
	for _, f := range fs {
		if f.Kind == "rollback" {
			in.St.Journal("update.rollback", map[string]any{"title": f.Title})
		}
	}
	return fs
}

// updateEnd: healthy = the tick saved its state and returns no error.
func (in *Instance) updateEnd(ctx context.Context, t *Tick, healthy bool) {
	if in.Upd == nil {
		return
	}
	in.Upd.Hold = in.paused
	if err := in.Upd.End(ctx, healthy); err != nil {
		t.Errors = append(t.Errors, "update: "+err.Error())
		in.St.Journal("update.error", map[string]any{"error": err.Error()})
	}
	if in.Upd.Installed != "" {
		in.St.Journal("update.installed", map[string]any{"from": version.Version, "to": in.Upd.Installed})
	}
	t.Restart = t.Restart || in.Upd.Restart
}

// versionLine: the report's "version vX (update: …)".
func (in *Instance) versionLine() string {
	if in.Upd == nil {
		return "version " + version.Version
	}
	return in.Upd.Status()
}

// UpdateNow: `warden update [--check]`.
func (in *Instance) UpdateNow(ctx context.Context, checkOnly bool) (string, error) {
	if in.Upd == nil {
		return "", in.updErr
	}
	return in.Upd.Run(ctx, checkOnly)
}
