---
name: go-maintenance
description: How to fix a Go dependency bump that broke the build or tests, with the smallest change.
---
# Fixing a Go dependency bump

1. Read the failing test output first; find the API that changed (`go doc MODULE.Symbol` in the worktree).
2. Prefer adapting call sites over pinning an older version; never weaken or delete tests.
3. Keep `go.mod` at the version the bump chose; run `go mod tidy` only if imports changed.
4. Finish with a two-line summary: what changed and why it is safe.
