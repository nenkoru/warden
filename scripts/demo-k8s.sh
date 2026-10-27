#!/bin/sh
# Demo: a Kubernetes namespace — FIXTURE ONLY. `kubectl` here is a fake script
# printing recorded JSON (events with BackOff warnings, a CrashLoopBackOff pod,
# two images). No cluster is contacted or started.
set -eu
# portable in-place sed (GNU and BSD): sedi ARGS... FILE
sedi() { for f; do :; done; sed "$@" > "$f.sedi" && mv "$f.sedi" "$f"; }
HERE=$(cd "$(dirname "$0")/.." && pwd)
DEMO=$(mktemp -d "${TMPDIR:-/tmp}/warden-demo-k8s.XXXXXX")
W="$DEMO/warden"
WARDEN_PACKS="$HERE/packs"; export WARDEN_PACKS
(cd "$HERE" && go build -o "$W" ./cmd/warden)
mkdir -p "$DEMO/bin" "$DEMO/fixtures"
cat > "$DEMO/fixtures/events.json" <<'JSON'
{"items":[
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api in pod api-7d9f4c","involvedObject":{"kind":"Pod","name":"api-7d9f4c"}},
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container api in pod api-7d9f4c","involvedObject":{"kind":"Pod","name":"api-7d9f4c"}},
 {"type":"Warning","reason":"FailedMount","message":"MountVolume.SetUp failed for volume \"cfg\": configmap \"api-config\" not found","involvedObject":{"kind":"Pod","name":"api-7d9f4c"}},
 {"type":"Normal","reason":"Pulled","message":"Container image already present","involvedObject":{"kind":"Pod","name":"web-5c8b"}}]}
JSON
cat > "$DEMO/fixtures/pods.json" <<'JSON'
{"items":[
 {"metadata":{"name":"api-7d9f4c"},"spec":{"containers":[{"name":"api","image":"ghcr.io/shop/api:1.4.2"}]},
  "status":{"containerStatuses":[{"name":"api","restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s restarting failed container=api"}}}]}},
 {"metadata":{"name":"web-5c8b"},"spec":{"containers":[{"name":"web","image":"nginx:1.25.3"}]},
  "status":{"containerStatuses":[{"name":"web","restartCount":0,"state":{"running":{}}}]}}]}
JSON
cat > "$DEMO/bin/kubectl" <<SH
#!/bin/sh
# FIXTURE kubectl: prints recorded JSON, never talks to a cluster
case "\$*" in
*" events "*) cat $DEMO/fixtures/events.json ;;
*" pods "*) cat $DEMO/fixtures/pods.json ;;
*" ns "*) echo namespace/shop ;;
*) echo "fixture kubectl: refusing \$*" >&2; exit 1 ;;
esac
SH
chmod +x "$DEMO/bin/kubectl"
PATH="$DEMO/bin:$PATH"; export PATH

"$W" init --pack k8s-namespace --name shop --config "$DEMO/warden.toml" >/dev/null
sedi -e 's/^params = .*/params = { context = "kind-demo", namespace = "shop" }/' -e 's/^mode   = "dry-run".*/mode = "act"/' \
  -e 's/^rules  = \[\].*/rules = [{ act = "propose", then = "auto" }]/' "$DEMO/warden.toml"
echo "== FIXTURE: kubectl is $DEMO/bin/kubectl (recorded JSON, no cluster)"
echo "== tick 1"
"$W" tick --config "$DEMO/warden.toml"
echo "== tick 2 (quiet)"
"$W" tick --config "$DEMO/warden.toml"
echo "== report (tick 1)"
cat "$(ls "$DEMO"/.warden/reports/2*.md | head -1)"
echo "== a proposal"
cat "$(ls "$DEMO"/.warden/proposals/*.md | head -1)"
echo "DEMO=$DEMO"
