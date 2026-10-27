# Writing your own domain pack

A pack is a folder:

```
packs/my-domain/
  domain.toml          what to observe, what may change, how to verify, where news comes from
  skills/              optional Agent Skills (SKILL.md folders) for the brain
  .mcp.json            optional MCP servers, in the usual shape
```

`warden init --pack ./packs/my-domain --config my.warden.toml` writes an instance for it (in dry-run), and
`warden packs` lists packs found under `WARDEN_PACKS` or any `packs/` folder above the config, the working directory or
the binary. Nothing in Warden's code knows about your domain; if something can be observed by a command, an MCP tool
or an HTTP API, it can be a pack.

## A small example: a Postgres database

```toml
# packs/postgres/domain.toml
name        = "postgres"
description = "A Postgres database: connections, replication lag, long transactions; proposes, never changes"
params      = { dsn = "postgres:///app", max_lag_s = "30" }

[[observe]]
name     = "long-transactions"           # psql prints JSON lines in Warden's native Finding shape
run      = { cmd = ["psql", "{dsn}", "-At", "-c", "select json_build_object('key', 'xact:'||pid, 'kind', 'incident', 'severity', 'medium', 'subject', 'pid '||pid, 'title', 'transaction open for '||(now()-xact_start)::text) from pg_stat_activity where now()-xact_start > interval '15 minutes'"] }
fix      = "propose"

[[observe]]
name     = "replication"                 # any JSON: map turns items into findings
run      = { cmd = ["sh", "-c", "psql '{dsn}' -At -c \"select coalesce(json_agg(r), '[]') from (select application_name, extract(epoch from replay_lag)::int as lag from pg_stat_replication) r\""] }
map      = { items = "[]", key = "{application_name}", title = "replica {application_name} lags {lag}s", subject = "{application_name}" }
kind     = "incident"
severity = "high"
fix      = "propose"

[[inventory]]
name = "server"
run  = { cmd = ["psql", "{dsn}", "-At", "-c", "show server_version"] }
map  = { lines = '^(?P<v>[0-9.]+)', component = "postgresql", version = "{v}" }

[feeds]
rss = ["https://www.postgresql.org/news.rss"]

[[act]]
name   = "propose"                       # a proposal file (and the brain's RCA) for the human
run    = { builtin = "propose" }
floors = { reversible = true }

[[act]]
name   = "cancel-query"
run    = { cmd = ["psql", "{dsn}", "-c", "select pg_cancel_backend({pid})"] }
floors = { grant = "pg.cancel", always_ask = true, max_per_day = 5 }

[[check]]
name = "reachable"
run  = { cmd = ["pg_isready", "-d", "{dsn}"] }
```

## The pieces

**Steps.** `observe`, `inventory`, `act` and `check` steps each have a `run` of one kind:

- `cmd` — argv with `{param}` placeholders (params come from the pack's defaults, overridden by the instance);
- `mcp` + `tool` + `args` — a tool on an MCP server the pack declares under `[mcp]`;
- `builtin` — Go code inside Warden (`lockfiles`, `homebrew`, `disk`, `caches`, `health`, `go-bump`, `npm-bump`,
  `pip-bump`, `tool-clean`, `propose`, …; see DESIGN.md §1).

**Output.** A step prints either Finding JSON lines
(`{"key","kind","severity","subject","title","detail","fix":{"act","params"}}`) or anything else plus a `map`:

- `items` — a path with `[]` flattening (`items[].status.containerStatuses[]`);
- `where` — filters (`{ type = "Warning" }`, dotted paths allowed);
- `key`, `title`, `detail`, `subject`, `severity` — `{dotted.path}` templates;
- `lines` — a regex with named groups for plain text;
- for inventory: `component` (`"{image}"` splits `nginx:1.25.3`), `version`, `ecosystem` (an OSV ecosystem name makes
  `[feeds] osv = true` work for it).

When neither fits, `interpret = true` lets the brain turn the raw text into findings (it sees the text fenced as data).

**Floors** belong to each act and are enforced by Warden, never by the model:

| floor | effect |
|---|---|
| `reversible` | the policy may let it run alone |
| `always_ask` | the human always decides |
| `grant = "x"` | report-only unless the instance lists `x` in `grants` |
| `max_per_day` | a daily limit (default 20) |
| `dry_run_only` | never executes |
| `where = "worktree"` | runs in a fresh git worktree on a new branch |
| `sandbox = true` + `net = [hosts]` | runs in the OS sandbox with an egress allowlist |

**Feeds.** `[feeds]` takes `osv = true`, `releases = ["owner/repo"]`, `rss = [urls]`, and `fix = { Ecosystem = "act" }`
to say which act fixes an advisory.

**Extensions.** `extensions = ["mcp:…@version", "skill:…@sha"]` names what the pack needs from registries; the instance
pins them in `warden.lock` with `warden add`.

## Checklist

- Start read-only: observers plus `propose`. Add acts that change things only with floors (`grant`, `always_ask`,
  `max_per_day`) and a `check` that proves the change worked.
- Give every finding a stable `key`: it is how Warden knows a finding is not new.
- Keep severities honest: `high`/`critical` ping, `low`/`info` go to the digest.
- Run `warden tick` in dry-run and read `.warden/reports/latest.md` before you set `mode = "act"`.
- Shipping it in this repository? See [CONTRIBUTING.md](../../CONTRIBUTING.md#adding-a-pack).
