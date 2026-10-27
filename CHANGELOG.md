# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
- Self-update (`[update]`, `internal/update`): Warden checks its GitHub releases inside the tick (at most every
  `every`), verifies the archive — its line in `checksums.txt`, then the release workflow's GitHub build-provenance
  attestation, a Sigstore bundle verified with sigstore-go against the public-good trusted root (signer
  `…/.github/workflows/release.yml@refs/tags/<tag>`, GitHub Actions' issuer, SLSA provenance v1 naming the digest),
  then `cosign` when present — and installs it at the end of a healthy tick (`<bin>.new` preflighted, `<bin>.prev`
  kept, atomic rename). A new binary that never ends a tick healthy is rolled back with a high finding that always
  pings. Channels `stable` (waits `canary_hours`, skips versions named by open `warden-rollback` issues) and `canary`;
  `mode = "propose"`; on Kubernetes the image bump is proposed, never the binary touched. Optional fleet beacon (one
  comment per instance on a pinned "Warden fleet" issue; `update.FleetStatus`). `warden update [--check]`; the
  report's "Under the hood" names the version and the update state. A `listen`/`serve` process exits after a tick that
  replaced the binary so its supervisor starts the new one. See `docs/usecases/self-update.md`.
- GitHub client: anonymous (read-only) clients, releases, release assets, attestations, and editing the token user's
  own issue comments (`PATCH …/issues/comments/N`, allowed by the guard; the author is checked first).
- The canary gate for `release` (`params.canary`, `canary_hours` default 24, `canary_min_ticks` default 6): a merged
  release PR is tagged `vX.Y.Z-rc.N` first (a prerelease only canary-channel Wardens install); once the named
  instance's fleet beacon (comments by the token's own login or the owner only) reports the rc healthy for
  `canary_hours` and `canary_min_ticks` ticks, not rolled back, with `main` green, `vX.Y.Z` is tagged on the same
  commit. A rollback of the rc on the canary, or an open `warden-rollback` issue naming it, stops it: a `rollback`
  finding that pings, a `needs-human` issue "release vX.Y.Z-rc.N failed on the canary" (once), and the next attempt
  waits for newly merged PRs (a new release PR folds the unshipped CHANGELOG section back in; a new rc number on its
  merge). rc tags are never "the latest release"; one in flight holds the next release PR. A beacon whose comment
  was not rewritten for `canary_stale_hours` (default 3; its `updated_at`) is a silent canary: "canary silent since
  …", no final tag. The report says
  "release vX.Y.Z: rc.N on the canary «name» since …, healthy N h of 24". Without `canary` nothing changes.
- The release workflow builds the pushed tag (`GORELEASER_CURRENT_TAG`), and goreleaser sorts `-rc.N` before its
  release (`git.prerelease_suffix`): an rc and its release share a commit.

### Changed

- Releases are published, not drafts (`.goreleaser.yaml` `release.draft: false`; prereleases stay marked by
  `prerelease: auto`): self-update cannot see drafts, and the canary gate replaces the owner's review of a draft.
- The GitHub guard lets `POST …/git/refs` create `refs/tags/vX.Y.Z-rc.N` as well as `refs/tags/vX.Y.Z` (nothing else).

### Security

- New dependency `github.com/sigstore/sigstore-go` v1.3.0 (release verification); `golang.org/x/net` v0.62.0 and
  `google.golang.org/grpc` v1.85.0 in its graph, past GO-2026-6603/6611/6612/6617 and GO-2026-6348. The binary grows
  from about 12 MB to 23 MB.
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
