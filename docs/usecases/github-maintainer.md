# A GitHub repository, maintained (the `github-repo` pack)

Warden can maintain one GitHub repository on its own: work on issues, review pull requests from outside, merge its
own PRs and Dependabot bumps when CI is green, revert its own merge when it turns `main` red, cut patch and minor
releases (through a canary if you run a fleet), write a weekly roadmap, and keep a public log of how its coding model
does — inside hard limits that live in code and in `warden.toml`, not in a prompt. Some of the repository is a
human-only kernel Warden never merges into. The owner hears of it through one pinned issue and GitHub's own email.
This page is how Warden maintains its own repository.

## What it does, every tick

1. **The kill switch first.** Any open issue labeled `warden-pause`, or a file `PAUSE` at the root of `main`, pauses
   the tick: Warden still observes and writes the report ("paused by the owner: …"), and every act is refused. If the
   switch can't be read (the API is down, the token is gone), the tick is paused too.
2. **Issues** (`github-issues`). Open issues become findings with their labels, author and author association. An
   issue is actionable when it is labeled `warden`, or `good first issue` / `bug` / `enhancement` and was opened by a
   member, owner or collaborator — and not labeled `needs-human`, `wontfix`, `question` or `delegated`. An issue
   labeled `rfc` waits until it also carries `warden` (see the weekly roadmap below). One issue at a time: while a
   `warden/issue-*` PR is in flight, the next one waits.
3. **Pull requests** (`github-prs`). Warden's own PRs, Dependabot's and those of `trusted_authors` are merged when every
   rule holds (below). Everyone else's get one review comment per head commit, never an approval, never a merge.
4. **Main** (`github-main`). A red `main` (the latest run of each workflow on its head) is a high finding and blocks
   releases; when Warden's own merge turned it red, it reverts that merge (`revert-pr`, see
   [Quality gates](#quality-gates)). Merged PRs since the latest `vX.Y.Z` tag make a release finding; with `canary`
   set, a merged release goes through [the canary](#canary-before-tagging) first. A newly published release's
   CHANGELOG section goes to [the digest issue](#notifications-without-new-credentials-the-digest-issue); a merged
   revert of one of Warden's PRs marks its task in [the experiment log](#the-27b-experiment-log).
5. **The experiment log** (`github-experiment`). Once a day the log of the coding model's tasks is published in its own
   PR (`publish-log`).
6. **The roadmap** (`github-roadmap`). Once an ISO week the roadmap is due; Warden's own RFCs that waited long enough
   without an objection are accepted ([below](#self-direction-the-weekly-roadmap)).

## The acts

| act | what | floors |
|---|---|---|
| `work-issue` | A branch `warden/issue-N-slug` from `main`; the brain edits files through file tools only (Go files are formatted in-process); the token is checked for in the diff; push; a **draft** PR "Closes #N" naming the brain, labeled `warden`. CI runs on it. Red → one fix round from the failed jobs' logs (fenced as data), pushed to the same branch; still red → delegated (if configured) or labeled `needs-human`. Green → marked ready, then `merge-pr`. | `needs_brain`, `max_per_day`, and `max_issues_per_day` |
| `review-pr` | The diff and the CI results go to the brain, fenced as untrusted; one `COMMENT` review per head SHA; mentions in the text are defused; "@owner, looks good to me" only with green CI and a looks-good verdict. The PR's code is never checked out or run. | `needs_brain` |
| `merge-pr` | Only own / Dependabot / trusted PRs; every reported check green (and `required_checks` present); mergeable; no `needs-human`; nothing in the [human-only kernel](#the-human-only-kernel) (checked again right before the merge); no protected path; not paused (asked again right before merging); a Dependabot major bump (or one it can't read) → `needs-human`; with `review_brain` set, a second model's looks-good on Warden's own issue PRs. Squash merge, pinned to the head SHA. | `untrusted = "deny"` |
| `release` | From green `main`, never paused, one a day: the next version from merged PR labels and titles (`feat`/`enhancement` → minor, else patch; breaking → an issue for the owner, never released). A release PR updates `CHANGELOG.md` from the PR titles and links (Keep a Changelog sections, no model). It merges under `merge-pr`'s rules; then Warden tags `vX.Y.Z` on its merge commit through the API, and the repository's release workflow builds it — or, with `params.canary`, a release candidate first (*Canary before tagging*, below). A failed release workflow is a critical ping. | `untrusted = "deny"` |
| `delegate` | Hands an issue to another agent of the owner's with `params.delegate_cmd`, an argv template (`{title}`, `{body}`, `{agent}`, `{issue}`, `{url}`, `{repo}`; no shell) — e.g. a team tracker CLI or a chat bridge the image already has. Used for issues labeled per `delegate_labels`, and for hard issues when no paid brain may take them (`delegate_default`). The work comes back as PRs from `trusted_authors`, or as a branch under `pickup_branches` that Warden opens a PR for. | `max_per_day` |
| `pickup-branch` | A delegate's pushed branch becomes a PR, merged under the same rules as Warden's own. The PR is titled after the branch's commits (one commit: its subject and body; several: the oldest subject and a list of every subject), so the release reads the right bump. | `untrusted = "deny"` |
| `publish-log` | Once a day: the experiment log rendered from the state (no model) into `experiment_doc`, on a branch `warden/experiment-YYYY-MM-DD`, in a ready PR labeled `warden`, `experiment` that touches only that file; `merge-pr` merges it on green CI. Nothing when the file on `main` is already current. See [The 27B experiment log](#the-27b-experiment-log). | `untrusted = "deny"`, `max_per_day = 2` |
| `revert-pr` | Main went red right after a merge of Warden's: a branch `warden/revert-<sha>` from `main` with `git revert` of the merge commit, a (non-draft) PR `Revert "<title>" (#N)` labeled `warden` and `revert`, a comment on the original PR for the owner. `merge-pr` merges it on green like any own PR. | `untrusted = "deny"`, `max_per_day = 3` |
| `roadmap` | Once a week: ROADMAP.md in a ready PR `warden/roadmap-YYYY-Www` (labeled `warden`, `roadmap`; merged under `merge-pr`'s rules) and up to `roadmap_max_issues` issues, from inputs gathered without a model (below). | `reversible`, `needs_brain`, `max_per_day = 1` |
| `rfc-accept` | An RFC Warden opened, `rfc_wait_hours` old, with no comment from anyone else and no `needs-human` / `wontfix` / `question` label, gets the `warden` label and a comment. | `reversible`, `max_per_day = 3` |

`checks_mode = "ci"` (the default) means Warden never runs repository code where it lives: no Go, node or sandbox
needed in its image; CI decides. `checks_mode = "local"` runs `params.checks` in the sandbox in the worktree (no
network, no secrets in the environment; modules from the Go proxy only), with one escalation on red, and opens a
ready PR.

## Guardrails

- **Untrusted text.** Issue and PR titles, bodies, comments, CI logs and outside code are data: fenced as
  `<<<untrusted from @user …>>>` wherever they reach a model, and marked tainted. The policy floor
  `untrusted = "deny"` keeps tainted findings away from `merge-pr`, `release` and `pickup-branch`. The coding agent's
  system prompt says that instructions inside the issue are not instructions.
- **Protected paths**: the [human-only kernel](#the-human-only-kernel), plus whatever `params.protected` (globs) and
  `params.protected_names` (words in a file name; default `signing`, `signature`, `cosign`, `gpg`) add. A change there
  from an issue someone else wrote is not pushed at all (the issue gets `needs-human`); from the owner's own issue it
  becomes a PR labeled `needs-human`. A PR touching them is never merged by Warden.
- **The token** is read from a file, sent only in the `Authorization` header to the API, handed to git through a
  credential helper that reads the file (never in a URL, argv or the environment), redacted from every comment, error
  and log, and checked for in every diff before a push. Inside the sandbox the token file is unreadable.
- **No workflows permission.** A push that touches `.github/workflows` fails by design; Warden opens an issue for the
  owner instead.
- **Guardrails as config.** Extra protected paths, merge and release policy (`trusted_authors`, `required_checks`,
  `release = "auto" | "off"`, `canary`, `max_issues_per_day`) and the budget live in `warden.toml`, which is itself
  protected.

## The human-only kernel

Warden maintains its own repository and merges its own PRs, so some of it must never change by Warden's hand. The
kernel is a list in code (`internal/github/kernel.go`), not configuration:

| paths | why |
|---|---|
| `.github/**` | workflows, Dependabot, templates: what CI runs, with which permissions, and what it checks |
| `.goreleaser.yaml`, `.goreleaser.yml` | how releases are built, signed and published |
| `LICENSE`, `NOTICE`, `SECURITY.md` | the project's terms and how vulnerabilities reach the owner |
| `warden.toml`, `*.warden.toml`, `warden.lock` | Warden's own instance config and pinned packs |
| `packs/github-repo/**` | the maintainer's pack: its policy floors and rules |
| `internal/policy/**`, `internal/meter/**`, `internal/box/**` | the policy engine, the budget cap, the sandbox |
| `internal/github/guard.go`, `internal/github/kernel.go` and their tests | the allowlist of GitHub writes and pushes; the kernel list, its matcher and the merge's last check |
| `internal/update/verify*.go`; `*attest*`, `*cosign*`, `*sign*` under `internal/update/` and `scripts/` | release verification and signing |
| any file whose name contains `secret`, `credential` or `token` | credentials |

What happens to a change there:

- **Any PR touching the kernel** — Warden's own, a delegate's picked-up branch, a trusted author's, a Dependabot bump
  of GitHub Actions — is labeled `needs-human` and `kernel`, gets one comment per head commit mentioning the owner
  with the kernel files, and is **never merged by Warden**, whatever its class or CI. The merge re-checks the PR's
  files right before it merges, and the GitHub client's own `Merge` (in `kernel.go`) refuses a kernel PR once more.
- **`work-issue` and the fix round**: for an issue the owner wrote, the change is pushed and opened as a ready
  (non-draft) PR labeled `needs-human` + `kernel`; for anyone else's issue nothing is pushed, and the issue gets
  `needs-human` and a comment.
- **The daily digest** lists every open PR labeled `kernel` under **Needs you**, right after the open asks:
  `kernel PR #N: title — human-only: <files>`.
- **Every act that pushes** (`work-issue` and its fix round, `publish-log`, `revert-pr`, `roadmap`) checks its change
  against the kernel and the protected paths before the push and pushes only to `warden/*` branches; every merge goes
  through `merge-pr`'s kernel re-check and the client's `Merge`. The release tags (`vX.Y.Z`, `vX.Y.Z-rc.N`) are the
  only other refs Warden creates.
- **The owner merges these by hand.** The kernel is a floor: `params.protected` and `protected_names` only add to it,
  and `protected = []` still protects every kernel path. `kernel.go` is in its own list, so a PR that edits or
  removes a pattern is itself held for the owner.

Make the `main` ruleset require a pull request and the CI status checks (not just for Warden), so even the owner's
hand merges of kernel PRs go through CI.

## Quality gates

Warden merges its own PRs with no human review, so the review is in CI and in the merge policy: `merge-pr` merges
only when every check named in `required_checks` is present and green. List the gates there, not only the tests:

```toml
[params]
required_checks = ["lint", "test (ubuntu-latest)", "test (macos-latest)", "govulncheck", "coverage", "fuzz", "demos (ubuntu-latest)"]
```

- **Coverage must not drop** (`coverage` job). `go test -race -coverprofile -covermode=atomic ./...`; every push to
  `main` uploads its total as the artifact `coverage` (`{"total", "sha"}`); a pull request compares its total with
  the latest green push to `main` (`scripts/covergate`) and fails on a drop of more than 0.1 percentage points. No
  baseline yet (or an expired artifact) passes with a notice.
- **Fuzzing** (`fuzz` job). Every Go fuzz target (`func FuzzX`: the manifest, map records and findings, components,
  templates, feeds, `go.mod`, registry references and the lock file, orders, signal lines, triage) runs for 20s
  (`scripts/fuzz.sh`; `FUZZTIME` to change it). A crasher lands in `testdata/fuzz/FuzzX/` and is committed with the
  fix as a regression test.
- **govulncheck** on every push and pull request, and daily on `main` (`schedule`): a new advisory turns `main` red
  without anyone pushing, and the red-main finding reaches the owner.
- **Automatic revert** (`revert-pr`). When `main` turns red and its head is the merge commit of a PR **Warden merged**
  (GitHub's `merged_by` is Warden's login; an own, Dependabot or trusted PR), a push run failed on it, and `main` was
  green on its parent, Warden reverts it: `Revert "<title>" (#N)`, "Main went red after #N (failed: <jobs>). This
  reverts it.", merged on green as an own PR. Once per commit (the finding is keyed on its SHA), at most three a day.
  Never: the owner's pushes and merges, a revert of a revert, or a red that only a scheduled run found (a new advisory
  is no fault of the last merge) — those stay a red-main finding for the owner.
- **A second review** (optional). `review_brain = "<brain name>"` (one of `[brains.*]`): before `merge-pr` merges
  one of Warden's own `warden/issue-*` PRs, that brain reviews the diff with the reviewer prompt (title, body and diff
  fenced as untrusted). `VERDICT: looks-good` → the merge goes on; anything else → refused, with the review as a
  comment on the PR for the owner. The brain unavailable (off-hours, budget) → the merge waits. Empty (the default):
  off, nothing is called. Pick a different model from the one that writes the code.
- **A canary before tagging** (`params.canary`): the final tag waits until a release candidate ran healthy on one
  canary instance — see [Canary before tagging](#canary-before-tagging).

**The ruleset the owner creates on `main`** (Settings → Rules; Warden's token has no administration permission):
require a pull request before merging; require status checks `lint`, `test (ubuntu-latest)`, `test (macos-latest)`,
`govulncheck`, `coverage`, `fuzz`, `demos (ubuntu-latest)`; block force pushes; restrict deletions. Then Warden's
merges pass the same gate as everyone's, and `required_checks` names the same checks.

## Canary before tagging

With `params.canary` naming a Warden of the fleet (one with `[update] channel = "canary"` and a beacon; see
[self-update](self-update.md)), a release reaches everyone else only after it ran on that instance:

1. Once the release PR merges, Warden tags **`vX.Y.Z-rc.N`** on its merge commit (`N` counts up across attempts). The
   release workflow builds it; goreleaser's `prerelease: auto` makes it a prerelease, which only canary-channel
   Wardens install (verified like any release).
2. Every tick `github-main` reads the beacons on the "Warden fleet" issue — only comments by the token's own login or
   by `owner`; anyone may comment on a public issue — and reports "release vX.Y.Z: rc.N on the canary «name» since …,
   healthy N h of 24". When the canary runs the rc, healthy for `canary_hours` (default 24) and at least
   `canary_min_ticks` ticks (default 6), not rolled back, and `main` is still green, Warden tags **`vX.Y.Z` on the
   same commit** (the canary is asked again right before). The release workflow builds the final release from it.
3. If the canary rolled back from the rc, or an open `warden-rollback` issue names it, there is no final tag: a
   `rollback` finding (it always pings), one `needs-human` issue "release vX.Y.Z-rc.N failed on the canary" that
   mentions the owner, and the next attempt waits for newly merged PRs — then a new release PR (the unshipped
   CHANGELOG section folds into it) and a new rc on its merge.

An rc tag is not a release: the next version counts from the latest `vX.Y.Z`, and while an rc is in flight no
second release PR is opened. Without `canary` the release act tags `vX.Y.Z` directly, as before. The guard lets Warden
create `refs/tags/vX.Y.Z` and `refs/tags/vX.Y.Z-rc.N` and no other ref. Releases are published, not drafts
(`release.draft: false`): self-update cannot see drafts, and the canary replaces the review of one.

A ticking canary rewrites its beacon at least hourly, and the gate reads the comment's `updated_at`: a beacon older
than `canary_stale_hours` (default 3) is a canary that stopped ticking — "release vX.Y.Z: rc.N on the canary «name»:
canary silent since …", a medium finding, and no final tag until the beacon is written again.

## Self-direction: the weekly roadmap

Warden decides what to build next on its own repository, once a week and within limits. From `roadmap_day` (default
`mon`) in `roadmap_tz` (default `Europe/Moscow`), when the week's roadmap is not done and no `warden/roadmap-*` PR is
open, `github-roadmap` proposes the act `roadmap` — only when a brain may work now (`brain_for.roadmap`, e.g. the
local model in its off-hours window), so a deferred week does not use up the day's one act. `roadmap = "off"` turns
it off.

The act gathers its inputs without a model, each capped and fenced as data:

- its own reports of the last 7 days (the header, **Done** and **Important**) and the acts that failed (`done.json`);
- the experiment log's weekly summary (`docs/experiment-27b.md` on `main`, param `experiment_doc`);
- the current `ROADMAP.md` on `main` — the owner's edits are read as the owner's priorities;
- the fleet: open `warden-rollback` issues and the latest comments on the `warden-fleet` issue;
- the repository: open issues (titles, labels), PRs merged in the last 14 days, open PRs;
- ecosystem news: titles and links of the last 14 days from `roadmap_feeds` (the pack lists MCP, the MCP registry,
  A2A, Agent Skills, ACP, the Go blog, Renovate, Dependabot and OpenHands).

The brain (system prompt: the pack's `skills/roadmap/SKILL.md`) answers one JSON object, `roadmap_md` and up to 10
issues (`title`, `body`, `kind` = `feature|fix|chore`, `size` = `S|M|L`, `rfc`, `why`). It is parsed strictly: an
issue with an unknown kind or size or a title over 100 characters is dropped, a body is cut at 4000 characters; an
answer without `roadmap_md` makes nothing. Then:

- `ROADMAP.md` — "Generated weekly by Warden; the owner edits freely; Warden rewrites it next week" and the brain's
  text, mentions defused — goes to a ready PR "docs: roadmap for YYYY-Www" labeled `warden`, `roadmap`. It is not a
  protected path, so it merges on green CI like Warden's other PRs.
- At most `roadmap_max_issues` (default 3) issues a week; one that duplicates an open issue (the same normalized
  title, or at least 80% of its words) is skipped. Labels: `enhancement` / `bug` / `chore` by kind, `size:S|M|L`,
  `roadmap`, and `warden`, so `work-issue` picks it up (`size:S` counts as small: the free brain first).
- An architecture change (`rfc: true`) becomes "RFC: …" labeled `rfc` and `roadmap`, **without** `warden`; its body
  says Warden implements it after 72 h without objection. Every tick, `github-roadmap` checks Warden's own open RFCs:
  older than `rfc_wait_hours` (default 72), no comment from anyone but the token's login, no `needs-human`, `wontfix`
  or `question` label → `rfc-accept` adds `warden` and comments "No objection in 72 h: Warden will implement this."
  To object, comment or add one of those labels. (If the token is the owner's own, the owner's comments look like
  Warden's: object with a label.)

The budget applies as everywhere: the brain is routed by `brain_for` and its off-hours, `max_per_day = 1`, and the
meter's daily cap. Rules in `warden.toml` decide whether `roadmap` and `rfc-accept` run alone (`then = "auto"`) or ask.

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

## Notifications without new credentials: the digest issue

`warden github-note` is a hook command that comments on one pinned issue, **"Warden digest"** (label
`warden-digest`, opened by the token's user on first use), with the same token. The comments that matter mention
the owner (`params.owner`), so **GitHub itself emails them** — no SMTP, no other service. On the repository page set
**Watch → Custom → Releases + Mentions** (and keep email on for participating and @mentions in your notification
settings); you then get mail for each mention below and for each published release.

```toml
digest = "daily"     # a Digest event once a day with the day before
[[hooks]]
event   = "Report"   # a comment only when something is urgent (below); the rest waits for the digest
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Report"]
[[hooks]]
event   = "Digest"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Digest"]
[[hooks]]
event   = "Ask"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Ask"]
```

- **The daily digest**: one comment a day (never two for the same day: a `<!-- warden-digest DAY -->` marker on the
  token user's comments) with the model spend of the day against the cap, the running version and the self-update
  state (`Warden version v0.2.0 (update: up to date)`), the counts (✅ done, ⛔ failed, ❓ waiting), **Needs you** (open
  asks with their `warden approve` line, then the [kernel](#the-human-only-kernel) PRs with their kernel files, then
  every other open issue and PR labeled `needs-human`, linked), **Also** (lines other parts of Warden add — with the
  experiment log on, the day's 27B line: `27B experiment DAY: tasks 3 · merged 2 · first-push green 50% (1/2) ·
  tool-call errors/call 0.05 (2/40) · idle 6.0 h — experiment-27b.md`, linked), then the day's lines.
- **Release notes**: when Warden tags a release, and when a release is published (by Warden's tag or by you), the
  CHANGELOG.md section of that version, once per version. Only once the digest issue exists.
- **Immediate pings**, only for: security findings (kind `security`/`vuln`, or marked security), a failed release
  (the release workflow failed, or the release act failed) or a rollback (a self-update rolled back, or a release
candidate failed on the canary), budget alerts, and anything waiting for you
  (an ask, a PR labeled `needs-human`). Everything else the report has for you (a red main, a failed work-issue,
  other high findings) goes into the next digest, without a comment. The backoff (`[backoff]`) still applies.
- Every comment ends with `<sub>… · warden VERSION</sub>`; the report header says `warden VERSION` too.

Text in the comments that comes from outside (issue and PR titles, the CHANGELOG built from them) is defused: the
only mention is the owner's. An older "Warden log" issue (label `warden-log`) stays as it is — Warden can't edit or
close issues — and gets no more comments; close it yourself.

## The 27B experiment log

Warden maintains its own repository with a local 27B model (`qwen3.8-27b-local` through LiteLLM, the brain `local`,
working outside its `unavailable` hours) doing all of the work, features included:

`brain_for` names `local` for `work-issue` (no `auto`: nothing is sized away from it), `experiment_log = "on"` (the
pack's default) and `experiment_tz = "Europe/Moscow"` for the log's days and weeks — see [An instance](#an-instance).

Every two or three days the owner and Claude read the published log and tune the prompts, tools, context and skills.

- **Task records.** A task is one issue worked by `work-issue` across ticks. Each act on it — the first attempt
  (phase `new`), the fix round (`fix`), the give-up (`giveup`), the merge (`merge`, by `merge-pr`), a revert
  (`revert`) — appends one line to `<state>/tasks.jsonl` (append-only, `0600`): the issue and PR, the brain and its
  model, ok/refused, a short summary, wall time, tokens in/out and calls (the difference of the brains' usage around
  the act), and the act's own data: size (`small` / `size:S|M|L|XL` labels, else the diff: under 50 changed lines S,
  200 M, 800 L, else XL), files, lines added/deleted, the brain's own counts (`brain.WithStats`: model turns, tool
  calls, calls to an unknown tool, arguments that are not a JSON object, tools that ran and failed, calls repeating
  the one before, the largest prompt, how the last turn ended), CI on the first push (green: merged without a fix round), the fix round,
  and at the merge the comments and reviews people other than Warden left. `experiment_acts` names the acts recorded
  (default `work-issue`, `merge-pr`, `pickup-branch`); an act deferred before any model call (CI running) is not an
  attempt. A merged PR titled `Revert "…"` that undoes a `warden/issue-*` PR (GitHub's revert branch, the quoted
  title, or its number in the title) is recorded once as the task's revert.
- **Failure categories**, several per failure, from the act's summary, log and data: `tool-call` (calls to an
  unknown tool or with arguments that are not JSON), `context-limit` (context length, too many tokens, prompt too
  long, or a prompt that reached 90% of `experiment_context_tokens`, default 262144), `wrong-edit` (CI red after the
  push — every fix round —, local checks red, no change, a protected path), `loop` (`max_steps`, two or more calls
  repeating the one before), `timeout`, `gave-up` (handed to the owner, or the model says it can't), `other`.
- **Idle time is not failure.** Each tick in which the `work-issue` brain may not work — off-hours, its upstream
  unavailable, the budget spent — adds the minutes since the previous tick (at most one wake interval) as an `idle`
  record. Brain acts wait meanwhile; nothing counts as an attempt.
- **Publishing.** The `experiment` observer proposes `publish-log` once a day (in `experiment_tz`) when there are
  records, no `warden/experiment-*` PR is open and today's log isn't out yet. The act renders `experiment_doc`
  (default `docs/experiment-27b.md`, never a protected path): a weekly summary (tasks, merged, first-push green %,
  average attempts and fix rounds, median wall time, tokens, tool-call errors per tool call, reverts, top failure
  categories, idle hours), then each day newest first with its tasks (steps, tool calls and tool-call errors among
  them), its failures with one-line reasons, and its idle hours. Its PR touches only that
  file, so `merge-pr` merges it on green CI; the log PRs never trigger a release. `experiment_log = "off"` stops the
  records and the publishing.
- **Where it is read.** The daily digest carries the day's numbers in one line with a link to the log; the weekly
  roadmap reads the log's weekly summary from `main`.

## An instance

The whole maintainer in one `warden.toml` — every act it may run alone, the quality gates, a canary, the experiment
log and the roadmap:

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
           { act = "publish-log", then = "auto" }, { act = "revert-pr", then = "auto" },
           { act = "roadmap", then = "auto" }, { act = "rfc-accept", then = "auto" }]
brain_for  = { triage = "local", interpret = "local", work-issue = "local", review-pr = "local", roadmap = "local" }

[params]
repo               = "OWNER/REPO"
token_file         = "/secrets/github/token"
owner              = "OWNER"
root               = "/state/repo"            # cloned on first use
max_issues_per_day = "3"
trusted_authors    = ["my-other-agent"]
required_checks    = ["lint", "test (ubuntu-latest)", "test (macos-latest)", "govulncheck", "coverage", "fuzz", "demos (ubuntu-latest)"]
protected          = ["deploy/**"]            # extra paths on top of the human-only kernel (it can't be removed)
review_brain       = ""                       # a second model's review before Warden merges its own PRs ("" = off)
canary             = "warden-canary"          # a fleet instance with [update] channel = "canary" and a beacon
canary_hours       = "24"                     # the rc runs healthy this long on it before vX.Y.Z is tagged
canary_min_ticks   = "6"
canary_stale_hours = "3"                      # an older beacon is a silent canary: no final tag
experiment_log     = "on"
experiment_tz      = "Europe/Moscow"          # the log's days and weeks
roadmap            = "on"
roadmap_day        = "mon"
roadmap_tz         = "Europe/Moscow"
delegate_cmd       = ["some-cli", "task", "create", "--title", "{title}", "--body", "{body}", "--to", "{agent}"]
delegate_labels    = { infra = "Ops" }
delegate_default   = "Coder"
llm_url            = "http://litellm.llm.svc:4000/v1"
llm_model          = "qwen3.8-27b-local"

[brains.local]                                # see Budget and brains
provider     = "openai"
base_url     = "{llm_url}"
model        = "{llm_model}"
api_key_file = "/secrets/llm/key"
free         = true
unavailable  = ["00:30-08:00", "sat", "sun"]
tz           = "Europe/Moscow"

[update]                                      # the maintainer updates itself too (docs/usecases/self-update.md)
mode    = "install"
channel = "stable"

[[hooks]]                                     # the digest issue (above)
event   = "Report"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Report"]
[[hooks]]
event   = "Digest"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Digest"]
[[hooks]]
event   = "Ask"
command = ["warden", "github-note", "--config", "/config/warden.toml", "--event", "Ask"]
```

The canary is another instance (any domain) named `warden-canary` (its `name`), with `[update] channel = "canary"`
and a `beacon_token_file`. Without a fleet, leave `canary` empty and releases are tagged straight from the merged
release PR. Start with `mode = "dry-run"` for a day and read the reports.

## Credentials checklist (the owner creates every credential)

1. **A fine-grained personal access token** scoped to this one repository: Contents read/write, Pull requests
   read/write, Issues read/write, Actions read, Commit statuses read, Metadata read. **No** Workflows, **no**
   Administration. Short expiry; rotate it by replacing the file.
2. **An Anthropic API key** (only if you configure a paid brain) in a dedicated Console workspace with a monthly spend
   limit — the outer backstop behind Warden's own daily cap.
3. **A branch protection rule or ruleset on `main`**: require a pull request, require the CI status checks (the list
   in [Quality gates](#quality-gates), including `coverage` and `fuzz`), block force pushes and deletions. Warden's
   merges then go through the same gate as everyone's.
4. Labels: `warden`, `needs-human`, `kernel`, `warden-pause`, `delegated`, `warden-fixed`, `release`, `revert`,
   `experiment`, `roadmap`, `rfc`, `size:S|M|L`, `warden-digest`, and for a fleet `warden-fleet` and
   `warden-rollback` (GitHub creates missing labels when Warden adds them).

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
