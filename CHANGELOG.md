# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `foreach` on observe and inventory steps: run a step once per value of a list param
  (`foreach = { namespace = "{namespaces}" }`); findings carry the bound value, so an act on one runs where it was
  seen. The `k8s-namespace` pack uses it to watch a project's several namespaces (`namespaces`) with only a
  namespaced `view` in each, and now also reports failed Jobs (keyed by their CronJob), and inventories init
  containers' and CronJob templates' images and Helm charts (from the `helm.sh/chart` label).

- The `github-repo` pack: Warden maintains one GitHub repository — issues worked into draft PRs (`work-issue`, CI
  decides, one fix round from the failed jobs' logs), outside PRs reviewed with one `COMMENT` per head commit
  (`review-pr`, never merged, their code never run), its own, Dependabot's and `trusted_authors`' PRs squash-merged on
  green CI (`merge-pr`), patch/minor releases with a CHANGELOG generated from merged PR titles and a tag on the release
  PR's merge commit (`release`), hand-offs to other agents through a templated command (`delegate`,
  `pickup-branch`). Observers `github-issues`, `github-prs`, `github-main`, `github-pause`; a GitHub REST client with
  the token from a file (never in argv, URLs, logs or errors); `warden github-note`, a hook command that comments on a
  pinned "Warden log" issue. See `docs/usecases/github-maintainer.md` and `deploy/k8s/github-maintainer.yaml`.
- A kill switch for any domain: `[[pause]]` steps run first every tick; a finding (or a pause step that fails) makes
  the tick observe and report only, every act refused.
- Several brains, routed per task: `[brains.NAME]`, `brain_for = { task = "name" | "auto" }`, `brain_auto`; per-brain
  `unavailable` windows (times and weekdays, `tz`), `api_key_file` (read at call time), `free`; an upstream that
  answers 403 is routed around for a while; every report counts each brain's calls and tokens. `[brain]` alone still
  means one brain for everything.
- `[budget]`: a daily model-spend ledger (`per_day_usd`, `alert_every_usd`, `burn_per_hour_usd`, `prices`,
  `free_models`, `upstream_proxy`). Endpoint brains are metered per call (LiteLLM's `x-litellm-response-cost` wins over
  the price table), a Claude Code ACP brain through the meter's local proxy; over the cap no paid model is called and
  brain acts are refused; alerts ping at once; every report header says `model spend today: $X of $Y`.
- `digest = "daily"`: a `Digest` hook event once a day with the day's summary.
- Floors `untrusted = "deny"` (a finding carrying untrusted input never drives the act), `needs_brain`, `clean_env`;
  the sandbox can open loopback ports (a local metering proxy) and hide credential files.
- Instance params accept arrays and tables (passed to packs as JSON).
- `pickup-branch` titles the PR after the branch's commits (one commit: its subject and body; several: the oldest
  subject and a list of all), so releases count it right; the generic title stays the fallback.

### Security

- Build with Go 1.26.10 (`toolchain go1.26.10`), which fixes GO-2026-6809 (crypto/x509: distrust compromised
  intermediate certificates). This is a deliberate exception to the project's "dependencies at least two weeks old"
  rule, because it is a security fix in a Go patch release.

## [0.1.0] - 2026-10-27

The first public version.

### Added

- A single Go binary bound to one domain described by a manifest (`domain.toml`): observe, inventory, act, check,
  feeds, MCP servers and skills. Steps are commands, MCP tools or built-ins; output becomes findings through `map` or
  the brain (`interpret = true`).
- Packs: `go-project`, `node-project`, `python-project`, `k8s-namespace`, `macos`, `linux-server`, `linux-host`.
- The loop: timer ticks (launchd/systemd units via `warden units`) or `warden listen` on streams; journaled, atomic
  state, one tick at a time, idempotency keys, cleanup after a crash.
- A floors-first policy engine: deny rules → floors (`reversible`, `always_ask`, `grant`, `max_per_day`,
  `dry_run_only`, `where = "worktree"`, `sandbox` + `net`) → rules and pins → learned trust → budgets → dry-run.
- Bump playbooks for Go, npm and PyPI in a new worktree branch, sandboxed with registry-only egress, checks without
  network, the brain on a red build, an OSV re-check, one commit and a PR description. Never pushed or merged.
- Human near the loop: `warden approve` / `decline`, asks with prepared work, report-only without a grant, quiet ticks,
  interrupt backoff when pings go unanswered.
- Self-improving triage behind a gate with compiled-in invariants; `warden feedback`, `warden improve`,
  `warden rollback`; source scores from labels.
- Signals: RSS/Atom, commands printing JSON lines, drop folders, streams, webhooks and A2A through `warden serve`,
  `warden signal`; trust tiers and claim verification.
- Integrations: MCP client and server (`warden mcp`), ACP and Anthropic/OpenAI-compatible brains (Claude Code
  isolated and sandboxed), Agent Skills, Claude Code-shaped hooks, A2A 1.0.
- Extensions from the MCP Registry, Agent Skills catalogs, Claude Code plugin marketplaces and Pi packages, pinned with
  integrity in `warden.lock` (`warden add`, `warden verify`).
- Every outgoing HTTP request (OSV, the Go proxy, GitHub, feeds, registries, the A2A client, model endpoints)
  carries `User-Agent: warden/<version> (+https://github.com/nenkoru/warden)`; `warden version` prints the version
  (set at build time with `-ldflags -X github.com/nenkoru/warden/internal/version.Version=…`).
- A feed that fails (HTTP status, parse error, timeout) shows in the report as
  `feed <url> broken: <error> — will retry next tick`, once per distinct error, and once more when it works again.
- A fail-closed sandbox: `sandbox-exec` on macOS, `bwrap` on Linux, with an egress allowlist proxy.
- Demos without a model (`scripts/demo-*.sh`, output in `examples/`), a container image script and a Kubernetes
  example.

[Unreleased]: https://github.com/nenkoru/warden/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/nenkoru/warden/releases/tag/v0.1.0
