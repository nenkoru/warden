#!/usr/bin/env bash
# Replays tasks of the 27B experiment log (internal/builtin/replay_test.go)
# against a live model on two harnesses: an older tree (WARDEN_REPLAY_OLD,
# default origin/main) and this one, then prints them side by side. Nothing
# is pushed; the model works in scratch worktrees of the commits the tasks
# were worked on. Runs a long time on a local 27B: start it in the
# background.
#
#   OPENAI_API_KEY=… scripts/replay-27b.sh http://litellm.example/v1 qwen3.8-27b
#
# WARDEN_REPLAY_RUNS (default 1) runs per task and harness; WARDEN_REPLAY_ONLY
# (e.g. issue-18) picks tasks; WARDEN_REPLAY_EXTRA (a JSON object, e.g.
# '{"chat_template_kwargs":{"enable_thinking":true}}') adds a third run of
# this harness with those request fields.
set -euo pipefail
url=${1:?usage: replay-27b.sh BASE_URL MODEL}
model=${2:?usage: replay-27b.sh BASE_URL MODEL}
here=$(cd "$(dirname "$0")/.." && pwd)
old=${WARDEN_REPLAY_OLD:-origin/main}
tmp=$(mktemp -d)
out=${WARDEN_REPLAY_OUT:-$tmp/replay.jsonl}
git -C "$here" worktree add -q --detach "$tmp/old" "$old"
trap 'git -C "$here" worktree remove --force "$tmp/old"' EXIT
cp "$here/internal/builtin/replay_test.go" "$tmp/old/internal/builtin/"
cp "$here/scripts/replay/old_shim.go.txt" "$tmp/old/internal/builtin/replay_shim_test.go"
export WARDEN_REPLAY_URL=$url WARDEN_REPLAY_MODEL=$model WARDEN_REPLAY_KEY_ENV=${WARDEN_REPLAY_KEY_ENV:-OPENAI_API_KEY} \
	WARDEN_REPLAY_OUT=$out WARDEN_REPLAY_REPO=$here
replay() { (cd "$1" && WARDEN_REPLAY_HARNESS=$2 go test -tags replay -run TestReplay -count=1 -timeout 6h -v ./internal/builtin | grep -E '^(=== RUN|--- |\s+replay_test)' || true); }
replay "$tmp/old" before
extra=${WARDEN_REPLAY_EXTRA:-}
WARDEN_REPLAY_EXTRA= replay "$here" after
[ -n "$extra" ] && WARDEN_REPLAY_EXTRA=$extra replay "$here" after+extra
echo "results: $out"
python3 -I - "$out" <<'PY'
import json, sys, collections
rows = [json.loads(l) for l in open(sys.argv[1])]
g = collections.defaultdict(list)
for r in rows:
    g[(r["task"], r["harness"])].append(r)
print("| task | harness | runs | green in the right place | avg steps | avg tool calls | bad calls (unknown+args) | repeats | finish | avg tokens in / out | avg wall |")
print("|---|---|---|---|---|---|---|---|---|---|---|")
for (task, h), rs in sorted(g.items()):
    n = len(rs)
    avg = lambda k: sum(int(r.get(k, 0)) for r in rs) / n
    ok = sum(1 for r in rs if r["build"] == "green" and r["right_place"])
    bad = sum(int(r.get("unknown_tool", 0)) + int(r.get("bad_args", 0)) for r in rs)
    fin = ",".join(sorted({r.get("finish_reason", "?") for r in rs}))
    print(f"| {task} | {h} | {n} | {ok}/{n} | {avg('steps'):.0f} | {avg('tool_calls'):.0f} | {bad} | {sum(int(r.get('repeats',0)) for r in rs)} | {fin} | {avg('tokens_in')/1000:.0f}k / {avg('tokens_out')/1000:.1f}k | {avg('wall_s'):.0f}s |")
PY
