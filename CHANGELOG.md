# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
