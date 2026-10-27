#!/bin/sh
# Live brain run. It spends model tokens, so run it on purpose. The brain is
# Claude Code through ACP (claude-agent-acp), isolated and sandboxed, using the
# owner's Claude Code login by reference. It makes three judgement calls on one
# small Go project:
#   1. a security bump that breaks the build (quic-go v0.50.0 → v0.59.1:
#      quic.Connection became *quic.Conn). The brain ports the code in the
#      worktree, the checks must pass, and the branch is committed;
#   2. an incident from a log stream (`warden listen` on tail -F): RCA;
#   3. an owner task through the local inbox: a branch with tests.
# OSV and the Go proxy are real. Nothing is pushed or sent (hooks write files).
# Usage: scripts/live-brain.sh [DIR] [STEP...]   steps: setup bump incident task (default: all)
set -eu
# portable in-place sed (GNU and BSD): sedi ARGS... FILE
sedi() { for f; do :; done; sed "$@" > "$f.sedi" && mv "$f.sedi" "$f"; }
HERE=$(cd "$(dirname "$0")/.." && pwd)
DEMO=${1:-$(mktemp -d "${TMPDIR:-/tmp}/warden-live.XXXXXX")}
[ $# -gt 0 ] && shift
STEPS=${*:-setup bump incident task}
W="$DEMO/warden"
WARDEN_PACKS="$HERE/packs"; export WARDEN_PACKS
ACP=${WARDEN_ACP:-'"npx", "-y", "@agentclientprotocol/claude-agent-acp@0.89.1"'}
mkdir -p "$DEMO"
(cd "$HERE" && go build -o "$W" ./cmd/warden)

setup() {
	rm -rf "${DEMO:?}/relay" "${DEMO:?}/.warden" "${DEMO:?}/warden.toml"
	mkdir -p "$DEMO/relay" && cd "$DEMO/relay"
	cat > go.mod <<'MOD'
module example.com/relay

go 1.24
MOD
	cat > relay.go <<'GO'
// Package relay forwards streams between QUIC peers.
package relay

import (
	"context"
	"io"
	"time"

	"github.com/quic-go/quic-go"
)

// Config: the relay's QUIC settings.
func Config() *quic.Config {
	return &quic.Config{MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second}
}

// Peer names a connection for the logs.
func Peer(c quic.Connection) string { return c.RemoteAddr().String() }

// Pipe copies one stream of a to a new stream of b.
func Pipe(ctx context.Context, a, b quic.Connection) error {
	in, err := a.AcceptStream(ctx)
	if err != nil {
		return err
	}
	out, err := b.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return err
}
GO
	cat > relay_test.go <<'GO'
package relay

import (
	"testing"
	"time"
)

func TestConfig(t *testing.T) {
	c := Config()
	if c.MaxIdleTimeout != 30*time.Second || c.KeepAlivePeriod != 10*time.Second {
		t.Fatal(c)
	}
}
GO
	GOFLAGS=-mod=mod go get github.com/quic-go/quic-go@v0.50.0 >/dev/null 2>&1
	GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
	go test ./... >/dev/null
	printf '.warden/\n' > .gitignore
	git init -q -b main && git add . && git -c user.name=demo -c user.email=demo@localhost commit -qm init
	cd "$DEMO"
	"$W" init --pack go-project --dir "$DEMO/relay" --config "$DEMO/warden.toml" >/dev/null
	sedi -e 's/^mode   = "dry-run".*/mode = "act"/' -e 's/^rules  = \[\].*/rules = [{ act = "go-bump", then = "auto" }]/' "$DEMO/warden.toml"
	# top-level keys must come before the first table
	{ printf 'brain = { acp = [%s] }\n' "$ACP"; cat "$DEMO/warden.toml"; } > "$DEMO/warden.toml.new" && mv "$DEMO/warden.toml.new" "$DEMO/warden.toml"
	cat >> "$DEMO/warden.toml" <<TOML

[[signals]]
name  = "app"
kind  = "stream"
argv  = ["tail", "-n0", "-F", "$DEMO/app.log"]
trust = "trusted"

[[hooks]]
event   = "Report"
command = ["sh", "-c", "cat > $DEMO/report-hook.json"]
TOML
	: > "$DEMO/app.log"
	echo "== setup: $DEMO/relay (quic-go v0.50.0), config $DEMO/warden.toml"
}

bump() {
	echo "== tick: OSV → bump quic-go; the build breaks; the brain ports the code"
	"$W" tick --config "$DEMO/warden.toml"
	echo "== branches"; git -C "$DEMO/relay" branch --list 'warden/*'
	b=$(git -C "$DEMO/relay" branch --list 'warden/bump-*' --format='%(refname:short)' | head -1)
	if [ -n "$b" ]; then git -C "$DEMO/relay" show --stat "$b" | cat; git -C "$DEMO/relay" diff main "$b" -- '*.go' | cat; fi
	echo "== checkout untouched:"; grep quic-go "$DEMO/relay/go.mod"
}

incident() {
	echo "== listen on the app log; a panic arrives"
	before=$(ls "$DEMO"/.warden/reports/2*.md 2>/dev/null | wc -l)
	"$W" listen --config "$DEMO/warden.toml" > "$DEMO/listen.log" 2>&1 &
	pid=$!
	sleep 3
	cat >> "$DEMO/app.log" <<'LOG'
2026/10/27 21:14:03 relay: accepted 10.0.4.17:51822
panic: runtime error: invalid memory address or nil pointer dereference in relay.Pipe: b is nil (relay.go:22, called from relay.Serve when the upstream dial failed)
LOG
	i=0
	# the report of the tick that saw the panic (the startup tick may write one of its own)
	while [ $i -lt 600 ] && ! grep -lqs "nil pointer dereference in relay.Pipe" $(ls "$DEMO"/.warden/reports/2*.md 2>/dev/null | tail -n +$((before+1))) /dev/null; do sleep 3; i=$((i+3)); done
	kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true
	echo "== listen log"; cat "$DEMO/listen.log"
	echo "== latest report"; cat "$DEMO/.warden/reports/latest.md"
}

task() {
	echo "== the owner's task, through the local inbox"
	"$W" signal --config "$DEMO/warden.toml" "warden task add func Version() string to package relay: the version of github.com/quic-go/quic-go from runtime/debug.ReadBuildInfo, or \"unknown\" when it is not there; with a test"
	b=$(git -C "$DEMO/relay" branch --list 'warden/task-*' --format='%(refname:short)' | head -1)
	if [ -n "$b" ]; then git -C "$DEMO/relay" show --stat "$b" | cat; git -C "$DEMO/relay" diff main "$b" | cat; fi
	echo "== latest report"; cat "$DEMO/.warden/reports/latest.md"
}

for s in $STEPS; do $s; done
echo "DEMO=$DEMO"
