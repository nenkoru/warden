# Warden internals — package APIs

This was the build plan for v1; it is kept as a map of the packages. Where it and the code disagree, the code wins.

Module `github.com/nenkoru/warden` (go 1.26). Only deps:

- `github.com/pelletier/go-toml/v2 v2.4.3`
- `github.com/modelcontextprotocol/go-sdk v1.8.0`
- `github.com/coder/acp-go-sdk v0.13.5`

All three are older than two weeks. Each package has tests (`go test ./...`, offline: fakes and httptest only).

Conventions:

- Exported API exactly as below; anything else unexported.
- Errors wrapped with context.
- Only stdlib beyond the three deps.
- No package imports `internal/core` (core imports them).

## internal/policy — may Warden act alone?

```go
type Floors struct {
    Reversible bool   `toml:"reversible"`
    AlwaysAsk  bool   `toml:"always_ask"`
    Grant      string `toml:"grant"`        // needs this in Config.Grants, else report-only
    MaxPerDay  int    `toml:"max_per_day"`  // 0 = default 20
    DryRunOnly bool   `toml:"dry_run_only"`
    Where      string `toml:"where"`        // "", "worktree"
    Sandbox    bool   `toml:"sandbox"`
    Net        []string `toml:"net"`        // egress allowlist when sandboxed
}
type Rule struct { Act string `toml:"act"`; Target string `toml:"target"`; Then string `toml:"then"` } // then: auto|ask|deny; Target "" = any, glob ok
type Config struct { DryRun bool; Grants []string; Rules []Rule; LearnAfter int /*default 3, min 2*/; DailyAuto int /*default 50*/ }
type Action struct { Act, Target, What string; Params map[string]string; Floors Floors }
type Verdict struct { Outcome string /*auto|ask|deny|report*/; Would string /*dry-run: what it would do*/; DryRun bool; Floor string /*the floor that decided, if any*/; Reasons []string }
func Open(cfg Config, stateDir string) (*Engine, error)   // state: stateDir/policy.json (atomic writes), log: stateDir/policy.jsonl
func (e *Engine) Decide(a Action) Verdict
func (e *Engine) Learn(a Action, approved bool)           // K approvals in a row of Act+Target → auto; a decline pins to ask
func (e *Engine) Status() Status                          // rules, learned keys, today's count
```

Order:

1. deny rules → deny
2. floors: DryRunOnly → report; Grant missing → report; AlwaysAsk → ask
3. ask rules / pinned → ask
4. auto rule or learned streak ≥ LearnAfter → candidate auto, else ask ("learning: n of K")
5. velocity: MaxPerDay per act, DailyAuto overall → ask
6. Config.DryRun → Outcome "ask", Would = what it would have done, DryRun = true

All reasons are in English, short.

## internal/box — run a command contained

```go
type Policy struct { Name string; ReadWrite, ReadOnly []string; Net []string /*hosts; nil = no network*/ }
type Result struct { Output string; Runner string /*sandbox-exec|bwrap|none*/; Refused []string /*egress refusals*/ }
func Run(ctx context.Context, runner string /*auto|sandbox-exec|bwrap|none*/, p Policy, dir string, argv []string, env []string) (Result, error)
func Available() []string
```

- macOS: generated deny-by-default Seatbelt profile. It allows reads everywhere except listed secrets dirs (`~/.ssh`,
  `~/.aws`, `~/.gnupg`, keychains), writes only to ReadWrite and the temp dir, process exec, and network only to the
  proxy's loopback port.
- Linux: bwrap with read-only binds and RW binds, `--unshare-net` when Net is empty; otherwise the proxy socket.
- `auto`: the best available. A named runner that is missing is an error (never a silent fallback).
- Egress proxy: an HTTP CONNECT + plain-HTTP proxy on 127.0.0.1:0 with a host allowlist (exact or `*.suffix`). It
  sets `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` / `NO_PROXY` and records refusals.

## internal/hooks — Claude Code-shaped hooks

```go
type Hook struct { Event string `toml:"event"`; Matcher string `toml:"matcher"`; Command []string `toml:"command"`; Timeout string `toml:"timeout"`; From string `toml:"-"` /*extension that brought it*/ }
type Outcome struct { Block bool; Reason string; Ran []string; Errors []string }
func Fire(ctx context.Context, hooks []Hook, event, subject string, payload any) Outcome
```

- Events: Wake, Finding, PreAction, PostAction, Ask, Report, Signal.
- Matcher is a regex on subject; empty matches all.
- Payload JSON goes on stdin. `{title}`, `{subject}`, `{event}` in argv are replaced from the payload's top-level
  string fields.
- Exit 2 or stdout `{"decision":"block","reason":…}` → Block (only meaningful for PreAction).

## internal/skills — Agent Skills

```go
type Skill struct { Name, Description, Dir, Body string; Meta map[string]string }
func Load(dirs ...string) ([]Skill, error)    // each dir: a skills root (subfolders with SKILL.md) or a single skill folder
func Index(sk []Skill) string                 // "- name: description" lines for a system prompt
```

## internal/mcpx — MCP client manager + Warden as an MCP server

```go
type Server struct { Name string; Command []string; Env map[string]string; URL string /*streamable HTTP*/; Sandbox *box.Policy }
type Manager struct{ … }
func NewManager(servers []Server) *Manager
func (m *Manager) Call(ctx context.Context, server, tool string, args map[string]any) (text string, isError bool, err error) // starts lazily, keeps the session for the tick
func (m *Manager) Tools(ctx context.Context, server string) ([]Tool, error)   // Tool{Name, Description string; Schema json.RawMessage}
func (m *Manager) Close()
// server side:
type Control interface { Status() string; Report() string; Pending() []PendingItem; Approve(id string, yes bool) (string, error); Signal(from, text string) (string, error) }
func Serve(ctx context.Context, c Control) error   // stdio MCP server: tools warden_status, warden_report, warden_pending, warden_approve, warden_signal; resource warden://report/latest
```

Sandboxed servers are started through `box`, same policy.

## internal/a2a — A2A server + client

```go
type Card struct { … }   // per current spec
func Handler(card Card, tokens map[string]string /*bearer → sender*/, onMessage func(ctx context.Context, from, text string) (reply string, err error)) http.Handler
// serves /.well-known/agent-card.json and JSON-RPC message send / task get (method names per the current spec)
func Send(ctx context.Context, url, token, text string) (reply string, err error)
```

## internal/registry — extensions from existing registries

```go
type Ref struct { Kind string /*mcp|skill|plugin|pi*/; Name, Version, Source string }   // parsed from "mcp:io.github.x/y@1.2.3", "skill:github.com/anthropics/skills//pdf@<sha>", "plugin:name@marketplace", "pi:npm-package@1.0.0"
type Locked struct { Ref Ref; Resolved string /*tarball URL / git commit*/; Integrity string /*sha512-…|sha256:…*/; Path string /*on disk*/; Server *mcpx.Server /*for mcp*/; Skills []string; Hooks []hooks.Hook }
func Parse(s string) (Ref, error)
func Resolve(ctx context.Context, r Ref, cacheDir string) (Locked, error)  // download, verify, unpack into cacheDir
func Verify(l Locked) error                                                 // bytes on disk still match the lock
type Lock struct { Entries []Locked }  // warden.lock (TOML)
func ReadLock(path string) (Lock, error); func (l Lock) Write(path string) error
```

- mcp: MCP Registry API → package (npm/pypi/oci) + version, with integrity from npm/PyPI/OCI. Server: `npx -y pkg@ver`
  / `uvx pkg==ver` / `docker run --rm -i image@sha256:…`.
- skill / plugin: git (`git clone --filter=blob:none`, checkout the SHA), tree hash.
- pi: npm tarball + integrity; take `skills/` and prompts; extensions are not run, only noted.

## internal/brain — the model (optional)

```go
type Tool struct { Name, Description string; Schema map[string]any; Run func(ctx context.Context, args map[string]any) (string, bool) }
type Brain interface { Do(ctx context.Context, system, task string, tools []Tool) (string, error) } // tools nil = plain answer
func Endpoint(cfg EndpointConfig) Brain   // anthropic | openai compatible (ported loop)
func ACP(cfg ACPConfig) Brain             // acp-go-sdk: start the agent, one session per call, kill after; permission requests → cfg.Allow(toolKind, paths) bool
```

## internal/domain — the manifest

```go
type Domain struct { Name, Description string; Params map[string]string; Observe, Inventory, Act, Check []Step; Feeds Feeds; MCP map[string]MCPRef; Dir string }
type Step struct { Name string; Run Run; Map *Map; Interpret bool; Floors policy.Floors; Every string }
type Run struct { Cmd []string; MCP, Tool string; Args map[string]any; Builtin string; With map[string]any }
func Load(dir string, params map[string]string) (*Domain, error)
func (m Map) Findings(raw []byte, params map[string]string) ([]Finding, error); func (m Map) Components(raw []byte) ([]Component, error)
```

## internal/core

The loop, state, triage, improve, signals, report, listen, units, CLI glue.

## Order of work

1. policy, box, hooks, skills
2. mcpx, a2a, registry, brain (needs `research/registries.md`)
3. domain + core + builtin + cmd + packs (go-project, macos, k8s-namespace)
4. demos and docs
