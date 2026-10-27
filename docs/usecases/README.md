# Use cases

One page per shipped pack, each with a real configuration and the report it produces (taken from `examples/`):

- [A Go project (and Node, Python)](go-project.md) — advisories, bumps in a worktree branch with green tests
- [A Mac](macos.md) — disk, caches, health; cleans only with a grant, through the tool's own command
- [A Kubernetes namespace](k8s-namespace.md) — incidents become proposals; apply only with a grant, always asked
- [A Linux server](linux-server.md) — read-only: disk, failed units, journal errors, GPUs, service health
- [A GitHub repository, maintained](github-maintainer.md) — issues into PRs, reviews, merges on green CI, releases; kill switch, budget
- [Signals: news, alerts and messages](signals.md) — feeds, chat adapters, log streams, webhooks, A2A; trust tiers
- [Warden updating itself](self-update.md) — verified releases (checksum + Sigstore build provenance), canary and stable, rollback, the fleet beacon
- [Writing your own domain pack](writing-a-pack.md) — the manifest, mapping output, floors
