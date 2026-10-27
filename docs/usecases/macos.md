# A Mac

**Pack:** `packs/macos` · **Demo:** `scripts/demo-macos.sh` → `examples/demo-macos.txt`

Warden watches a Mac: free disk space, regenerable caches (Go, npm, uv, pip, pnpm, yarn, simulators), what apps keep
in `~/Library/Caches`, the Trash, battery condition and thermal throttling, and the installed Homebrew formulas as an
inventory that news and advisories are matched against. Everything is read-only by default and the pack starts in
dry-run.

Cleaning is deliberately narrow: only a cache tool's **own** clean command (`go clean -cache`,
`npm cache clean --force`, `uv cache prune`…), only for cleaners you list in `params.clean`, only with the `mac.clean`
grant, at most 6 a day. Warden never runs `rm` on your files and never empties the Trash.

## Configuration

```toml
# ~/.warden/mac/warden.toml — written by `warden init --pack macos --name mac`
name    = "mac"
domain  = "pack:macos"
params  = { min_free_gb = "25", clean = "go-build,npm,uv" }   # clean: the cleaners you allow
mode    = "act"
wake    = { every = "6h", listen = false }
grants  = ["mac.clean"]                                       # without it: report only
rules   = [{ act = "tool-clean", then = "ask" }]             # or "auto" once you trust it
# params.skip = "caches,library-caches" turns slow observers off (they use du)
```

`tool-clean` runs **outside** the sandbox on purpose: a "write only here" box would break `go clean -cache`. It is
gated by the grant, your allowlist and an exact argv match, and it is refused when no sandbox is available at all.

Run it as a launchd user agent: see [INSTALL.md](../../INSTALL.md).

## What the report looks like

From `examples/demo-macos.txt` (a real Mac, dry-run, no grant; app names replaced):

```markdown
Domain: **macos** (`min_free_gb=25`) · mode: **dry-run (changing nothing)** · triage v1 · sandbox: sandbox-exec

**Worth your attention: 1** (done: 0, waiting for you: 0, important: 1).

## Important

- **Low disk space: 16.5 GB free** — medium · `d7b7f2`
  - 16.5 GB free of 460.4 GB (96% used); threshold 25 GB. Regenerable caches the tools can clean: 14.4 GB. Biggest: Go build cache 8.2 GB, Go module cache 3.4 GB, npm cache 1.7 GB, uv cache (unused) 1.1 GB.

## Digest (no ping)

- **Go build cache: 8.2 GB** — low · `b2e5ca`
  - rebuilt on the next build
  - possible: run `go clean -cache` (frees up to 8.2 GB) — not doing it: floor: grant mac.clean missing
- **Hugging Face models: 14.3 GB** — low · `bc39e2`
  - downloaded models: expensive to fetch again — yours to prune
- **Battery: Normal, 767 cycles** — info · `31a60c`

## Under the hood

- components in the inventory: 367
- act tool-clean: report-only — needs the mac.clean grant (grants in warden.toml)
```

Only the low-disk finding is "worth your attention"; the caches go to the digest, which never pings. Label anything
with `warden feedback ID noise|digest|important` and `warden improve` will try to adjust the triage through its gate.
