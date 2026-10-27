# Contributing to Warden

Thanks for helping. Warden aims to stay small: one binary, few dependencies, safety enforced in code. Changes that
keep it that way are the easiest to merge. For anything larger than a fix, please open an issue first.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues go to
[SECURITY.md](SECURITY.md), not to public issues.

## Development setup

- Go 1.26 or newer (`go.mod` is the source of truth), git.
- macOS: `sandbox-exec` is built in. Linux: install `bubblewrap`; on Ubuntu 24.04 also see the AppArmor note in
  [SECURITY.md](SECURITY.md#the-sandbox-and-its-limits), otherwise the sandbox tests skip and acts are refused.
- [golangci-lint](https://golangci-lint.run) v2 for linting.

```bash
git clone https://github.com/nenkoru/warden && cd warden
go build ./...
go build -o warden ./cmd/warden
```

## Tests

```bash
go vet ./...
go test -race -count=1 ./...          # offline: fakes and httptest only
golangci-lint run ./...                # config in .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

Tests must not need the network, a model, a cluster or credentials. Use `httptest`, fake commands in `t.TempDir()`,
and skip with a reason when a platform tool (git, `sandbox-exec`, `bwrap`) is missing.

The demos are end-to-end checks without a model; CI runs them:

```bash
scripts/demo-k8s.sh          # fixture only: a fake kubectl, no cluster
scripts/demo-go-project.sh   # real OSV and Go proxy (needs network)
scripts/demo-macos.sh        # macOS only, dry-run
scripts/demo-npm-pypi.sh     # real OSV, npm and PyPI (needs node and python3)
```

`scripts/live-brain.sh` spends model tokens; run it only on purpose. If a change alters report output, re-run the
relevant demo and update `examples/` (replace temp paths with `$DEMO`).

## Layout

| path | what |
|---|---|
| `cmd/warden` | the CLI |
| `internal/core` | the loop: tick, triage, policy glue, signals, reports, listen, serve, units |
| `internal/policy` | floors, rules, learned trust, budgets |
| `internal/box` | the sandbox (`sandbox-exec`, `bwrap`) and the egress proxy |
| `internal/builtin` | built-in steps: lockfiles, OSV, bumps, machine observers, `propose`, `tool-clean` |
| `internal/domain` | the `domain.toml` manifest and `map` |
| `internal/registry` | extensions from registries and `warden.lock` |
| `internal/brain` | ACP and Anthropic/OpenAI-compatible brains |
| `internal/mcpx`, `internal/a2a`, `internal/hooks`, `internal/skills` | the standard integrations |
| `packs/` | shipped domain packs |
| `docs/internals.md` | package APIs |

Rules of thumb: no package imports `internal/core`; only the standard library beyond the three dependencies; errors
wrapped with context; anything that changes the world goes through policy floors and the sandbox.

## Adding a pack

1. Create `packs/NAME/domain.toml` (and optional `skills/`, `.mcp.json`). Start read-only: observers plus `propose`;
   any act that changes things needs floors (`grant`, `always_ask`, `max_per_day`, `sandbox` + `net`) and a `check`.
2. Write a header comment saying what the pack watches and what it may change.
3. Add a test that loads it (see `internal/domain/domain_test.go` and the pack tests in `internal/core`), and, if it can
   run without external systems, a fixture demo in `scripts/` with its output in `examples/`.
4. Add a page under `docs/usecases/` and a row in the README's packs table.

See [docs/usecases/writing-a-pack.md](docs/usecases/writing-a-pack.md) for the manifest.

## Adding a builtin

Builtins are for what must be fast, safe or impossible as a command. Observers and inventory live in
`internal/builtin/machine.go` / `inventory.go` (dispatch in `Observe` / `Inventory`); acts in `act.go` (`Act`). A new
builtin needs: a table-driven test with fakes, no network in tests, every external command run through the step's
sandbox policy, and an entry in the builtins table in DESIGN.md.

## Adding a registry adapter

Adapters live in `internal/registry` (`Parse` → `Resolve` → `Verify`). An adapter must pin by integrity (a version
plus a hash the registry publishes, or a git commit plus a tree hash), refuse on mismatch, never run install scripts,
and write everything it needs into `warden.lock`. Extensions never get more rights than the act that calls them. Tests
use `httptest` servers that serve both good and tampered bytes. Document the format facts in `research/registries.md`.

## Commits and pull requests

- Small, focused commits; imperative subject line under ~72 characters, e.g. `box: refuse egress to IP literals`,
  using the package or area as the prefix. Explain the why in the body.
- Keep `go vet`, the tests, golangci-lint and govulncheck green; CI runs them on Linux and macOS.
- Update docs, `CHANGELOG.md` (under `[Unreleased]`) and `examples/` when behavior or output changes.
- DCO sign-off is welcome but optional: `git commit -s` adds `Signed-off-by:` (the
  [Developer Certificate of Origin](https://developercertificate.org/)).
- By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).
