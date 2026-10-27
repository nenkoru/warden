# Security policy

Warden runs unattended next to your code, machines and clusters, so security reports are welcome and taken seriously.

## Reporting a vulnerability

Please report privately through **GitHub Security Advisories**: on the repository page open
**Security → Report a vulnerability**. Do not open a public issue, pull request or discussion for a vulnerability.

Include what you can: the version or commit, the platform (macOS / Linux distribution, sandbox runner), the
configuration involved (`warden.toml`, the pack), steps to reproduce, and the impact you see. You can expect an
acknowledgement within a week. Fixes are released as a patch version with an advisory that credits you, unless you
prefer otherwise.

## Supported versions

Warden is young; only the latest release line gets security fixes.

| version | supported |
|---|---|
| 0.1.x | yes |
| < 0.1 | no |

## Threat model

Warden's job is to act on its own inside hard limits. The limits are enforced by Warden's code and the operating
system, never by a model's good behavior.

### Untrusted input: signals and model output

- **Signals** (feeds, chat adapters, log streams, webhooks, A2A messages, drop folders) are data. What a signal can
  cause is capped by its sender's trust tier: `update` and `task` from a trusted sender become a question to the owner;
  a stranger gets at most a digest line and no reply. Urgency raises severity, never permissions. Claims are verified:
  an advisory is looked up in OSV and matched against the real inventory.
- **Model output** is untrusted too. The brain sees signal text fenced as data and can only pick a verb from a fixed
  set. Its edits are confined to a worktree on a new branch, and Warden runs the checks itself afterwards. The policy
  (floors, grants, rules, budgets) is evaluated in code after the brain, so a prompt injection can at worst propose
  something that still has to pass the floors — and asks of the owner stay asks.
- **Self-improvement** may change only the triage (when to tell the human), only through a gate with compiled-in
  invariants (a high vulnerability, low disk or a failing check can never be hidden). Floors, grants, policy rules and
  the evaluator are outside its reach.

### Maintainer mode (the `github-repo` pack)

In this mode Warden pushes branches and merges pull requests in one repository, so the threat model widens: anyone
who can open an issue or a PR can put text in front of the model. The limits, all in code:

- Issue and PR titles, bodies, comments, CI logs and outside code are data: fenced as untrusted wherever they reach a
  model, and marked tainted. The floor `untrusted = "deny"` keeps tainted findings away from merges, releases and
  branch pick-ups; outside PRs are reviewed from their diff and CI results only — their code is never checked out or
  run, and the review never approves.
- A change from an issue that touches protected paths (`.github/**`, release config, `SECURITY.md`, `LICENSE`,
  `NOTICE`, the pack, `warden.toml`, anything named like a secret, token or signing file) is never pushed unless the
  owner wrote the issue, and is then labeled `needs-human`; Warden never merges a PR touching them. A diff containing
  the token is refused before any push.
- The GitHub token is a fine-grained PAT for this one repository without the workflows or administration
  permissions, read from a file (refused if readable by others), sent only in the API's `Authorization` header and to
  git through a credential helper that reads the file — never in argv, URLs, the environment, logs, reports, the
  journal, comments or errors (redacted everywhere as a second line), and unreadable inside the sandbox.
- Merges require every reported check green, a mergeable PR, no `needs-human` label and no protected path, and are
  pinned to the head SHA; branch protection on `main` (required PR and checks, no force pushes) is the outer gate.
  Releases only from green `main`, at most one a day, never a major version, never while paused.
- The owner's kill switch (a `warden-pause` label or a `PAUSE` file on `main`) is read first every tick and again right
  before a merge or a tag; if it can't be read, the tick is paused.
- Model spend is capped per day in code, with a spend limit on the API workspace as the outer backstop.
- Residual risk: a prompt-injected brain can still write a wrong but plausible change to unprotected code; it lands
  only through CI and the merge rules above, and every merge is a squash commit naming the issue and the brain.

### Extensions from registries

- Extensions (MCP servers, Agent Skills, plugins, Pi packages) are pinned in `warden.lock` by version plus integrity
  (npm `dist.integrity`, PyPI sha256, OCI digest, git commit plus tree hash). `warden verify` re-checks the bytes;
  startup refuses an extension whose bytes don't match, and an MCP server that is not in the lock is not started.
- Installed extensions get no more rights than the act that calls them: stdio MCP servers run in the sandbox with their
  declared hosts only, a server's tools are callable only from steps and tool lists that name them, plugin hooks are off
  until enabled by name, and Pi extensions (in-process TypeScript) are never run by Warden.
- Known gaps: npm per-platform optional dependencies are noted in the lock but not pinned; `uvx` resolves transitive
  dependencies at run time.

### The sandbox and its limits

- Acts, sandboxed checks, stdio MCP servers, the ACP brain, hooks and replies run under `sandbox-exec` (macOS) or
  `bwrap` (Linux): writes only to the working folder (and the worktree), reads everywhere except secrets directories
  (`~/.ssh`, `~/.aws`, `~/.gnupg`, keychains), egress only through Warden's own allowlist proxy (exact hosts or
  `*.suffix`), refusals recorded.
- **It fails closed.** Without a working runner Warden keeps observing and reporting but refuses everything above, with a
  report line. `sandbox = "none"` is an explicit owner choice and is printed in every report header as
  `sandbox: NONE (owner's choice)`. A named runner that is missing is an error, never a silent fallback.
- **Outside the sandbox by design:** `tool-clean` runs a cache tool's own clean command (`go clean -cache`,
  `npm cache clean --force`, …) unsandboxed, because a "write only here" box would break it. It is gated by the
  `mac.clean` grant, the owner's allowlist (`params.clean`) and an exact argv match, and refused when no sandbox is
  available at all. Observe and inventory steps without a `sandbox` floor run unsandboxed as the warden's user; packs
  should keep them read-only.
- **ACP brains** read with their own tools without asking, so their reads are bounded by the sandbox (secrets closed),
  not by the worktree; their writes are bounded by both. Claude Code runs with its own config directory and the
  owner's login only by reference (Keychain read-only), or a token from the environment.
- **Ubuntu 24.04:** AppArmor blocks unprivileged user namespaces, so `bwrap` cannot start and Warden reports "no
  sandbox". The narrow fix is a profile for bwrap alone (needs sudo; your decision):
  `/etc/apparmor.d/bwrap` containing
  `abi <abi/4.0>, include <tunables/global> profile bwrap /usr/bin/bwrap flags=(unconfined) { userns, }`, then
  `sudo apparmor_parser -r /etc/apparmor.d/bwrap`.
- In containers, `bwrap` usually cannot create namespaces; the Kubernetes example sets `sandbox = "none"` and relies on
  the pod (view-only ServiceAccount, read-only root filesystem, no capabilities) as the boundary.

### What Warden never does

- push, merge, or touch your checkout (changes land in a new worktree branch) — except the `github-repo` pack's
  maintainer mode above, which pushes only its own `warden/` branches and merges only under those rules;
- delete user data (only a tool's own clean command, never a generic `rm`; it never empties the Trash);
- modify your agent harness configs, settings, MCP servers or memory;
- send messages or replies on your behalf unless `replies = true` (hooks you configure are your commands);
- act in a new domain before you set `mode = "act"` (new domains start in dry-run);
- run unsandboxed unless you set `sandbox = "none"`.

### Out of scope

- Anything a configured hook, `cmd` step or `sandbox = "none"` does with the owner's own rights: those are the owner's
  commands.
- Compromise of the host account Warden runs as, or of the model provider.
- Denial of service by a trusted sender flooding a stream (streams are rate-limited to 120 lines a minute per source,
  the rest counted and reported).
