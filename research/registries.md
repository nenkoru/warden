# Warden: facts about the extension registries (researched 2026-10-27)
Key: [V] = verified by a live request or by reading local source. [D] = taken from the docs at the URL given. [U] = not verified.

## 1. Pi / oh-my-pi
**Upstream pi has moved to `earendil-works/pi`.** The npm package is `@earendil-works/pi-coding-agent` (version 1.2.0 was checked) [V]. Its docs ship in the package (`docs/packages.md`, `extensions.md`).
- **Sources** [V, docs/packages.md]: `pi install npm:@x/pkg@1.0.0` | `git:github.com/x/pkg@v1` | `https://github.com/x/pkg` (treated as git) | `./local`. A versioned npm spec is pinned. A git tag or commit is pinned, and `pi update --extensions` reconciles the checkout without moving the ref. Use `--local`/`-l` to write to `.pi/settings.json`, which is read only after project trust is granted. `pi -e <src>` loads a package for one run only.
- **Discovery**: the npm keyword `pi-package` feeds the gallery at https://pi.dev/packages. Query it directly: `GET https://registry.npmjs.org/-/v1/search?text=keywords:pi-package` returns total=12885 [V]. Optional `pi.image` and `pi.video` fields add gallery previews.
- **Package format**: `package.json` `"pi": {"extensions":[...],"skills":[...],"prompts":[...],"themes":[...]}` (globs and `!excl` allowed). Without that key, pi falls back to the conventional dirs `extensions/ skills/ prompts/ themes/`. The host supplies `@earendil-works/pi-ai`, `pi-agent-core`, `pi-coding-agent`, `pi-tui` and `typebox`; declare these as `peerDependencies: "*"`. Settings can filter a package: `{"source":"npm:x","extensions":["!a.ts"],"skills":[]}`. Identity is the npm name, the git URL without its ref, or the absolute path.
- **Extension**: a TS module loaded by jiti with `export default function (pi: ExtensionAPI)`. API: `pi.on(event)`, `pi.registerTool`, `registerCommand`, `registerShortcut`, `registerFlag`, `sendUserMessage`. Events include `session_start`, `session_shutdown`, `tool_call`, `before_agent_start`, `agent_end`, `agent_settled`, `project_trust`. It runs in-process with full OS permissions.
- **Pi's per-user state** (`~/.pi/agent`) [D]: `settings.json` lists `"packages"` (npm or git specs, unpinned unless a version or ref is given) and `"extensions"` overrides. npm installs live in `npm/{package.json,package-lock.json,node_modules/}`; git installs are full clones in `git/<host>/<owner>/<repo>/`; loose extensions, `skills/<name>/SKILL.md`, `agents/*.md`, `models.json` and `auth.json` sit next to them. Warden does not read this directory.
- **Example manifest** (`pi-mcp-adapter/package.json` from the npm registry): `{"name":"pi-mcp-adapter","version":"5.3.1","keywords":["pi-package",...],"pi":{"extensions":["./index.ts"],"video":"…"},"type":"module","peerDependencies":{"@earendil-works/pi-ai":"^0.84.1 || … ^1.0.0","@earendil-works/pi-tui":"*","typebox":"*"}}`. A package-lock entry gives `resolved: https://registry.npmjs.org/pi-mcp-adapter/-/pi-mcp-adapter-5.3.1.tgz` and `integrity: sha512-…`, so **integrity is already available from package-lock**.
- **oh-my-pi (`omp`, can1357/oh-my-pi)**: a single self-contained binary [V]. Extensions import `@oh-my-pi/pi-coding-agent`, use the same `ExtensionAPI` shape, and declare `package.json` `"omp":{"extensions":[...]}` (falls back to `"pi"`) [D: raw.githubusercontent.com/can1357/oh-my-pi/main/docs/extensions.md]. omp also runs a **Claude-compatible marketplace**. It reads `.omp-plugin/marketplace.json` first and falls back to `.claude-plugin/marketplace.json`. Commands: `omp plugin marketplace add owner/repo` and `omp plugin install [--dry-run] [--scope user|project] name@mkt`. State lives in `~/.omp/plugins/installed_plugins.json`, `omp-plugins.lock.json` and `marketplaces.json`. Sources `github`, `url` and `git-subdir` take `ref`/`sha`. **npm sources are rejected** ("npm plugin sources are not yet supported") [D: docs/marketplace.md].
- **oh-my-pi's per-user state** (`~/.omp`) [D]: `agent/config.yml` (YAML: model roles, extensions, memory and task settings), `agent/mcp.json` in the standard `{"mcpServers":{…}}` shape, and SQLite databases for sessions, models, history and skill descriptions. Marketplace installs are recorded under `~/.omp/plugins/`. Warden does not read this directory.

## 2. MCP Registry (https://registry.modelcontextprotocol.io)
- `GET /v0/servers?search=<substr>&version=latest&limit=N&cursor=<nextCursor>` [V]. Without `version=latest`, the response returns **every version** as a separate entry. `search` is a substring match on `name`. `/v0.1/servers` also returns 200.
- `GET /v0/servers/{urlencoded name}/versions/latest` and `/versions/{ver}` both return 200 [V]. `GET .../versions` lists all versions.
- Response: `{"servers":[{"server":{ServerDetail},"_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","publishedAt","updatedAt","isLatest":true}}}],"metadata":{"nextCursor":"name:ver","count":N}}`.
- **ServerDetail** [V, `static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json`]: `name` (reverse-DNS, `io.github.owner/x`), `description`, `version` (required), plus `title`, `websiteUrl`, `repository{url,source,subfolder,id}`, `icons`, `packages[]`, `remotes[]` and `_meta`.
- **Package**: `registryType` (`npm|pypi|oci|nuget|mcpb`), `registryBaseUrl`, `identifier`, `version`, `fileSha256` (mcpb), `runtimeHint` (`npx|uvx|docker`), `transport{type:stdio|streamable-http|sse, url, headers}`, `runtimeArguments[]`, `packageArguments[]` and `environmentVariables[]`. Each argument is `{type:"named"|"positional", name, value, default, isRequired, isSecret, format, placeholder, choices, valueHint, isRepeated}`. `remotes[]` = `{type:"streamable-http"|"sse", url, headers[]}`.
- Trimmed real entry [V]:
```json
{"name":"io.github.containers/kubernetes-mcp-server","version":"0.0.67","packages":[
 {"registryType":"npm","registryBaseUrl":"https://registry.npmjs.org","identifier":"kubernetes-mcp-server","version":"0.0.67","transport":{"type":"stdio"}},
 {"registryType":"pypi","identifier":"kubernetes-mcp-server","version":"0.0.67","runtimeHint":"uvx","transport":{"type":"stdio"}},
 {"registryType":"oci","identifier":"ghcr.io/containers/kubernetes-mcp-server:v0.0.67","runtimeHint":"docker",
  "transport":{"type":"streamable-http","url":"http://localhost:8080/mcp"},"runtimeArguments":[{"type":"named","name":"-v","placeholder":"~/.kube/config:/kubeconfig:ro","isRequired":true}]}]}
```
- **Integrity** [V]:
  - npm: `GET https://registry.npmjs.org/<pkg>/<ver>` → `dist.integrity` (`sha512-+pAaMnXn…==`), `dist.shasum` and `dist.tarball`.
  - PyPI: `GET https://pypi.org/pypi/<pkg>/<ver>/json` → `urls[].{filename,packagetype,digests.sha256,url}`.
  - OCI: `GET https://ghcr.io/token?scope=repository:<repo>:pull`, then `HEAD /v2/<repo>/manifests/<tag>` with `Accept: application/vnd.oci.image.index.v1+json`. The digest is in header `docker-content-digest: sha256:e47f6ccc…` and should be pinned as `image@sha256:…`.
  - mcpb: `fileSha256`.
  - **Caveat**: the npm `kubernetes-mcp-server` uses per-platform `optionalDependencies` (`kubernetes-mcp-server-darwin-arm64`, …), so the platform package also needs its own integrity pin.

## 3. Agent Skills / Claude Code plugins
- **Spec** [D: https://agentskills.io/specification]: the folder `skill-name/SKILL.md` holds YAML frontmatter, and `scripts/`, `references/`, `assets/` are optional.
  - Required: `name` (1–64 chars, `[a-z0-9-]`, no leading, trailing or double `-`, must equal the dir name) and `description` (1–1024 chars).
  - Optional: `license`, `compatibility` (≤500), `metadata` (string→string map), `allowed-tools` (space-separated, experimental).
  - Validator: `skills-ref validate ./skill` (github.com/agentskills/agentskills).
- **anthropics/skills** [V]: `.claude-plugin/marketplace.json` has `name:"anthropic-agent-skills"` and a plugin entry `{"name":"document-skills","source":"./","strict":false,"skills":["./skills/xlsx","./skills/docx","./skills/pptx","./skills/pdf"]}`.
- **marketplace.json** [D: code.claude.com/docs/en/plugins/marketplace-reference.md]:
  - Required: `name`, `owner{name,email?,url?}` and `plugins[]`. Optional: `metadata.{description,version,pluginRoot}`, `renames`, `forceRemoveDeletedPlugins`.
  - Entry: `name`, `source`, plus `description`, `version`, `category`, `tags`, `strict` (default true), `dependencies` and `defaultEnabled`, and any plugin.json field.
  - **Sources**:
    - `"./path"`
    - `{"source":"github","repo":"o/r","ref":"v2","sha":"<40hex>"}`
    - `{"source":"url","url":"https://….git","ref","sha"}`
    - `{"source":"git-subdir","url","path","ref","sha"}`
    - `{"source":"npm","package","version","registry"}` (install scripts are not run)
    - `{"source":"archive","url":"https://….zip","sha256":"<64hex>"}`
    - `{"source":"command","command","timeout","mode":"copy|link"}`
  - When both are given, `sha` wins over `ref`.
- **plugin.json** (`.claude-plugin/plugin.json`, optional) [D: plugins-reference]: `name` (required, kebab-case), `displayName`, `version` (pins), `description`, `author{name,email,url}`, `homepage`, `repository`, `license`, `keywords`, `dependencies`, `userConfig{key:{type,title,description,sensitive,…}}`, `skills`, `commands`, `agents`, `hooks`, `mcpServers`, `lspServers`, `outputStyles`, `experimental.{themes,monitors}`.
- **Default layout**: `skills/<n>/SKILL.md`, `commands/*.md`, `agents/*.md`, `hooks/hooks.json`, `.mcp.json`, `.lsp.json`, `monitors/monitors.json`, `output-styles/`, `bin/` (added to PATH), `settings.json`.
- **Variables**: `${CLAUDE_PLUGIN_ROOT}`, `${CLAUDE_PLUGIN_DATA}` (`~/.claude/plugins/data/<id>/`), `${CLAUDE_PROJECT_DIR}` and `${user_config.KEY}`. Hooks receive `CLAUDE_PLUGIN_OPTION_<KEY>`.
- **hooks.json** [D: code.claude.com/docs/en/hooks]: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"\"${CLAUDE_PLUGIN_ROOT}\"/check.sh","timeout":600}]}]}}`. Handler types are `command|http|mcp_tool|prompt|agent`.
  - Events: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PostToolUseFailure`, `Notification`, `SubagentStart`, `SubagentStop`, `Stop`, `PreCompact`, `SessionEnd`, and others.
  - Stdin: `session_id`, `transcript_path`, `cwd`, `permission_mode`, `hook_event_name`, plus `tool_name`, `tool_input` and `tool_use_id` on PreToolUse.
  - **Exit 0** means OK, and stdout is parsed if it is a JSON object. **Exit 2** blocks, and stderr becomes the reason. Any other code is a non-blocking error.
  - JSON decision: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow|deny|ask|defer","permissionDecisionReason":"…","updatedInput":{…}}}`.

## 4. Gemini CLI extensions [D: geminicli.com/docs/extensions/reference]
- Note: the site says Gemini CLI was superseded by Antigravity CLI on 2026-06-18.
- `~/.gemini/extensions/<name>/gemini-extension.json` fields: `name`, `version`, `description`, `mcpServers`, `contextFileName` (default GEMINI.md), `excludeTools`, `settings[{name,description,envVar,sensitive}]`, `themes`, `plan.directory`, `migratedTo`.
- Can bundle `commands/*.toml`, `skills/`, `hooks/hooks.json`, `agents/*.md` and `policies/*.toml`.
- Install: `gemini extensions install <git-url|path> [--ref <ref>] [--auto-update] [--pre-release] [--consent]`. Variables: `${extensionPath}`, `${workspacePath}`, `${/}`.

## 5. A2A (a2a-protocol.org)
- **Current spec: 1.0.0** [D: /latest/specification]. Requests carry the header `A2A-Version: 1.0`; an empty value means 0.3.
- Agent card location: `/.well-known/agent-card.json` (the same path as in 0.3).
- Required AgentCard fields: `name`, `description`, `version`, `supportedInterfaces[{url,protocolBinding:"JSONRPC"|"GRPC"|"HTTP+JSON",protocolVersion,tenant?}]`, `capabilities{streaming,pushNotifications,extendedAgentCard}`, `defaultInputModes`, `defaultOutputModes`, `skills`. Also `securitySchemes` and `security`.
- **The method names changed** [V via curl of both specs]:

| Operation | v0.3 | v1.0 |
|---|---|---|
| Send | `message/send` | `SendMessage` |
| Stream | `message/stream` | `SendStreamingMessage` |
| Get task | `tasks/get` | `GetTask` |
| List tasks | `tasks/list` | `ListTasks` |
| Cancel | `tasks/cancel` | `CancelTask` |
| Resubscribe | `tasks/resubscribe` | `SubscribeToTask` |
| Push config | `tasks/pushNotificationConfig/{set,get,list,delete}` | `{Create,Get,List,Delete}TaskPushNotificationConfig` |
| Extended card | `agent/getAuthenticatedExtendedCard` | `GetExtendedAgentCard` |

- Other 1.0 changes: the `kind` discriminator is removed (a Part is one of `text|raw|url|data`, plus `mediaType`, `filename`, `metadata`). Enums are now `ROLE_USER`/`ROLE_AGENT` and `TASK_STATE_{SUBMITTED,WORKING,COMPLETED,FAILED,CANCELED,REJECTED,INPUT_REQUIRED,AUTH_REQUIRED}`.
```json
{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"uuid","role":"ROLE_USER","parts":[{"text":"Hello"}]}}}
{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"task-uuid","contextId":"ctx-uuid","status":{"state":"TASK_STATE_COMPLETED"}}}}
```
- The `params` wrapper key `message` is [U]. It is inferred from the REST example and the `SendMessageRequest` type. The result is `{task}` or `{message}`. `GetTask` params are `{"id":"task-uuid","historyLength"?}` [U].

## 6. Kubernetes MCP servers in the registry (`version=latest`, searches kube/k8s/openshift) [V]
- **io.github.containers/kubernetes-mcp-server 0.0.67**: npm, pypi and oci. This is the reference choice.
  - Read-only in the released 0.0.67: CLI `--read-only` (also `--disable-destructive`, `--toolsets core,config`).
  - **Changing on `main`**: runtime flags are being moved to TOML. Use `--config f.toml` containing `read_only = true` / `toolsets = ["core","config"]`, plus `enabled_tools` and `disabled_tools`.
  - **Warden should write a TOML file and pass `--config`**, which works on both the old and new flag sets. `read_only` exposes only tools with `readOnlyHint=true`.
  - Read tools: `pods_list`, `pods_list_in_namespace`, `pods_get`, `pods_log`, `pods_top`, `events_list`, `resources_list`, `resources_get`, `namespaces_list`, `nodes_top`, `nodes_log`, `nodes_stats_summary`, `configuration_view`, `configuration_contexts_list`, `helm_list`.
  - Write tools: `pods_delete`, `pods_exec`, `pods_run`, `resources_create_or_update`, `resources_delete`, `resources_scale`, `helm_install`, `helm_uninstall`.
  - Multi-cluster adds a `context` argument.
- Others found:

| Server | Packaging |
|---|---|
| `io.github.dockndevai/mcp-kubernetes` 0.3.3 | npm `@dockndevai/mcp-kubernetes` |
| `io.github.your-ko/mcp-k8s-ro` 1.2.1 | oci; read-only by design per its name [U] |
| `io.github.mikhae1/kubeview` 2.3.0 | npm `kubeview-mcp` |
| `io.github.kaneg/k8s-mcp-go` 0.4.6 | mcpb |
| `io.github.apatilgtn/k8scortex-mcp` | npm + oci |
| `io.github.AIops-tools/k8s-aiops` | pypi |
| `io.github.sanjaypsachdev/mcp-server-openshift` | npm + oci |
| `io.github.kubeshark/mcp` | mcpb |
| `io.github.SCGIS-Wales/helm-mcp` | pypi + oci |

  Read-only flags and tool names for these are [U].

## 7. modelcontextprotocol/go-sdk v1.8.0 (go 1.25) [V: ~/go/pkg/mod/.../go-sdk@v1.8.0 README + mcp/*.go]
```go
// client
c := mcp.NewClient(&mcp.Implementation{Name: "warden", Version: "v0.1"}, nil)
cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: exec.Command("kubernetes-mcp-server", "--config", cfg)}, nil)
defer cs.Close()
lt, _ := cs.ListTools(ctx, &mcp.ListToolsParams{})          // lt.Tools []*mcp.Tool, lt.NextCursor; or cs.Tools(ctx,nil) iter.Seq2
for t := range ... { _ = t.Annotations /* *ToolAnnotations{ReadOnlyHint bool, DestructiveHint *bool} */ }
res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "pods_list", Arguments: map[string]any{"namespace": "default"}})
if res.IsError {...}; for _, c := range res.Content { if t, ok := c.(*mcp.TextContent); ok { fmt.Print(t.Text) } } // res.StructuredContent any
// server
type In struct{ Name string `json:"name" jsonschema:"who to greet"` }
type Out struct{ Greeting string `json:"greeting"` }
s := mcp.NewServer(&mcp.Implementation{Name: "warden", Version: "v0.1"}, nil)
mcp.AddTool(s, &mcp.Tool{Name: "greet", Description: "say hi"},
  func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) { return nil, Out{"Hi " + in.Name}, nil })
err = s.Run(ctx, &mcp.StdioTransport{})
```
- `CommandTransport` also has a `TerminateDuration` field (default 5s before SIGTERM). The untyped form is `(*Server).AddTool(t, mcp.ToolHandler)`.

## 8. coder/acp-go-sdk v0.13.5 (`package acp`, ProtocolVersionNumber = 1) [V: example_client_test.go, client_gen.go, types_gen.go]
```go
cmd := exec.Command("some-agent", "--acp"); stdin, _ := cmd.StdinPipe(); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start()
conn := acp.NewClientSideConnection(myClient{}, stdin, stdout)   // conn.Done() <-chan struct{}, conn.SetLogger(*slog.Logger)
_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber,
  ClientCapabilities: acp.ClientCapabilities{Fs: acp.FileSystemCapabilities{ReadTextFile: true}, Terminal: false},
  ClientInfo: &acp.Implementation{Name: "warden", Version: "0.1"}})
sess, _ := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/abs", McpServers: []acp.McpServer{}})
resp, _ := conn.Prompt(ctx, acp.PromptRequest{SessionId: sess.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
// resp.StopReason: acp.StopReasonEndTurn|MaxTokens|MaxTurnRequests|Refusal|Cancelled; conn.Cancel(ctx, acp.CancelNotification{...})
```
- `myClient` must implement `acp.Client`: `ReadTextFile`, `WriteTextFile`, `RequestPermission`, `SessionUpdate`, `CreateTerminal`, `KillTerminal`, `TerminalOutput`, `ReleaseTerminal`, `WaitForTerminalExit`. `UnstableCompleteElicitation` is optional and detected by type assertion.
```go
func (myClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
  u := n.Update
  switch { case u.AgentMessageChunk != nil: if c := u.AgentMessageChunk.Content; c.Text != nil { fmt.Print(c.Text.Text) }
  case u.ToolCall != nil: log(u.ToolCall.Title, u.ToolCall.Status)
  case u.ToolCallUpdate != nil: log(u.ToolCallUpdate.ToolCallId, u.ToolCallUpdate.Status) }
  return nil }
func (myClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
  for _, o := range p.Options { if o.Kind == acp.PermissionOptionKindRejectOnce { // also AllowOnce/AllowAlways/RejectAlways
    return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId}}}, nil } }
  return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil }
```
