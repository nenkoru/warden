# A Go project (and Node, Python)

**Pack:** `packs/go-project` · **Demo:** `scripts/demo-go-project.sh` → `examples/demo-go-project.txt`

Warden keeps a repository's dependencies free of known vulnerabilities. Each tick it reads `go.mod` (and
`requirements.txt` / `package-lock.json` if present), asks OSV about every module, and, when a fix exists, bumps it in
a **new worktree on a new branch** — never in your checkout. The update runs in the sandbox with egress only to the Go
proxy and checksum DB; the build and the tests run without network. If the bump breaks the build or the tests and a
brain is configured, the brain fixes the code in the same worktree and the checks run again. You get one commit, a PR
description, and a line in the report. Warden never pushes or merges.

## Configuration

```toml
# myproj.warden.toml — written by `warden init --pack go-project --dir ~/src/myproj`
name   = "myproj"
domain = "pack:go-project"
params = { root = "/home/me/src/myproj", test = "go test ./...", min_age_days = "14" }
mode   = "act"                                   # after a few dry-run reports you liked
wake   = { every = "6h", listen = false }
rules  = [{ act = "go-bump", then = "auto" }]    # bump alone; without a rule every bump asks

# optional: a brain for red builds after a bump, and for incidents and tasks
# brain = { acp = ["npx", "-y", "@agentclientprotocol/claude-agent-acp@0.89.1"] }

[[hooks]]
event   = "Report"                               # where the news goes: any command, JSON on stdin
command = ["sh", "-c", "cat >> ~/warden-reports.jsonl"]
```

The pack's own floors (in `packs/go-project/domain.toml`) make `go-bump` reversible, worktree-only, sandboxed with
`net = ["proxy.golang.org", "sum.golang.org"]`, and at most 10 a day. A fix version younger than `min_age_days` is
always asked, never automatic.

## What the report looks like

From `examples/demo-go-project.txt` (real OSV and Go proxy, no model):

```markdown
# Warden «greet» · 2026-10-27 19:22 · warden 0.2.0

Domain: **go-project** (`$DEMO/greet`) · mode: **act** · triage v1 · sandbox: sandbox-exec

**Worth your attention: 1** (done: 1, waiting for you: 0, important: 0).

## Done

- ✅ **golang.org/x/text v0.3.7: 3 known vulnerabilities** — branch warden/bump-golang.org-x-text-v0.41.0: golang.org/x/text v0.3.7 → v0.41.0, checks pass
  - file: `$DEMO/.warden/prs/warden_bump-golang.org-x-text-v0.41.0.md`

## Under the hood

- components in the inventory: 1
- ran `go get golang.org/x/text@v0.41.0` in sandbox-exec (net: proxy.golang.org, sum.golang.org)
- ran `go mod tidy` in sandbox-exec (net: proxy.golang.org, sum.golang.org)
- ran `go test -count=1 -run=^$ ./...` in sandbox-exec (net: none)
- ran `sh -c go test ./...` in sandbox-exec (net: none)
```

The second tick is quiet ("nothing new worth telling") and your checkout still says `v0.3.7`. To take the fix:
`git merge --ff-only warden/bump-golang.org-x-text-v0.41.0`, or push the branch and open a PR yourself.

With a brain, a bump that changes an API is ported in the worktree. `examples/live-brain-bump.txt` shows
`quic-go v0.50.0 → v0.59.1`, where `quic.Connection` became `*quic.Conn`: the build failed, the brain changed two
signatures, the checks passed, and the commit landed on `warden/bump-github.com-quic-go-quic-go-v0.59.1`.

## Node and Python

`packs/node-project` and `packs/python-project` use the same playbook (`examples/demo-npm-pypi.txt`):

- `npm-bump`: a direct dependency via `npm install PKG@FIX`, a transitive one via `overrides`; always
  `--ignore-scripts`, Warden's own npm cache, egress only to `registry.npmjs.org`.
- `pip-bump`: edits the `==` pin in `requirements.txt` (extras and markers kept), installs the set into a fresh
  `.warden-venv` in the worktree (egress only to PyPI), runs the tests there; the venv is not committed.

```markdown
## Done

- ✅ **minimist 1.2.5: 1 known vulnerability** — branch warden/bump-minimist-1.2.6: minimist 1.2.5 → 1.2.6, checks pass
- ✅ **requests 2.31.0: 3 known vulnerabilities** — branch warden/bump-requests-2.33.0: requests 2.31.0 → 2.33.0, checks pass
```

Limits: `pip-bump` needs `==` pins (no poetry/uv locks); `npm-bump` needs `package-lock.json` v2+.
