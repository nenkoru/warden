# Warden research brief (2026-10-27)

Legend: **[V]** = checked this session on the primary page. **[S]** = secondary source or search snippet only. **[K]** = prior knowledge, URL not fetched, so verify.

## 1. Google "Harness^2"

**Found under that exact name:** *Harness^2: Recursive Agent Harnessing for an Open World*, by Xu, Chen, … Lee and Pfister (Google Cloud AI Research), arXiv:2610.14224, 9 Oct 2026. [V] https://arxiv.org/abs/2610.14224 , https://arxiv.org/html/2610.14224
- **What it edits:** the harness only, never the weights. That means 8 files: system prompt, guardrails, memory, skills, tool guidance, scripts, subagents and a middleware plugin. [V]
- **Propose-Probe-Compose (per task):**
  - Run the current harness to get a reference.
  - Propose an *invariant* edit (planning, tool use, delegation) and a *domain-specific* edit.
  - Probe each one on the same task under matched settings, and diff the trajectories against the reference ("behavioral contrast").
  - The composer then merges what the evidence supports, adopts one candidate, or **keeps the current harness**. [V]
- **Cross-task loop:** a reflector stores the adopted harnesses with an `EXPERIENCE.md` (situation, lessons, evidence) plus an index. A lesson is kept only if execution contrasts support it. **Each task restarts from the base harness H0.** [V]
- **Gates and regressions:** there are no ground-truth verifiers or numeric thresholds. Each decision must cite an observed contrast (fixed / unchanged / regressed). The paper says the result is "adopted rather than verified optimal". It describes **no explicit rollback** beyond the H0 reset. [V]
- **Results:** up to +11.5 on WorkBuddy-Bench, +4.5 on JobBench and +11.8pp on LAB. Gains scale with the recursion budget k. Probes are noisy and the judges are LLMs. [V]
- **Code:** google-research/harness2 (Apache-2.0). The default editor is Claude Opus 4.8. Executor network was unrestricted, so rubric leakage is possible. [S] https://rits.shanghai.nyu.edu/ai/google-open-sources-harness2-for-self-improving-ai-agents/

**Closest related work:**
- **Meta-Harness** (Lee, Khattab, Finn, Mar 2026). A coding-agent proposer reads all prior candidates' code, scores and traces from a filesystem. It beat hand-built harnesses on TerminalBench-2. [S] https://arxiv.org/abs/2603.28052
- **Darwin Gödel Machine** (Sakana/UBC). Self-edit, benchmark, then add to an open-ended archive. Runs were sandboxed with human oversight. It **gamed its evaluator** by removing hallucination markers. [S] https://arxiv.org/abs/2505.22954
- **SICA.** The agent edits its own code, with the best archived agent acting as meta-agent. 17% to 53% on a SWE-Bench Verified subset. [S] https://arxiv.org/abs/2504.15228
- **ACE.** Context as an evolving playbook (Generator/Reflector/Curator), using incremental deltas to avoid "context collapse". [S] https://arxiv.org/abs/2510.04618
- **2026 harness-RSI papers** (abstracts unverified) [S]:
  - RRSI: https://arxiv.org/abs/2609.24972
  - HarnessBank: https://arxiv.org/abs/2607.13683
  - RegenHarness: https://arxiv.org/abs/2609.27612
- **Older lineage** [K]:
  - ADAS: https://arxiv.org/abs/2408.08435
  - Gödel Agent: https://arxiv.org/abs/2410.04444
  - AlphaEvolve: https://deepmind.google/discover/blog/alphaevolve-a-gemini-powered-coding-agent-for-designing-advanced-algorithms/

## 2. Durable / always-on patterns

- **Cloudflare Agents.** `schedule()` takes a delay, a Date or a cron string. Tasks persist in SQLite, survive restarts and are woken by Durable Object alarms. `scheduleEvery()` skips overlapping runs. [V] https://developers.cloudflare.com/agents/api-reference/schedule-tasks/
- **LangGraph.** A checkpointer stores per-thread state (HITL, time travel, fault tolerance). A Store holds long-term memory. [V] https://docs.langchain.com/oss/python/langgraph/persistence
- **Durable execution engines** checkpoint each side-effecting step so a crash resumes instead of re-running [K]:
  - Temporal: https://docs.temporal.io/workflows
  - DBOS, a Postgres-backed library: https://docs.dbos.dev/
  - Restate: https://docs.restate.dev/
  - Inngest: https://www.inngest.com/docs
- **OpenClaw heartbeat** [S]:
  - A periodic turn runs every 30m by default; `0m` disables it. https://docs.nvidia.com/nemoclaw/user-guide/openclaw/configure-agents/configure-agent-heartbeats
  - `HEARTBEAT.md` holds the checklist. If it's empty, the LLM call is skipped. The reply `HEARTBEAT_OK` means stay silent. https://docs2.openclaw.ai/reference/templates/HEARTBEAT.md
- **Claude Code Routines** (Apr 2026, preview). Prompt, repos and MCP connectors run on Anthropic's cloud. Triggers are cron, API or GitHub events, with daily caps per plan. [S] https://thenewstack.io/claude-code-can-now-do-your-job-overnight/
- **Background coding agents:**
  - Codex cloud: a sandbox per task, agent internet off by default. [S] https://openai.com/index/introducing-codex/
  - Cursor Cloud Agents: a VM per agent, with network policies. [S] https://cursor.com/help/ai-features/background-agents
  - Copilot coding agent: ephemeral environment, 59-min cap, one branch/PR per task, single repo. [V] https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-coding-agent
  - Jules: https://jules.google [K]
  - Devin: https://docs.devin.ai [K]
- **Ambient agents** (LangChain). Event-triggered rather than chat-triggered, with three modes: **Notify**, **Question** and **Review**. [V] https://blog.langchain.com/introducing-ambient-agents

## 3. Safety patterns for self-modifying agents

- **Probe before adopting, with "keep current" as a valid outcome**, and keep lessons only when evidence supports them. [V] https://arxiv.org/html/2610.14224
- **Reset to an immutable base every episode** so bad edits can't compound (Harness^2 H0). [V] same
- **Keep an archive of every variant with its score** so you can roll back to any ancestor (DGM, SICA). [S] links above
- **Keep the evaluator outside the agent's write scope.** DGM hacked its own metric. [S] https://the-decoder.com/sakana-ais-darwin-godel-machine-evolves-by-rewriting-its-own-code-to-boost-performance/
- **Apply delta edits, not rewrites** (ACE). [S]
- **Sandbox with a default-deny network and scope work to one repo** (Codex, Cursor, Copilot rulesets). See section 2 links.
- **Hard-code human review for dangerous actions.** [V] https://blog.langchain.com/introducing-ambient-agents
- **Unverified:** I found no formal regression-suite gate or automatic rollback inside Harness^2. Appendix F was not read.

## 4. Cheap vulnerability / release feeds

- **OSV querybatch** [V] https://google.github.io/osv.dev/post-v1-querybatch/
  - Request: `POST https://api.osv.dev/v1/querybatch` with body `{"queries":[{"package":{"ecosystem":"PyPI","name":"jinja2"},"version":"2.4.1"},{"package":{"purl":"pkg:npm/x@1.0.0"}},{"commit":"<sha>"}]}`.
  - Don't combine `version` with a versioned purl; that returns 400.
  - Response: results in input order, only `{id, modified}` per vuln. Fetch details with `GET /v1/vulns/{id}`.
  - Pagination: `next_page_token` once a query passes 1,000 results or the batch passes 3,000.
  - The page states no auth or rate limit.
- **GitHub Advisory DB** [V] https://docs.github.com/en/rest/security-advisories/global-advisories
  - `GET /advisories` with filters `ecosystem`, `affects` (up to 1000 `pkg@ver`), `modified` date ranges, `severity`, `type` and `epss_*`.
  - `per_page` max is 100, with cursor pagination.
- **GitHub rate limits** [V] https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api
  - 60/h unauthenticated, 5,000/h with a token.
  - Authenticated ETag requests that return 304 are free. [S] https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api
- **Releases** [S] https://changedetection.io/tutorial/atom-feed-monitoring-github-project-releases-just-one-easy-example
  - Atom feed: `https://github.com/{owner}/{repo}/releases.atom`, roughly the last 10 entries, no auth for public repos.
- **NVD CVE API 2.0** [S]
  - 5 requests per 30s without a key, 50 with one. https://app.unpkg.com/@nodesecure/vulnera@3.1.0/files/dist/database/nvd.d.ts
  - Sync incrementally with `lastModStartDate` plus `lastModEndDate`, used together, max 120 days. https://pypi.org/project/nvd-api/
  - Workflow guide: https://nvd.nist.gov/developers/api-workflows
  - **Not verified on nvd.nist.gov**, because those pages did not render.
- **deps.dev v3** [S] https://docs.deps.dev/api/v3/index.html
  - `GET https://api.deps.dev/v3/systems/{system}/packages/{name}/versions/{version}` returns licenses and advisory keys.
  - The API is stable.

## Design takeaways for Warden

1. Make the harness a few editable files: prompt, memory, skills, tool notes, scripts. Self-edits touch only those.
2. Keep an immutable floor out of the agent's write scope: guardrails, permissions, the evaluator, the notify policy and the edit gate.
3. Gate every self-edit by replaying recorded tasks with the old and new harness and diffing the behaviour and cost. "Keep current" is the default.
4. Put the harness in git: one commit per adopted edit with an `EXPERIENCE.md` rationale. Auto-revert if later heartbeats get worse on cost, errors or human rejections.
5. Write memory as incremental deltas (ACE) and prune it on a schedule.
6. Let a heartbeat drive the loop, with deterministic pollers deciding first (ETag/304, OSV `modified`, NVD `lastMod`). Call the LLM only on a change. `HEARTBEAT_OK` means silence.
7. Run a launchd loop with a SQLite step journal (DBOS-style). Temporal-level infrastructure is overkill for one Mac.
8. Vuln flow: OSV querybatch over the lockfile purls, then fetch details only for new IDs. Cross-check with GHSA `affects`, and use NVD only for CVSS, with a key.
9. Release flow: `releases.atom` or authenticated REST with ETags, plus deps.dev for "newer version + advisories".
10. Reporting follows LangChain's modes: Notify above a severity threshold, Question when blocked, Review before anything irreversible. Everything else goes into a digest.
11. Mac and project actions run sandboxed with a default-deny network, on a branch with one PR per task, and nothing destructive without Review.
12. Budget the self-improvement. Harness^2 gains and tokens both rise with k, so cap it, for example one nightly run on a fixed budget.
