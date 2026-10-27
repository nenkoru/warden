---
name: node-maintenance
description: How to fix an npm dependency bump that broke the tests, with the smallest change.
---
# Fixing an npm dependency bump

1. Read the failing test output first; find the API that changed (the package's CHANGELOG in node_modules/PKG).
2. Prefer adapting call sites over pinning an older version; never weaken or delete tests.
3. Keep the version the bump chose in package.json and package-lock.json (an override stays).
4. Finish with a two-line summary: what changed and why it is safe.
