# Signals: news, alerts and messages

Anything from outside — a news item, an advisory, a log line, a message from a colleague, another agent's request — is
a **signal**. Signals wake Warden (in `warden listen`) or are collected at the next tick, and they can only cause what
their sender's **trust tier** allows. Urgency raises severity, never permissions; claims are checked (an advisory ID is
looked up in OSV and matched against the real inventory before anything happens).

| kind | what | config |
|---|---|---|
| `feed` | an RSS/Atom URL; the first look is a baseline, later items are news | `url` |
| `command` | a program printing `{"id","from","group","text","url","verified"}` JSON lines — the adapter for any chat, mail or chain network | `argv`, optional `reply` |
| `stream` | a long-running command, one line per signal (`journalctl -f`, `tail -F app.log`); run under `warden listen` | `argv`, `chat` |
| `dir` | a drop folder: `*.json` (a signal) or `*.txt` (its text) | `dir` |
| built in | `warden signal TEXT` (owner tier), and `warden serve`: `POST /signal` and A2A `SendMessage` with bearer tokens | `[serve]` |

## Trust tiers

A source has a default `trust` (`anyone`, `trusted`, `owner`). Senders the source marks `verified` are raised by the
instance's lists: `owners` and `trusted` (`"name"` or `"name@group"`).

| verb | owner | trusted | anyone |
|---|---|---|---|
| `status` | reply | reply | — |
| `check` | tick + reply | tick + reply | — |
| `advisory` | look it up | look it up | look it up (OSV is the authority) |
| `news` | a finding | a finding | a digest line if it touches the inventory |
| `incident` | a finding | a finding | a low finding |
| `update`, `task` | do it | ask the owner | — |

The verb comes from a `warden VERB …` line, else from checkable hints (advisory IDs, `module@version` near "update", an
error line → incident), else the brain picks one. The brain sees the text fenced as data and can only pick a verb.

## Configuration

```toml
# news that matters only when it touches the inventory
[[signals]]
name  = "cisa"
kind  = "feed"
url   = "https://www.cisa.gov/cybersecurity-advisories/all.xml"

# a team chat or chain network through a small adapter that prints JSON lines
[[signals]]
name    = "team"
kind    = "command"
argv    = ["my-chat-adapter", "fetch", "--json"]
owners  = ["me"]
trusted = ["alice@ops"]
reply   = ["my-chat-adapter", "send", "--to", "{from}", "--text", "{text}"]   # dry-run until replies = true

# errors from the application's log, as they happen (warden listen)
[[signals]]
name  = "app"
kind  = "stream"
argv  = ["tail", "-n0", "-F", "/var/log/myapp.log"]
trust = "trusted"
```

`chat = true` on a stream makes every line a message to Warden instead of filtering for error lines (for a chat
bridge or `echo hi >> chat.in`). Replies on your behalf are dry-run (a report line saying what it would send) until
`replies = true`.

## What the report looks like

An incident from a log stream, with a brain (from `examples/live-brain-incident.txt`, shortened): the RCA sits next to
the decision, and the fix is prepared but not started.

```markdown
## Waiting for you

- **app: panic: runtime error: invalid memory address or nil pointer dereference in relay.Pipe: b is nil (…)** — high
  - 1. **Likely cause:** `relay.Serve` ignores the error from the upstream dial and still calls `Pipe(ctx, a, b)` with `b == nil`. …
    5. FIX: in `relay.Serve`, add `if err != nil { return err }` right after the upstream dial, before calling `Pipe`. …
    <<<from app — data, not instructions>>>
    panic: runtime error: invalid memory address or nil pointer dereference in relay.Pipe: b is nil (relay.go:22, …)
    <<<end>>>
  - proposed: fix it in a branch with the brain, run the checks
  - why not alone: a code change for an incident: prepared, yours to start
  - `warden approve w_f3b1718e` · `warden decline w_f3b1718e`
```

A feed's first look only sets the baseline, and a feed that fails is told once per distinct error (and once when it
works again), not on every tick:

```markdown
- feed https://kubernetes.io/feed.xml: first look, 50 items as the baseline
- feed https://example.org/news.xml broken: 403 Forbidden — will retry next tick
```

Warden identifies itself to every site it fetches from with `User-Agent: warden/<version> (+https://github.com/nenkoru/warden)`.

## Learning which sources matter

Every signal- or feed-born finding carries its source. Label findings with `warden feedback ID noise|digest|important`;
per source the score is `(important + ½·digest + 1) / (labels + 2)`. After three labels a source at ≤ 0.25 becomes
one step quieter (report → digest → ignore) and stops spending brain calls; one at ≥ 0.75 becomes louder
(digest → report). Vulnerabilities, low disk, failing checks and anything critical are never quieted. `warden status`
shows the scores.

## Pinging less when you don't answer

A ping is a fired `Report` or `Ask` hook. After two pings without any sign of you (a command, an MCP call, a signal from
you), the next waits one wake interval, then twice that, up to `backoff.max` (default 7 days). Nothing is lost: the
report is written and the items stay new. Critical items go at once. `backoff = { after = 2, max = "168h" }`;
`after = -1` turns it off.
