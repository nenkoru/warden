#!/bin/sh
# Demo: the npm and PyPI bumps, with real OSV and registries and no model.
#   node: minimist 1.2.5 (direct) and a vulnerable transitive dependency →
#         npm install / an override, --ignore-scripts, egress only to
#         registry.npmjs.org; the tests run without network.
#   python: requests==2.31.0 → the pin edited, a fresh venv with the set
#         installed (egress only to PyPI), the tests in it; the venv is not committed.
# Nothing is pushed. Everything lives in a temp dir.
set -eu
# portable in-place sed (GNU and BSD): sedi ARGS... FILE
sedi() { for f; do :; done; sed "$@" > "$f.sedi" && mv "$f.sedi" "$f"; }
HERE=$(cd "$(dirname "$0")/.." && pwd)
DEMO=${1:-$(mktemp -d "${TMPDIR:-/tmp}/warden-demo-pkg.XXXXXX")}
W="$DEMO/warden"
WARDEN_PACKS="$HERE/packs"; export WARDEN_PACKS
mkdir -p "$DEMO"
(cd "$HERE" && go build -o "$W" ./cmd/warden)

project() { # name pack
	cd "$DEMO/$1"
	git init -q -b main && git add . && git -c user.name=demo -c user.email=demo@localhost commit -qm init
	cd "$DEMO"
	"$W" init --pack "$2" --dir "$DEMO/$1" --config "$DEMO/$1.toml" >/dev/null
	sedi -e 's/^mode   = "dry-run".*/mode = "act"/' -e "s/^rules  = \[\].*/rules = [{ act = \"$3\", then = \"auto\" }]/" -e "s|^state .*||" "$DEMO/$1.toml"
	printf 'state = "%s"\n' "$DEMO/$1.warden" | cat - "$DEMO/$1.toml" > "$DEMO/$1.toml.new" && mv "$DEMO/$1.toml.new" "$DEMO/$1.toml"
}

rm -rf "${DEMO:?}/node" "${DEMO:?}/py" "${DEMO:?}/node.warden" "${DEMO:?}/py.warden" "${DEMO:?}/node.toml" "${DEMO:?}/py.toml"
mkdir -p "$DEMO/node" "$DEMO/py"

cd "$DEMO/node"
cat > package.json <<'JSON'
{
  "name": "argv-demo",
  "version": "1.0.0",
  "private": true,
  "scripts": { "test": "node test.js" },
  "dependencies": { "minimist": "1.2.5" }
}
JSON
cat > test.js <<'JS'
const assert = require("assert");
const argv = require("minimist")(["--name", "warden", "-v"]);
assert.strictEqual(argv.name, "warden");
assert.strictEqual(argv.v, true);
console.log("ok");
JS
npm install --ignore-scripts --no-audit --no-fund --silent >/dev/null 2>&1
printf 'node_modules/\n' > .gitignore
project node node-project npm-bump

cd "$DEMO/py"
printf 'requests==2.31.0\n' > requirements.txt
cat > test_app.py <<'PY'
import requests

def test_prepare():
    r = requests.Request("GET", "https://example.org", params={"q": "warden"}).prepare()
    assert r.url == "https://example.org/?q=warden"
PY
printf '.warden-venv/\n__pycache__/\n' > .gitignore
project py python-project pip-bump
# pytest comes from the venv, so the pins need it too
printf 'pytest==9.0.3\n' >> "$DEMO/py/requirements.txt"
git -C "$DEMO/py" -c user.name=demo -c user.email=demo@localhost commit -qam pytest
# the owner's own environment, for the every-tick health check in the checkout
(cd "$DEMO/py" && python3 -m venv .warden-venv && .warden-venv/bin/pip install -q --disable-pip-version-check -r requirements.txt)

for p in node py; do
	echo "== $p: tick"
	"$W" tick --config "$DEMO/$p.toml" || true
	echo "== $p: branches"; git -C "$DEMO/$p" branch --list 'warden/*'
	for b in $(git -C "$DEMO/$p" branch --list 'warden/*' --format='%(refname:short)'); do
		git -C "$DEMO/$p" show --stat "$b" | cat
	done
	echo "== $p: report"; cat "$DEMO/$p.warden/reports/latest.md"
done
echo "DEMO=$DEMO"
