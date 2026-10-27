# Warden

A small, always-on agent bound to one **domain**: a project, a Mac, a Kubernetes namespace, a server — anything you can
describe in a manifest. Warden watches the domain, fixes small things on its own inside hard limits, and calls you only
when it matters, with the work already prepared: a branch with green tests, a proposal file, a ready diff. What is left
for you is one command: `warden approve ID`.

[Русская версия](README.ru.md) · [Design](DESIGN.md) · [Use cases](docs/usecases/) · [Install as a service](INSTALL.md)

## Philosophy

- **Minimal.** One Go binary (~15 MB) with three dependencies (go-toml, the MCP Go SDK, the ACP Go SDK). No database,
  no daemon framework, no web UI. State is plain files next to `warden.toml`.
- **Durable.** Every step is journaled (`journal.jsonl`, begin/end), state is written atomically, one tick runs at a
  time (flock), actions carry idempotency keys. After a crash the next tick cleans up the abandoned worktree and says so.
- **Wakes on a timer or a signal.** launchd/systemd run a tick every interval and nothing runs in between; or one
  process sleeps on its streams (`journalctl -f`, `kubectl get events -w`, a chat adapter) and wakes when a line arrives.
- **Self-improving, behind a gate.** After Harness^2 (propose → probe → compose, "keep current" is a valid outcome), but
  narrow: only the triage ("is this worth telling you?") learns, from your labels. Every candidate must pass compiled-in
  invariants (a high vulnerability, low disk or a failing check can never be hidden) and be strictly better on your
  labels. Floors, grants, rules and the evaluator are out of its reach. `warden rollback` undoes it.
- **Human near the loop.** Not in the loop (approving every step) and not out of it (never asked). Warden acts alone on
  what is reversible and allowed, asks with the work prepared, only reports what it has no grant for, stays silent on
  quiet ticks, and pings less when you don't answer.
- **One warden per whatever.** A domain is a manifest (`domain.toml`): what to observe, what may change, how to verify,
  where news comes from. Nothing about projects or Macs is hard-coded; those are packs among others.
- **Standard integrations, no lock-in.** MCP (it uses MCP servers and is one), ACP or any Anthropic/OpenAI-compatible
  endpoint as the brain, Agent Skills, Claude Code-shaped hooks, A2A 1.0, plain signals (RSS, webhooks, folders,
  streams, any CLI printing JSON lines).
- **Existing registries.** Extensions come from the MCP Registry, Agent Skills catalogs and Claude Code plugin
  marketplaces, and Pi packages — pinned with integrity in `warden.lock`. Warden has no package format of its own.
- **Fail-closed safety.** Acts run in the OS sandbox (`sandbox-exec` on macOS, `bwrap` on Linux) with an egress
  allowlist. With no sandbox available, Warden observes and reports but refuses to act, unless you explicitly set
  `sandbox = "none"`, which then shows in every report header.
- **Small footprint.** Between ticks nothing runs. The model is optional and used only for judgement calls; the loop
  works without it.

## Quick start

```bash
git clone https://github.com/nenkoru/warden && cd warden
go build -o ~/bin/warden ./cmd/warden          # Go 1.26+; or download a release archive (binary + packs)
export WARDEN_PACKS=$PWD/packs                 # where `pack:NAME` is found (also any packs/ above the config or binary)

warden init --pack go-project --dir ~/src/myproj --config ~/src/myproj.warden.toml   # writes mode = "dry-run"
warden tick    --config ~/src/myproj.warden.toml   # one pass; the report is in .warden/reports/latest.md
warden status  --config ~/src/myproj.warden.toml   # what waits for you
warden approve a_1b2c3d4e --config ~/src/myproj.warden.toml
warden units   --config ~/src/myproj.warden.toml   # writes launchd/systemd units; you load them (see INSTALL.md)
```

When the dry-run reports look right, set `mode = "act"` and rules such as
`rules = [{ act = "go-bump", then = "auto" }]`. Without a rule an act asks; three approvals in a row make it automatic,
one decline pins it to ask.

A report looks like this (from `scripts/demo-go-project.sh`, real OSV and Go proxy, no model):

```markdown
# Warden «greet» · 2026-10-27 19:22 · warden 0.2.0

Domain: **go-project** (`$DEMO/greet`) · mode: **act** · triage v1 · sandbox: sandbox-exec

**Worth your attention: 1** (done: 1, waiting for you: 0, important: 0).

## Done

- ✅ **golang.org/x/text v0.3.7: 3 known vulnerabilities** — branch warden/bump-golang.org-x-text-v0.41.0: golang.org/x/text v0.3.7 → v0.41.0, checks pass
  - file: `$DEMO/.warden/prs/warden_bump-golang.org-x-text-v0.41.0.md`
```

## Packs

`warden packs` lists them. A pack is a folder with `domain.toml`, optional `skills/` and `.mcp.json`.

| pack | what it does | page |
|---|---|---|
| `go-project` | Inventory from go.mod, OSV advisories, upstream releases, tests as health. Fix: `go-bump` in a new worktree branch, sandboxed, with a PR description; on a red build the brain fixes it in the same worktree. Never pushes or merges. | [go-project](docs/usecases/go-project.md) |
| `node-project` | The same for npm: direct deps via `npm install PKG@FIX`, transitive via `overrides`; `--ignore-scripts`, egress only to registry.npmjs.org. | [go-project](docs/usecases/go-project.md#node-and-python) |
| `python-project` | The same for PyPI: edits the `==` pin in requirements.txt, installs the set in a fresh venv in the worktree (egress only to PyPI), runs the tests there. | [go-project](docs/usecases/go-project.md#node-and-python) |
| `k8s-namespace` | Warning events and CrashLoopBackOff become incidents, images become inventory, a fix is a proposal file. `kubectl apply` only with the `k8s.apply` grant, always asked. | [k8s-namespace](docs/usecases/k8s-namespace.md) |
| `macos` | Disk, regenerable caches (cleaned only by the tool's own command, only from your list, with the `mac.clean` grant), battery and thermals, Homebrew as inventory. Starts in dry-run. | [macos](docs/usecases/macos.md) |
| `linux-server` | Read-only: disk, failed systemd units, journal errors, dpkg packages as inventory, CISA advisories. Every fix is a proposal. | [linux-server](docs/usecases/linux-server.md) |
| `github-repo` | Maintains one GitHub repository: works issues into draft PRs (CI decides; one fix round from the logs), reviews outside PRs with one comment (never merges them, never runs their code), merges its own, Dependabot's and trusted agents' PRs on green CI, cuts patch/minor releases with a generated CHANGELOG. Kill switch (`warden-pause` label or `PAUSE` file), protected paths → `needs-human`, untrusted text never drives a merge or release, a daily model budget, several brains routed per task, delegation to other agents. | [github-maintainer](docs/usecases/github-maintainer.md) |
| `linux-host` | A GPU/model-serving host: disk, failed units (system and user), GPU temperature and ECC, the health of the service it exists for, with quiet hours. Read-only. | [linux-server](docs/usecases/linux-server.md#linux-host) |

Also: [signals — news, alerts and messages](docs/usecases/signals.md) and
[writing your own domain pack](docs/usecases/writing-a-pack.md).

## Integrations

- **MCP.** Domains take tools from MCP servers (stdio or streamable HTTP); stdio servers run in the sandbox with their
  own host allowlist. Warden is an MCP server too: `warden mcp --config …` exposes `warden_status`, `warden_report`,
  `warden_pending`, `warden_approve`, `warden_signal` and the resource `warden://report/latest`.
- **Brains.** Any ACP agent (`brain = { acp = [...] }`: Claude Code, Gemini CLI, Codex, pi) or an
  Anthropic/OpenAI-compatible endpoint (Claude, GLM via Z.ai, OpenRouter, local Ollama/vLLM) with Warden's own tool
  loop. An ACP agent runs like an act: sandboxed, writing only its own folder and the worktree it was given, egress only
  to the model's hosts. Claude Code runs isolated: its own config dir, none of your settings, MCP servers, plugins,
  hooks, CLAUDE.md or memory; your login only by reference (or `CLAUDE_CODE_OAUTH_TOKEN`).
- **Agent Skills.** The pack's `skills/` and skills from extensions reach the brain as an index; `read_skill` loads one.
- **Hooks** in the Claude Code shape: `Wake`, `Finding`, `PreAction` (may block: exit 2 or `{"decision":"block"}`),
  `PostAction`, `Ask`, `Report`, `Signal`; JSON on stdin. Hooks are how Warden reaches Telegram, a team chat, an issue
  tracker or mail. Warden itself sends nothing.
- **A2A 1.0.** `warden serve` serves an agent card and JSON-RPC (`SendMessage`, `GetTask`; the 0.3 names are accepted),
  plus `POST /signal` and `GET /report`. Senders are identified by bearer token; trust tiers come from `[serve]`.
- **Registries.** `warden add mcp:…|skill:…|plugin:…@marketplace|pi:…` resolves, downloads, verifies and writes
  `warden.lock`; `warden verify` re-checks the bytes. An MCP server that is not in the lock or fails verification is not
  started, and the report says what to run. Plugin hooks stay off until enabled by name (`enable_hooks`).
- **Signals.** RSS/Atom, commands printing JSON lines (an adapter to any chat), drop folders, streams in `warden listen`,
  webhooks and A2A through `warden serve`, and `warden signal TEXT`. The sender's trust tier caps what a signal can
  cause; claims are verified (an advisory is looked up in OSV and matched against the real inventory).

## Safety

- **The sandbox fails closed.** Writes only to the working folder; egress through Warden's own allowlist proxy. With no
  runner, Warden still observes and reports, but refuses acts, sandboxed checks, stdio MCP servers, the ACP brain, hooks
  and replies, with the line `no sandbox available: set sandbox = "none" to run unsandboxed — your choice`.
- **Floors are code, not prompts:** `reversible`, `always_ask`, `grant`, `max_per_day`, `dry_run_only`,
  `where = "worktree"`, `sandbox` + `net`. The policy decides in order: deny rules → floors → rules and pins → learned
  trust → budgets → dry-run.
- **What Warden never does:** push or merge (except the `github-repo` pack's maintainer mode, which pushes only its own
  `warden/` branches over a token from a file and merges only under the rules in
  [its page](docs/usecases/github-maintainer.md)), delete your data (only a tool's own clean command, never a generic `rm`),
  touch your agent configs, or send messages for you unless `replies = true`. New domains start in dry-run.
- **By design outside the sandbox:** `tool-clean` runs a cache tool's own clean command unsandboxed (a "write only here"
  box would break `go clean -cache`). It is gated by the `mac.clean` grant, your allowlist and an exact argv match, and
  is refused when no sandbox is available at all.
- **Ubuntu 24.04:** AppArmor blocks unprivileged user namespaces, so `bwrap` fails and Warden reports "no sandbox". A
  narrow AppArmor profile for `/usr/bin/bwrap` fixes it (your decision, needs sudo); see [SECURITY.md](SECURITY.md).

Threat model and reporting: [SECURITY.md](SECURITY.md).

## Status and limits

Version 0.1.0. The demos in `scripts/` run without a model and their output is in `examples/`; `scripts/live-brain.sh`
spends model tokens and was run with Claude Code over ACP (bump that broke the build ported by the brain, an incident
RCA from a log stream, an owner task turned into a tested branch). Known limits:

- No self-update (`warden update`); rebuild manually.
- The `mcp` signal source (polling an MCP tool) is not implemented; wrap it in a `command` source.
- ACP brains don't get Warden's own tools; Warden runs the checks after them. Their reads are bounded by the sandbox
  (secrets dirs closed), not by the worktree; writes by both.
- `pip-bump` needs `==` pins in requirements.txt (no poetry/uv locks); `npm-bump` needs package-lock.json v2+.
- A "read-only" MCP server is recognized by its `--read-only` argument.
- Registries: npm per-platform optional dependencies are noted, not pinned; `uvx` resolves transitive deps at run time;
  inline hooks/mcpServers in `plugin.json` are noted, not extracted.
- The macOS pack measures caches with `du`, which can take seconds; `params.skip = "caches,library-caches"` turns
  observers off.
- Packs are looked up on disk (`WARDEN_PACKS`, or a `packs/` folder above the config, the working directory or the
  binary); `go install` alone doesn't bring them.

## Docs

- [DESIGN.md](DESIGN.md) — the manifest, the loop, signals, extensions, hooks, the brain, self-improvement, safety
- [docs/usecases/](docs/usecases/) — one page per pack, signals, writing a pack
- [INSTALL.md](INSTALL.md) — running it as a launchd/systemd user service
- [docs/internals.md](docs/internals.md) — package APIs
- [research/](research/) — notes behind the design (registries, durable agents, near-the-loop)
- [CONTRIBUTING.md](CONTRIBUTING.md) · [SECURITY.md](SECURITY.md) · [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) ·
  [CHANGELOG.md](CHANGELOG.md)

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
