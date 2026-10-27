#!/bin/sh
# Demo: a Go project with a vulnerable dependency (golang.org/x/text v0.3.7).
# Real OSV and Go proxy. Warden bumps it in a new local branch (worktree,
# sandboxed, egress only to the Go proxy and checksum DB), runs the tests,
# writes a PR description; the second tick is quiet. Nothing is pushed.
# Hooks only write files. Everything lives in a temp dir.
set -eu
# portable in-place sed (GNU and BSD): sedi ARGS... FILE
sedi() { for f; do :; done; sed "$@" > "$f.sedi" && mv "$f.sedi" "$f"; }
HERE=$(cd "$(dirname "$0")/.." && pwd)
DEMO=$(mktemp -d "${TMPDIR:-/tmp}/warden-demo-go.XXXXXX")
W="$DEMO/warden"
WARDEN_PACKS="$HERE/packs"; export WARDEN_PACKS
(cd "$HERE" && go build -o "$W" ./cmd/warden)

mkdir -p "$DEMO/greet" && cd "$DEMO/greet"
cat > go.mod <<'MOD'
module example.com/greet

go 1.22

require golang.org/x/text v0.3.7
MOD
cat > main.go <<'GO'
package main

import (
	"fmt"

	"golang.org/x/text/language"
)

func main() { fmt.Println(Greet("ru")) }

// Greet names the language of tag.
func Greet(tag string) string { return "hello, " + language.Make(tag).String() }
GO
cat > main_test.go <<'GO'
package main

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("ru"); got != "hello, ru" {
		t.Fatal(got)
	}
}
GO
GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
git init -q && git add . && git -c user.name=demo -c user.email=demo@localhost commit -qm init

cd "$DEMO"
"$W" init --pack go-project --dir "$DEMO/greet" --config "$DEMO/warden.toml" >/dev/null
# act on the owner's word: bumps alone; the Report hook writes a file (no messages are sent)
sedi -e 's/^mode   = "dry-run".*/mode = "act"/' -e 's/^rules  = \[\].*/rules = [{ act = "go-bump", then = "auto" }]/' "$DEMO/warden.toml"
cat >> "$DEMO/warden.toml" <<TOML

[[hooks]]
event   = "Report"
command = ["sh", "-c", "cat > $DEMO/report-hook.json"]
TOML

echo "== tick 1"
"$W" tick --config "$DEMO/warden.toml"
echo "== branches"
git -C "$DEMO/greet" branch --list 'warden/*'
echo "== checkout untouched:"
grep x/text "$DEMO/greet/go.mod"
echo "== tick 2 (quiet)"
"$W" tick --config "$DEMO/warden.toml"
echo "== Report hook got:"
cat "$DEMO/report-hook.json"; echo
echo "== report (tick 1)"
cat "$(ls "$DEMO"/.warden/reports/2*.md | head -1)"
echo "== PR description"
cat "$DEMO"/.warden/prs/*.md
echo "DEMO=$DEMO"
