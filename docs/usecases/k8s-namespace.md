# A Kubernetes namespace

**Pack:** `packs/k8s-namespace` · **Demo:** `scripts/demo-k8s.sh` → `examples/demo-k8s.txt` (fixture: a fake
`kubectl` prints recorded JSON; no cluster is contacted)

Warden guards one namespace, read-only by default:

- `Warning` events and `CrashLoopBackOff` containers become incidents;
- container images become the inventory (news and advisories are matched against it);
- the fix is a **proposal** file (with the brain's root-cause analysis when a brain is configured);
- `kubectl apply` exists as an act but needs the `k8s.apply` grant and is always asked;
- the brain gets the read-only tools of `io.github.containers/kubernetes-mcp-server`, pinned from the MCP Registry in
  `warden.lock` (`warden add mcp:io.github.containers/kubernetes-mcp-server@0.0.67`). Until it is added, the server is
  refused and the report says what to run.

## Configuration

```toml
name    = "shop"
domain  = "pack:k8s-namespace"
params  = { context = "kind-staging", namespace = "shop" }
mode    = "act"
wake    = { every = "15m", listen = false }
grants  = []                                    # add "k8s.apply" only if Warden may apply (always asked)
rules   = [{ act = "propose", then = "auto" }]  # writing a proposal file is harmless
```

To get events as they happen instead of every 15 minutes, add a stream and run `warden listen`:

```toml
wake = { every = "15m", listen = true }

[[signals]]
name  = "events"
kind  = "stream"
argv  = ["kubectl", "--context=kind-staging", "get", "events", "-n", "shop", "--watch-only"]
trust = "trusted"
```

A stream is read one line per signal, and only lines that look like errors, orders or advisories are kept
(at most 120 a minute; the rest is counted and reported as dropped).

In a cluster, run Warden as a Deployment with a view-only ServiceAccount: `deploy/k8s/warden-namespace.yaml` and an
image from `scripts/build-image.sh IMAGE`. Inside a pod `bwrap` usually cannot create user namespaces, so that example
sets `sandbox = "none"` explicitly and relies on the pod as the boundary; the report header then says
`sandbox: NONE (owner's choice)`.

## What the report looks like

From `examples/demo-k8s.txt`:

```markdown
# Warden «shop» · 2026-10-27 19:22 · warden 0.2.0

Domain: **k8s-namespace** (`context=kind-demo namespace=shop`) · mode: **act** · triage v1 · sandbox: sandbox-exec

**Worth your attention: 3** (done: 3, waiting for you: 0, important: 0).

## Done

- ✅ **CrashLoopBackOff: container api (7 restarts)** — proposal written: $DEMO/.warden/proposals/ebe408ade36d.md
- ✅ **BackOff: Pod/api-7d9f4c** ×2 — proposal written: $DEMO/.warden/proposals/9cc2308096b5.md
- ✅ **FailedMount: Pod/api-7d9f4c** — proposal written: $DEMO/.warden/proposals/f3cbc43b9ebf.md

## Under the hood

- components in the inventory: 2
- act apply: report-only — needs the k8s.apply grant (grants in warden.toml)
- mcp server kubernetes: refused — not in warden.lock — run `warden add mcp:io.github.containers/kubernetes-mcp-server@0.0.67`
- feed https://kubernetes.io/feed.xml: first look, 50 items as the baseline
```

A proposal is a Markdown file with the finding, its data and (with a brain) the analysis. Nothing in the cluster is
changed.
