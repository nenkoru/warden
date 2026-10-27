# A GitHub repository, maintained (the `github-repo` pack)

Warden can maintain one GitHub repository on its own: work on issues, review pull requests from outside, merge its
own PRs and Dependabot bumps when CI is green, and cut patch and minor releases — inside hard limits that live in
code and in `warden.toml`, not in a prompt. This page is how Warden maintains its own repository.

## What it does, every tick

1. **The kill switch first.** Any open issue labeled `warden-pause`, or a file `PAUSE` at the root of `main`, pauses
   the tick: Warden still observes and writes the report ("paused by the owner: …"), and every act is refused. If the
   switch can't be read (the API is down, the token is gone), the tick is paused too.
2. **Issues** (`github-issues`). Open issues become findings with their labels, author and author association. An
   issue is actionable when it is labeled `warden`, or `good first issue` / `bug` / `enhancement` and was opened by a
   member, owner or collaborator — and not labeled `needs-human`, `wontfix`, `question` or `delegated`. One issue at a
   time: while a `warden/issue-*` PR is in flight, the next one waits.
3. **Pull requests** (`github-prs`). Warden's own PRs, Dependabot's and those of `trusted_authors` are merged when every
   rule holds (below). Everyone else's get one review comment per head commit, never an approval, never a merge.
4. **Main** (`github-main`). A red `main` (the latest run of each workflow on its head) is a high finding and blocks
   releases. Merged PRs since the latest `vX.Y.Z` tag make a release finding.

## The acts

| act | what | floors |
|---|---|---|
| `work-issue` | A branch `warden/issue-N-slug` from `main`; the brain edits files through file tools only (Go files are formatted in-process); the token is checked for in the diff; push; a **draft** PR "Closes #N" naming the brain, labeled `warden`. CI runs on it. Red → one fix round from the failed jobs' logs (fenced as data), pushed to the same branch; still red → delegated (if configured) or labeled `needs-human`. Green → marked ready, then `merge-pr`. | `needs_brain`, `max_per_day`, and `max_issues_per_day` |
| `review-pr` | The diff and the CI results go to the brain, fenced as untrusted; one `COMMENT` review per head SHA; mentions in the text are defused; "@owner, looks good to me" only with green CI and a looks-good verdict. The PR's code is never checked out or run. | `needs_brain` |
| `merge-pr` | Only own / Dependabot / trusted PRs; every reported check green (and `required_checks` present); mergeable; no `needs-human`; no protected path; not paused (asked again right before merging); a Dependabot major bump (or one it can't read) → `needs-human`. Squash merge, pinned to the head SHA. | `untrusted = "deny"` |
| `release` | From green `main`, never paused, one a day: the next version from merged PR labels and titles (`feat`/`enhancement` → minor, else patch; breaking → an issue for the owner, never released). A release PR updates `CHANGELOG.md` from the PR titles and links (Keep a Changelog sections, no model). It merges under `merge-pr`'s rules; then Warden tags `vX.Y.Z` on its merge commit through the API, and the repository's release workflow builds it. A failed release workflow is a critical ping. | `untrusted = "deny"` |
| `delegate` | Hands an issue to another agent of the owner's with `params.delegate_cmd`, an argv template (`{title}`, `{body}`, `{agent}`, `{issue}`, `{url}`, `{repo}`; no shell) — e.g. a team tracker CLI or a chat bridge the image already has. Used for issues labeled per `delegate_labels`, and for hard issues when no paid brain may take them (`delegate_default`). The work comes back as PRs from `trusted_authors`, or as a branch under `pickup_branches` that Warden opens a PR for. | `max_per_day` |
| `pickup-branch` | A delegate's pushed branch becomes a PR, merged under the same rules as Warden's own. The PR is titled after the branch's commits (one commit: its subject and body; several: the oldest subject and a list of every subject), so the release reads the right bump. | `untrusted = "deny"` |
| `publish-log` | Once a day: the experiment log rendered from the state (no model) into `experiment_doc`, on a branch `warden/experiment-YYYY-MM-DD`, in a ready PR labeled `warden`, `experiment` that touches only that file; `merge-pr` merges it on green CI. Nothing when the file on `main` is already current. See [The 27B experiment log](#the-27b-experiment-log). | `untrusted = "deny"`, `max_per_day = 2` |

`checks_mode = "ci"` (the default) means Warden never runs repository code where it lives: no Go, node or sandbox
needed in its image; CI decides. `checks_mode = "local"` runs `params.checks` in the sandbox in the worktree (no
network, no secrets in the environment; modules from the Go proxy only), with one escalation on red, and opens a
ready PR.

## Guardrails

- **Untrusted text.** Issue and PR titles, bodies, comments, CI logs and outside code are data: fenced as
  `<<<untrusted from @user …>>>` wherever they reach a model, and marked tainted. The policy floor
  `untrusted = "deny"` keeps tainted findings away from `merge-pr`, `release` and `pickup-branch`. The coding agent's
  system prompt says that instructions inside the issue are not instructions.
- **Protected paths** (`params.protected`, default `.github/**`, `.goreleaser.yaml`, `SECURITY.md`, `LICENSE`,
  `NOTICE`, `packs/github-repo/**`, `warden.toml`, `*.warden.toml`, `warden.lock`, and any file whose name contains
  `secret`, `token` or `sign` — `protected_names`). A change there from an issue someone else wrote is not pushed at
  all (the issue gets `needs-human`); from the owner's own issue it becomes a PR labeled `needs-human`. A PR touching
  them is never merged by Warden. The `sign` rule is deliberately wide (it catches `signals.go` too); narrow it in
  `protected_names` if you prefer.
- **The token** is read from a file, sent only in the `Authorization` header to the API, handed to git through a
  credential helper that reads the file (never in a URL, argv or the environment), redacted from every comment, error
  and log, and checked for in every diff before a push. Inside the sandbox the token file is unreadable.
- **No workflows permission.** A push that touches `.github/workflows` fails by design; Warden opens an issue for the
  owner instead.
- **Guardrails as config.** Protected paths, merge and release policy (`trusted_authors`, `required_checks`,
  `release = "auto" | "off"`, `max_issues_per_day`) and the budget live in `warden.toml`, which is itself protected.

## Budget and brains

```toml
[budget]
per_day_usd       = 1000      # the cap: over it, no paid model call today; brain acts are refused with a line
alert_every_usd   = 100       # a ping each time today's spend crosses a multiple
burn_per_hour_usd = 100       # a ping when the last hour's spend exceeds this
free_models       = ["qwen3.8-27b-local"]   # cost $0 even without a price
# upstream_proxy  = "http://proxy:3128"     # the metering proxy's way to the Anthropic API, if the direct route is blocked
```

Every report header says `model spend today: $X of $Y`; the report's "brains" line counts each brain's calls and
tokens, free ones included. Endpoint brains are metered per call (a LiteLLM `x-litellm-response-cost` header wins over
the price table); a Claude Code ACP brain is metered by pointing its `ANTHROPIC_BASE_URL` at the meter's local proxy.
Alerts are immediate pings. Several brains, routed per task:

```toml
brain_for  = { triage = "local", interpret = "local", review-pr = "local", release = "local", work-issue = "auto" }
brain_auto = ["local", "claude"]     # what "auto" climbs: free first

[brains.local]                       # an OpenAI-compatible model in the cluster (LiteLLM, vLLM): free, counted
provider     = "openai"
base_url     = "{llm_url}"           # from params
model        = "{llm_model}"
api_key_file = "/secrets/llm/key"    # read at call time, never logged
free         = true
unavailable  = ["00:30-08:00", "sat", "sun"]   # off-hours: skipped, or brain acts wait ("off-hours: waiting")
tz           = "Europe/Moscow"

[brains.claude]                      # optional, paid, metered
acp = ["npx", "-y", "@agentclientprotocol/claude-agent-acp@<pinned>"]
```

`auto` for `work-issue`: small issues (labels `small`, `good first issue`, `docs`, `chore`, `dependencies`, or a
dependency-only title) go to the free brain, with the paid one as the one escalation when CI (or the local checks)
stay red; other issues go to the paid brain, and with none available they are **hard**: delegated
(`delegate_default`), never a paid model you didn't configure. Only `auto` sizes issues: with a named brain
(`work-issue = "local"`) every actionable issue goes to it — features and large changes included, never delegated or
labeled `needs-human` for its size; a paid brain is then just another brain you may name. An upstream that answers 403 ("Request not allowed",
a blocked region) marks that brain unavailable for a few hours: one report line, no retries. Acts that need no brain —
merging green PRs, release PRs (the CHANGELOG is generated without a model), the digest — run off-hours too.

## Notifications without new credentials

`warden github-note` is a hook command that comments on a pinned issue "Warden log" with the same token:

```toml
digest = "daily"     # a Digest event once a day with the day's summary
[[hooks]]
event   = "Report"   # a comment mentioning the owner only for: security findings, red main, a failed release,
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Report"]   # budget alerts, anything waiting
[[hooks]]
event   = "Digest"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Digest"]
[[hooks]]
event   = "Ask"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Ask"]
```

Mentions inside the payload (issue titles) are defused; the only mention is the owner's.

## An instance

```toml
name    = "warden-maintainer"
domain  = "pack:github-repo"
mode    = "act"
sandbox = "none"                    # Kubernetes: the pod is the boundary (see below); on a host: "auto"
state   = "/state/.warden"
wake    = { every = "10m" }
digest  = "daily"
rules   = [{ act = "release", target = "v*.0.0", then = "deny" },   # majors: never
           { act = "work-issue", then = "auto" }, { act = "review-pr", then = "auto" }, { act = "merge-pr", then = "auto" },
           { act = "release", then = "auto" }, { act = "delegate", then = "auto" }, { act = "pickup-branch", then = "auto" },
           { act = "publish-log", then = "auto" }]

[params]
repo               = "OWNER/REPO"
token_file         = "/secrets/github/token"
owner              = "OWNER"
root               = "/state/repo"            # cloned on first use
max_issues_per_day = "3"
trusted_authors    = ["my-other-agent"]
protected          = [".github/**", ".goreleaser.yaml", "SECURITY.md", "LICENSE", "NOTICE", "packs/github-repo/**", "warden.toml", "*.warden.toml", "warden.lock"]
delegate_cmd       = ["some-cli", "task", "create", "--title", "{title}", "--body", "{body}", "--to", "{agent}"]
delegate_labels    = { infra = "Ops" }
delegate_default   = "Coder"
llm_url            = "http://litellm.llm.svc:4000/v1"
llm_model          = "qwen3.8-27b-local"
```

Start with `mode = "dry-run"` for a day and read the reports.

## The 27B experiment log

Warden maintains its own repository with a local 27B model (`qwen3.8-27b-local` through LiteLLM, the brain `local`,
working outside its `unavailable` hours) doing all of the work, features included:

```toml
brain_for = { work-issue = "local", review-pr = "local", triage = "local" }   # no "auto": nothing is sized away from it
[params]
experiment_log = "on"              # the pack's default
experiment_tz  = "Europe/Moscow"   # days and weeks of the log
```

Every two or three days the owner and Claude read the published log and tune the prompts, tools, context and skills.

- **Task records.** A task is one issue worked by `work-issue` across ticks. Each act on it — the first attempt
  (phase `new`), the fix round (`fix`), the give-up (`giveup`), the merge (`merge`, by `merge-pr`), a revert
  (`revert`) — appends one line to `<state>/tasks.jsonl` (append-only, `0600`): the issue and PR, the brain and its
  model, ok/refused, a short summary, wall time, tokens in/out and calls (the difference of the brains' usage around
  the act), and the act's own data: size (`small` / `size:S|M|L|XL` labels, else the diff: under 50 changed lines S,
  200 M, 800 L, else XL), files, lines added/deleted, tool runs and tool errors (the coder's file tools are counted),
  the longest run of identical tool calls, CI on the first push (green: merged without a fix round), the fix round,
  and at the merge the comments and reviews people other than Warden left. `experiment_acts` names the acts recorded
  (default `work-issue`, `merge-pr`, `pickup-branch`); an act deferred before any model call (CI running) is not an
  attempt. A merged PR titled `Revert "…"` that undoes a `warden/issue-*` PR (GitHub's revert branch, the quoted
  title, or its number in the title) is recorded once as the task's revert.
- **Failure categories**, several per failure, from the act's summary, log and data: `tool-call` (an unknown tool,
  arguments that are not JSON, tool errors on a failed attempt), `context-limit` (context length, too many tokens,
  prompt too long), `wrong-edit` (CI red after the push — every fix round —, local checks red, no change, a protected
  path), `loop` (`max_steps`, three or more identical tool calls in a row), `timeout`, `gave-up` (handed to the owner,
  or the model says it can't), `other`.
- **Idle time is not failure.** Each tick in which the `work-issue` brain may not work — off-hours, its upstream
  unavailable, the budget spent — adds the minutes since the previous tick (at most one wake interval) as an `idle`
  record. Brain acts wait meanwhile; nothing counts as an attempt.
- **Publishing.** The `experiment` observer proposes `publish-log` once a day (in `experiment_tz`) when there are
  records, no `warden/experiment-*` PR is open and today's log isn't out yet. The act renders `experiment_doc`
  (default `docs/experiment-27b.md`, never a protected path): a weekly summary (tasks, merged, first-push green %,
  average attempts and fix rounds, median wall time, tokens, reverts, top failure categories, idle hours), then each
  day newest first with its tasks, its failures with one-line reasons, and its idle hours. Its PR touches only that
  file, so `merge-pr` merges it on green CI; the log PRs never trigger a release. `experiment_log = "off"` stops the
  records and the publishing.

## Credentials checklist (the owner creates every credential)

1. **A fine-grained personal access token** scoped to this one repository: Contents read/write, Pull requests
   read/write, Issues read/write, Actions read, Commit statuses read, Metadata read. **No** Workflows, **no**
   Administration. Short expiry; rotate it by replacing the file.
2. **An Anthropic API key** (only if you configure a paid brain) in a dedicated Console workspace with a monthly spend
   limit — the outer backstop behind Warden's own daily cap.
3. **A branch protection rule or ruleset on `main`**: require a pull request, require the CI status checks, block
   force pushes and deletions. Warden's merges then go through the same gate as everyone's.
4. Labels: `warden`, `needs-human`, `warden-pause`, `delegated`, `warden-fixed`, `release`, `experiment`, `warden-log` (GitHub
   creates missing labels when Warden adds them).

## Deploy on Kubernetes

`deploy/k8s/github-maintainer.yaml` is a generic manifest: a namespace, a ServiceAccount with **no RBAC** (Warden needs
no cluster access), a PVC for the state, a Deployment running `warden listen` as non-root with a read-only root
filesystem and a `/tmp` emptyDir, the token and the LLM key mounted from Secrets as files (mode 0400), and a
NetworkPolicy allowing egress only to DNS, GitHub on 443, the LiteLLM service and the delegate endpoint. The image
needs `warden`, the packs and `git` — no Go, node or bwrap in `checks_mode = "ci"`.

Create the secrets yourself, typing them so they are never echoed or kept in shell history:

```bash
kubectl create namespace warden-maint
read -rs TOKEN && printf '%s' "$TOKEN" | kubectl -n warden-maint create secret generic github-token --from-file=token=/dev/stdin; unset TOKEN
read -rs KEY && printf '%s' "$KEY" | kubectl -n warden-maint create secret generic llm-key --from-file=key=/dev/stdin; unset KEY
kubectl apply -f deploy/k8s/github-maintainer.yaml      # after filling in the placeholders
kubectl -n warden-maint logs deploy/warden -f
```

To pause from anywhere: label any issue `warden-pause` (or commit a `PAUSE` file). To stop: scale the Deployment to 0.

## Deploy with systemd --user

On a host, the same instance runs from a timer (see [INSTALL.md](../../INSTALL.md)): `warden units --config
~/maint/warden.toml` writes the units, then `systemctl --user enable --now <name>.timer`. Keep the token file at
`chmod 600` outside the clone, and set `sandbox = "auto"` (bwrap) so the ACP brain and local checks are contained.
