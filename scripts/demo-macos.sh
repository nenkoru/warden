#!/bin/sh
# Demo: this Mac, dry-run. Builtins only read (disk, caches, health,
# Homebrew Cellar); nothing is cleaned, brew never runs. State in a temp dir.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
DEMO=$(mktemp -d "${TMPDIR:-/tmp}/warden-demo-mac.XXXXXX")
W="$DEMO/warden"
WARDEN_PACKS="$HERE/packs"; export WARDEN_PACKS
(cd "$HERE" && go build -o "$W" ./cmd/warden)
"$W" init --pack macos --name mac --config "$DEMO/warden.toml" >/dev/null
grep -q '^mode   = "dry-run"' "$DEMO/warden.toml" || { echo "not dry-run"; exit 1; }
echo "== tick (dry-run)"
"$W" tick --config "$DEMO/warden.toml"
echo "== status"
"$W" status --config "$DEMO/warden.toml"
echo "== report"
cat "$DEMO/.warden/reports/latest.md"
echo "DEMO=$DEMO"
