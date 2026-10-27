---
name: k8s-triage
description: Root-cause a failing Kubernetes workload with read-only tools and propose a fixed manifest instead of changing the cluster.
---
# Kubernetes triage (read-only)

1. Use the kubernetes MCP tools read-only: events_list, pods_list_in_namespace, pods_get, pods_log (previous container for crash loops), resources_get.
2. Typical causes: CrashLoopBackOff → the container's last log lines and exit code; FailedMount → a missing Secret/ConfigMap; ImagePullBackOff → tag or registry auth; OOMKilled → memory limit.
3. Propose, don't apply: a minimal manifest patch (YAML) and a one-paragraph cause + how to verify. Applying is the owner's (grant k8s.apply).
