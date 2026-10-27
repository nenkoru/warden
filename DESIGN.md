# Warden — design (v1, standalone)

Warden is a small, always-on agent bound to one **domain**: a project, a Mac, a Kubernetes cluster or namespace, a
fleet of servers, anything. It watches the domain, keeps it healthy, fixes small things on its own inside hard limits,
and brings its human **near the loop** only for what matters: the work is prepared, the human decides with one
approve. It wakes on a **timer** or on a **signal**, keeps its state on disk, and uses a model only for judgement
calls.

Warden is its own product. It does not import any other harness and does not depend on any network or chat.
Everything around it plugs in through the mechanisms agent harnesses already share:

| need | standard mechanism Warden uses |
|---|---|
| tools for observing/acting (kubectl, GitHub, a chain network, an issue tracker…) | **MCP servers** (stdio/HTTP), installed from the **MCP Registry** or any `.mcp.json` |
| instructions / know-how for a domain | **Agent Skills** (`SKILL.md` folders), from skill catalogs or Claude Code plugin marketplaces |
| bring your own agent backend | **ACP** (Claude Code, Gemini CLI, Codex, pi …) or any **Anthropic-/OpenAI-compatible endpoint** |
| react to lifecycle events (notify, block, audit) | **hooks** in the Claude Code shape: event + matcher + command, JSON on stdin, exit 2 / `{"decision":"block"}` blocks |
| talk to other agents | **A2A** (agent card + JSON-RPC); also any MCP tool |
| news, alerts, messages | **signals**: RSS/Atom, webhooks (HTTP POST), drop folders, log streams, any CLI printing JSON lines, MCP tools |
| control from other harnesses | Warden is itself an **MCP server** (`warden mcp`: status, report, approve, signal) and an **A2A agent** (`warden serve`) |
| packaging | a **domain pack** = a folder with `domain.toml` + `skills/` + `.mcp.json`, or just references into existing registries |

Sources for the ideas are listed at the end. The research notes behind them are in `research/`.

## 1. The domain manifest

A domain says five things: what to **watch**, what Warden may **change**, how to **verify** a change, where **news**
comes from, and how to **report**. Nothing about projects or Macs is hard-coded: those are two packs among many.

```toml
# an illustrative k8s-namespace manifest (the shipped one is packs/k8s-namespace/domain.toml)
name        = "k8s-namespace"
description = "One Kubernetes namespace: workload health, image news, proposed fixes as manifests"
params      = { namespace = "default", context = "" }     # filled from warden.toml

[[observe]]                                # → findings
name  = "warnings"
run   = { mcp = "kubernetes", tool = "events_list", args = { namespace = "{namespace}" } }
map   = { items = "[]", where = { type = "Warning" }, key = "{involvedObject.kind}/{involvedObject.name}/{reason}",
          title = "{reason}: {involvedObject.kind}/{involvedObject.name}", detail = "{message}", severity = "medium" }

[[inventory]]                              # → components (name, version, ecosystem): news and advisories match these
name = "images"
run  = { cmd = ["kubectl", "get", "pods", "-n", "{namespace}", "-o", "json"] }
map  = { items = "items[].spec.containers[]", component = "{image}" }          # nginx:1.25.3 → name nginx, version 1.25.3

[[act]]                                    # actions Warden may take, each with its own floors
name   = "propose-manifest"               # default: write the fixed manifest + diff for the human
run    = { builtin = "propose" }
floors = { reversible = true }
[[act]]
name   = "apply"
run    = { cmd = ["kubectl", "apply", "-n", "{namespace}", "-f", "{file}"] }
floors = { grant = "k8s.apply", always_ask = true }  # without the grant in warden.toml: report-only

[[check]]                                  # after an action, and every tick as a health probe
name = "rollout"
run  = { cmd = ["kubectl", "rollout", "status", "-n", "{namespace}", "deploy/{target}", "--timeout=120s"] }

[feeds]
rss = ["https://www.cisa.gov/cybersecurity-advisories/all.xml"]
osv = true                                 # OSV for every inventory component it can map to an ecosystem

[mcp]                                      # standard shapes: a registry reference (pinned) or an inline .mcp.json entry
kubernetes = { registry = "mcp", name = "io.github.containers/kubernetes-mcp-server", version = "…", args = ["--read-only"] }
```

**Steps.** Every `observe`, `inventory`, `act` and `check` step has a `run` of one kind:

- `cmd` — argv with `{param}` placeholders;
- `mcp` + `tool` + `args` — a tool on a server the domain declares;
- `builtin` — Go code inside Warden, kept to what must be fast or safe.

The built-ins:

| builtin | step | what |
|---|---|---|
| `lockfiles` | inventory | go.mod / requirements.txt / package-lock.json |
| `homebrew` | inventory | Homebrew Cellar formulas and versions (no `brew` run) |
| `disk` | observe | free space |
| `caches` | observe | regenerable caches and their clean commands |
| `library-caches` | observe | `~/Library/Caches` by app (report only) |
| `trash` | observe | the Trash's size (report only: Warden never empties it) |
| `snapshots` | observe | local Time Machine snapshots |
| `health` | observe | battery, thermal |
| `go-bump`, `npm-bump`, `pip-bump` | act | the worktree bump playbooks (§8a) |
| `tool-clean` | act | a cache's own clean command |
| `propose` | act | write a proposal file and ask |
| `github-pause`, `github-issues`, `github-prs`, `github-main`, `github-experiment`, `github-roadmap` | pause / observe | one GitHub repository: the kill switch, issues, PRs and their CI, main's CI, release state and reverts, the experiment log's daily publication, the weekly roadmap and RFC timer |
| `work-issue`, `review-pr`, `merge-pr`, `release`, `delegate`, `pickup-branch`, `publish-log`, `revert-pr`, `roadmap`, `rfc-accept` | act | the maintainer playbooks (`docs/usecases/github-maintainer.md`) |

Advisories and news are not steps: they come from `[feeds]` (`osv = true` for every inventory component OSV knows,
`releases` for GitHub repos, `rss` for anything else).

**Mapping output (`map`).** A step prints either Finding JSON lines (the native shape: `{"key","kind","severity",
"subject","title","detail","fix":{"act","params"}}`) or anything else. For anything else, `map` turns JSON into
findings or components:

- `items` is a path with `[]` flattening;
- `where` filters items;
- the other keys are `{dotted.path}` templates;
- for plain text there is `lines` with a regex.

This covers kubectl, cloud CLIs, MCP tool results and HTTP APIs without new code. When neither fits, a step can say
`interpret = true`, and the brain turns the raw text into findings: it sees the text fenced as data.

**Floors** belong to each action and are enforced in Warden, never by the model:

| floor | effect |
|---|---|
| `reversible` | used by the policy |
| `always_ask` | the human always decides |
| `grant = "x"` | report-only unless the instance's `grants` list `x` |
| `max_per_day` | a daily limit |
| `dry_run_only` | never executes |
| `where = "worktree"` | runs in a fresh git worktree on a new branch, never in the human's checkout |
| `sandbox = true` | runs in the OS sandbox with an egress allowlist `net = [hosts]` |
| `untrusted = "deny"` | never runs for a finding that carries untrusted input (`data.tainted`) |
| `needs_brain` | refused when no brain may be called (budget spent); waits a tick when the routed brains are off-hours |
| `clean_env` | sandboxed commands get no secrets in their environment |

Global floors on top of these:

- dry-run mode;
- never delete user data: only an action's own command, never a generic `rm`;
- an installed extension never gets more than the floors of the action that calls it.

## 2. The instance: warden.toml

```toml
name     = "staging-ns"
domain   = "pack:k8s-namespace"             # a folder, or pack:NAME (./packs, next to the binary, $WARDEN_PACKS)
params   = { namespace = "shop", context = "kind-staging" }   # params.skip = "step,step" turns observers off
mode     = "act"                            # act | dry-run (unset: a domain's first run is dry-run)
wake     = { every = "15m", listen = true } # timer and/or signals
grants   = []                               # e.g. ["k8s.apply"]: what this human allows beyond the defaults
rules    = [{ act = "propose", then = "auto" }, { act = "apply", then = "ask" }]
brain    = { provider = "anthropic", model = "claude-opus-5-5" }   # or openai-compatible, or { acp = ["npx","-y","…"] }
sandbox  = "auto"                           # auto | sandbox-exec | bwrap | none (explicit: unsandboxed, shown in every report)
replies  = false                            # replies on the human's behalf stay dry-run until true
extensions   = ["skill:github.com/anthropics/skills//pdf@<sha>"]   # §5: pinned, hashed, in warden.lock
enable_hooks = []                           # extensions whose hooks may run
[[signals]] …                               # §4
[[hooks]] …                                 # §6
[serve]                                     # warden serve: POST /signal, A2A at /a2a, GET /report
tokens = { "…" = "ci" }                     # bearer token → sender; owners / trusted lists set the tier
```

## 3. The loop

```
wake (timer | signal) → observe + inventory → news/advisories for the inventory → triage (tell the human?)
  → decide (floors → rules → learned → budgets → dry-run) → act (sandbox / worktree) → check → report (+ hooks)
```

- **Triage** decides report / digest / ignore. Only new things ping; a quiet tick writes `latest.md` and nothing else.
- **Interrupt backoff** (`backoff.go`). A ping is a fired Report or Ask hook. Attention is any owner command
  (status, report, approve, decline, feedback, improve, rollback, signal), the same through `warden mcp`, or an
  owner-tier signal; it is recorded in `<state>/attention`, and the pings in `pings.json`. After `backoff.after`
  pings (default 2) without attention, the next ping waits the wake interval, then double that, up to `backoff.max`
  (default 168h). A held ping loses nothing: the report is written, the items stay new (not marked told), asks keep
  `told = false` and their Ask hook fires once the pause ends. A critical finding never waits.
- **Source scores** (`sources.go`). Every signal- or feed-born finding carries its `source`, and all of a tick's
  findings, signal-born ones included, go to `last.json` so `warden feedback` can label them. Per source: score =
  (important + ½·digest + 1) / (labels + 2). After 3 labels, ≤ 0.25 makes its items one step quieter (report → digest →
  ignore) and skips the brain for them (classification, incident RCA); ≥ 0.75 makes its digest lines reports.
  Vulnerabilities, low disk, checks, critical items, asks and done work are never touched. News items are each new,
  so a triage rule from one label rarely fits the next; the score generalizes across them.
- **Decide** is Warden's own policy engine:
  - the floors of the action, then the human's rules, then learning (K consistent approvals of the same action+target
    → auto; one decline pins it to ask), then velocity and daily budgets, then dry-run;
  - outcomes are auto / ask / deny;
  - "ask" puts the item under "waits for you" with `warden approve ID` / `warden decline ID`, also reachable as an MCP
    tool, an A2A message, or a hook that posts it wherever the human lives.
- **Self-update** (`warden update` from a signal) is not in v1: rebuild manually.
- **Durability.** State is files on disk:
  - `journal.jsonl` holds every step's begin/end;
  - findings seen, pending, done, and the policy state are written atomically;
  - a lock allows one tick at a time.

  A crash mid-action leaves an unfinished begin; the next tick cleans up (drops the worktree) and retries if still
  needed. Each action has an idempotency key, and a failure is retried at most daily. The OS keeps it alive: launchd /
  systemd units for timer mode (nothing runs between ticks) or listen mode (one process asleep on its streams).

**Pause, brains, budget, digest.** A domain's `[[pause]]` steps run first every tick; any finding (or a failing pause
step) makes that tick observe and report only. `[brains.NAME]` plus `brain_for` route judgement calls per task
(`auto` climbs `brain_auto`, free first); a brain may declare `unavailable` windows, and one whose upstream answers 403
is routed around for a while. `[budget]` keeps a daily ledger (`internal/meter`): paid calls are metered, over the cap
none is made, alerts ping at once, and the report header says `model spend today: $X of $Y`. `digest = "daily"`
fires a `Digest` hook once a day.

## 4. Signals

Anything from outside — a tweet, a news item, a systemd or Kubernetes error, a message from the group leader, another
agent's request — is a **signal**. Sources:

| source | shape |
|---|---|
| `feed` | an RSS/Atom URL; the first look is a baseline |
| `webhook` | `warden serve` takes HTTP POST `/signal` with a bearer token per sender |
| `a2a` | `warden serve` speaks A2A (`message/send`); the sender is the authenticated agent |
| `stream` | a long-running command, one line = one signal: `journalctl -f`, `kubectl get events -w`, `tail -F` |
| `command` | a CLI printing `{"id","from","group","text","url","verified"}` lines: the adapter for any chat or network |
| `mcp` | a tool on an MCP server polled for new messages (not in v1: wrap it as a `command` source) |
| `dir` | a drop folder (`warden signal TEXT` writes to the built-in one) |

What a signal may cause:

1. **What it asks** comes from a `warden VERB …` line, else checkable hints (advisory IDs; module@version near
   "update"; a component of the inventory near "update/vulnerable"; an error line → incident), else the brain picks a
   verb. The brain sees the text fenced as data and can only pick a verb.
2. **Who asks** is the tier. The source sets a default trust; senders the source verified are raised to owner or
   trusted by the instance's lists.
3. **What it may cause.** The tier caps the verb: `update` and `task` from a trusted sender become an ask to the
   owner; strangers get a digest line and no reply. Urgency raises severity, never permissions. Claims are verified:
   an advisory is looked up and matched against the real inventory before anything happens.

## 5. Extensions from existing registries

Warden does not have its own package format. A domain pack is a folder in the shape harnesses already use (`skills/`,
`.mcp.json`, plus `domain.toml`), and it can reference what others publish:

| registry | what Warden takes | how it is pinned |
|---|---|---|
| MCP Registry (`registry.modelcontextprotocol.io`) | MCP servers (npm / PyPI / OCI packages, remotes) | version + npm `dist.integrity` / PyPI sha256 / OCI digest |
| Agent Skills catalogs, Claude Code plugin marketplaces (`.claude-plugin/marketplace.json`) | `SKILL.md` folders; a plugin's `.mcp.json` and `hooks/hooks.json` | git commit SHA + tree hash |
| Pi packages (npm) | skills and prompts; extensions only through pi itself as an ACP agent | npm version + integrity |
| Gemini CLI extensions | context files, MCP servers | git commit SHA |

`warden add mcp:io.github.containers/kubernetes-mcp-server` (or `skill:…`, `pi:…`, `plugin:…@marketplace`) resolves,
downloads, verifies and writes `warden.lock` (source, version, integrity). Startup refuses an extension whose bytes
don't match the lock. Installed extensions get no more rights than the action that calls them:

- MCP servers run in the sandbox with their declared hosts only;
- a server's tools are callable only from steps and brain tool lists that name them;
- hooks from plugins are off until enabled by name.

(Registry formats in detail: §10 and `research/registries.md`.)

## 6. Hooks (Claude Code shape)

```toml
[[hooks]]
event   = "Report"          # Wake | Finding | PreAction | PostAction | Ask | Report | Signal
matcher = "report|ask"      # regex on the finding kind / action name / disposition
command = ["my-tracker", "issue", "create", "--title", "{title}"]   # any CLI: a team chat, a tracker, mail
```

- The event JSON goes on stdin.
- `PreAction` may block: exit code 2, or `{"decision":"block","reason":"…"}`.
- `Report` and `Ask` hooks are how Warden reaches Telegram, a team chat, an issue tracker, e-mail or a chain network; a plugin may bring them.

Hooks from extensions are disabled until enabled by name.

## 7. The brain

The brain is optional and is used only for judgement:

- a fix after red tests;
- an owner's task;
- an incident RCA;
- classifying an unclear signal;
- `interpret` steps;
- proposing triage changes.

Backends:

- `acp` — any ACP agent (Claude Code, Gemini CLI, Codex, pi). The process is started per call and runs like an act
  (`core/acpbrain.go`):
  - in the sandbox, writing only its own folder (`<state>/agents/acp`) and the worktree it was given
    (`brain.WithDir`, which is also the session's cwd); egress only to `brain.net` (Claude: the Anthropic API and
    login hosts and the npm registry for npx);
  - `Confine`: every permission request and fs/* call must name paths inside that worktree (not `.git`), and then
    only `read` and `edit` are allowed; execute, fetch, delete and move are refused; with no worktree (RCA,
    classification) nothing is;
  - `isolate = "claude"` (default when the argv names claude): a clean environment, `CLAUDE_CONFIG_DIR` of its own,
    `settingSources = []` and `strictMcpConfig` in session/new, no claude.ai connectors, CLAUDE.md or auto memory.
    The login is the owner's, by reference (an empty `CLAUDE_SECURESTORAGE_CONFIG_DIR` and read-only Keychain access
    in the sandbox) or `CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY` from the environment. An expired login fails
    with a one-line remedy in the report;
  - Warden's tools are not forwarded (the task says so); Warden runs the checks after the agent;
  - an incident RCA gets a throwaway detached worktree of HEAD (`readTree`) to read the code; a task and a bump fix
    get their branch's worktree;
  - honest limit: the agent's own read tools don't ask, so reads are bounded by the sandbox (secrets closed), not by
    the worktree; writes are bounded by both.
- `anthropic` / `openai` — any compatible endpoint (Claude; GLM via Z.ai with `auth = "bearer"`; OpenRouter; local
  Ollama/vLLM), driven by Warden's own tool loop.

  The built-in tools are `list_files`, `read_file`, `write_file`, `replace_in_file` and `run_checks`, all confined to
  the worktree. Added to them:
  - the MCP tools the step allows;
  - the domain's skills (names and descriptions in the system prompt, `read_skill` to load one: progressive
    disclosure).

  There is a step budget and the conversation is append-only.

## 8. Self-improvement (after Harness^2, with a hard gate)

- **What may change:** only the triage skill — when to tell the human.
- **Gate.** A candidate, from the owner's labels (`warden feedback ID noise|digest|important`) or from the brain, must
  pass every compiled-in invariant (a high vulnerability, a failing check or low disk can't be hidden) and be strictly
  better on the owner's labels.
- **History.** Adopted versions are kept with an `EXPERIENCE.md` entry; `warden rollback` restores the previous one.
- **What can't change:** floors, grants, policy rules and the evaluator are outside its reach.

## 8a. Bump playbooks

One playbook (`builtin/bump.go`) for every ecosystem: a worktree on `warden/bump-PKG-VERSION`, the update in the
sandbox with egress only to the registry, a build where there is one, the checks without network, the brain on red
(build or checks), a lock-file verify (no copy below the fixed version), an OSV re-query, one commit and a PR
description. Never pushed or merged.

| act | update | build | registry |
|---|---|---|---|
| `go-bump` | `go get M@V`, `go mod tidy` | `go test -run=^$ ./...` (packages and tests compile) | proxy/sum.golang.org |
| `npm-bump` | direct: `npm install P@V`; transitive: `npm pkg set overrides[P]=V`, `npm install`; always `--ignore-scripts`, Warden's cache | — | registry.npmjs.org |
| `pip-bump` | the `==` pin edited in requirements.txt (extras and markers kept) | a fresh `.warden-venv` with the set installed (removed before the commit) | pypi.org, files.pythonhosted.org |

A fix version younger than `min_age_days` is always asked; its publish time comes from the Go proxy, the npm
packument or the PyPI release files.

## 9. Footprint and safety

- One Go binary.
- **Between ticks:** nothing runs (timer mode), or one process asleep on reads (listen mode).
- **Sandbox:** `sandbox-exec` (macOS) or `bwrap` (Linux), deny-by-default writes, and egress through Warden's own
  allowlist proxy. It **fails closed**: with no runner (and no explicit `sandbox = "none"`) Warden keeps observing and
  reporting but refuses actions, sandboxed checks, stdio MCP servers, the ACP brain, hooks and replies, each with a
  report line. `sandbox = "none"` is allowed and shows as `sandbox: NONE (owner's choice)` in every report header.
  `tool-clean` runs a cache tool's own clean command outside the sandbox by design (a write-only-here box would break
  `go clean -cache`); it is gated by the grant, the owner's allowlist and an exact argv match, and refused when no
  sandbox is available.
- **The human's machine and accounts:**
  - dry-run first;
  - notifications and replies on the human's behalf are dry-run until enabled;
  - Warden never touches the human's agent configs;
  - it never pushes or merges (the `github-repo` maintainer mode pushes its own `warden/` branches and merges under
    its rules);
  - it never deletes user data.

## 10. Registry formats (facts behind the adapters; details and verification marks in `research/registries.md`)

### MCP Registry

- Endpoints: `GET /v0/servers?search=…&version=latest` and `GET /v0/servers/{name}/versions/{ver|latest}`.
- A ServerDetail has `packages[]`; each package has `registryType` npm|pypi|oci|mcpb, `identifier`, `version`,
  `runtimeHint`, `transport`, `packageArguments`, `environmentVariables`. Remote servers are listed in `remotes[]`.
- Where the integrity comes from:

| package | integrity |
|---|---|
| npm | `dist.integrity` (sha512 SRI) |
| PyPI | `digests.sha256` |
| OCI | the registry's `docker-content-digest`, pinned as `image@sha256:…` |
| mcpb | `fileSha256` |

- Caveat: npm packages with per-platform optional binaries, such as `kubernetes-mcp-server`, need the platform
  package pinned too.

### Agent Skills

- A folder with `SKILL.md`; its frontmatter has `name` and `description`.
- Catalogs are git repos, for example `anthropics/skills`. The pin is a commit SHA plus a hash of the folder tree.

### Claude Code plugin marketplaces

- `.claude-plugin/marketplace.json` lists `plugins[]` with a `source`: a relative path, `github`/`url`/`git-subdir`
  with `ref`/`sha`, `npm`, or `archive` with `sha256`.
- A plugin is `.claude-plugin/plugin.json` plus `skills/`, `hooks/hooks.json` and `.mcp.json`.
- oh-my-pi reads the same marketplaces: `.omp-plugin/` first, then `.claude-plugin/`.
- Warden takes skills, MCP servers and hooks from a plugin. Plugin hooks are off until enabled by name.

### Pi packages

- Pi packages live on npm under the keyword `pi-package` (≈12.9k packages, gallery pi.dev/packages) or in git
  (`git:host/owner/repo@ref`).
- `package.json` `"pi"` (`"omp"` for oh-my-pi) lists `extensions`, `skills`, `prompts`, `themes`; without it, the
  conventional folders are used.
- Warden uses skills and prompts. Extensions are TypeScript modules running in-process with full rights inside pi:
  Warden never runs them itself. Use pi as an ACP brain if they are wanted, and they then run under pi's own trust
  model.

### Gemini CLI extensions

- `gemini-extension.json` declares `mcpServers` and context files; it is installed from a git URL.
- Warden takes the MCP servers and the context files, the latter as skills.

### A2A 1.0

- Agent card at `/.well-known/agent-card.json`; endpoints under `supportedInterfaces[]`.
- JSON-RPC methods `SendMessage` and `GetTask`. The 0.3 names `message/send` and `tasks/get` are accepted as
  aliases.

### Kubernetes MCP server

- The pack references `io.github.containers/kubernetes-mcp-server`, pinned.
- Warden writes a config file with `read_only = true` and passes `--config`. This works on both the released flags
  (`--read-only`) and `main`, where flags are moving to TOML. In read-only mode only tools with `readOnlyHint` are
  listed.

## Sources

- **Harness^2:** Google Cloud AI Research, arXiv:2610.14224 (propose → probe → compose; "keep current" is a valid
  outcome; EXPERIENCE.md).
- **Where self-improvement fails:** Darwin Gödel Machine (arXiv:2505.22954) gamed its evaluator, so here the
  evaluator is outside the agent's reach.
- **Ambient agents:** LangChain (notify / question / review): https://blog.langchain.com/introducing-ambient-agents
- **Leaving the loop:** IndyDevDan, "ELITE Agentic Engineers Are LEAVING THE LOOP" (2026-10-26):
  https://www.youtube.com/watch?v=52JX83rGPLc. Prompt → Click → Cron; proactive triggers; durable agents in
  sandboxes; on-call agent prepares the fix and waits for one click.
- **Autonomy levels:** L4 Approver / L5 Observer (arXiv:2506.12469); selective, trigger-based human involvement
  (arXiv:2604.23049); Anthropic, Measuring agent autonomy: https://www.anthropic.com/research/measuring-agent-autonomy
- **Durable patterns:** Cloudflare Agents schedules, LangGraph checkpoints, Temporal/DBOS durable execution; OpenClaw
  heartbeat → automation jobs: https://docs.openclaw.ai/gateway/heartbeat
- **Feeds:** OSV querybatch https://google.github.io/osv.dev/post-v1-querybatch/ ; GitHub releases.atom; Go proxy
  `.info`.
