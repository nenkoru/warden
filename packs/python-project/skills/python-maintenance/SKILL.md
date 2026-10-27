---
name: python-maintenance
description: How to fix a PyPI dependency bump that broke the tests, with the smallest change.
---
# Fixing a PyPI dependency bump

1. Read the failing test output first; find the API that changed (the package's changelog; its code is in .warden-venv/lib).
2. Prefer adapting call sites over pinning an older version; never weaken or delete tests.
3. Keep the == pin the bump chose in requirements.txt; never edit .warden-venv.
4. Finish with a two-line summary: what changed and why it is safe.
