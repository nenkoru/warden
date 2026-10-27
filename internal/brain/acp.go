package brain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/nenkoru/warden/internal/version"
)

// ACPConfig: an external ACP agent (Claude Code via claude-agent-acp, Gemini
// CLI, Codex, pi, ...). One process and one session per Do; the process group
// is killed afterwards.
type ACPConfig struct {
	Command []string // argv of the agent
	Env     []string // added to os.Environ() (later entries win)
	// CleanEnv: the agent gets Env only, nothing of Warden's environment.
	CleanEnv bool
	Cwd      string // the session's working directory (made absolute); WithDir overrides it per call
	// Confine: every path a permission request or fs/* call names must lie
	// inside the session's working directory (not its .git) — checked before Allow.
	Confine bool
	// SessionMeta: session/new _meta (e.g. Claude Code options that keep the
	// owner's settings and MCP servers out).
	SessionMeta map[string]any
	// Allow decides the agent's permission requests and fs/* calls. kind is
	// the ACP tool kind (read, edit, delete, move, execute, fetch, ... ;
	// "other" when the agent sent none), paths are absolute and cleaned. nil
	// denies everything. It may be called concurrently.
	Allow   func(kind string, paths []string) bool
	Timeout time.Duration // whole call; 0 = none beyond ctx
	Log     io.Writer     // agent stderr and decisions; default: discard
	// Wrap optionally rewrites argv and env, e.g. into a sandbox that may
	// write cwd; cleanup (may be nil) runs after the agent is gone.
	Wrap func(argv, env []string, cwd string) (argv2, env2 []string, cleanup func(), err error)
}

type dirKey struct{}

// WithDir: the working directory for the calls made with ctx (a worktree):
// the session's cwd, and with Confine the only place the agent may touch.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, dir)
}

// DirFrom: the directory set by WithDir, or "".
func DirFrom(ctx context.Context) string {
	d, _ := ctx.Value(dirKey{}).(string)
	return d
}

// toolsNote: the tools Warden offers are not forwarded to an ACP agent; it
// is told so, and what happens instead.
const toolsNote = "\n\nNote: the tools named above are not available in this session. Use your own file tools, inside the working directory only. " +
	"Shell commands and network tools are refused; Warden runs the checks itself after you finish."

// ACP returns a Brain that drives an ACP agent.
//
// v1 limitation: the tools passed to Do are NOT forwarded to the agent (no
// MCP servers are offered in session/new); the agent works with its own tools,
// gated by cfg.Allow.
func ACP(cfg ACPConfig) Brain { return &acpBrain{cfg: cfg} }

type acpBrain struct{ cfg ACPConfig }

func (b *acpBrain) Do(ctx context.Context, system, task string, tools []Tool) (string, error) {
	c := b.cfg
	if len(c.Command) == 0 {
		return "", errors.New("brain: acp needs a command")
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	cwd, err := filepath.Abs(or(DirFrom(ctx), or(c.Cwd, ".")))
	if err != nil {
		return "", err
	}
	if len(tools) > 0 {
		task += toolsNote
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	argv, env := c.Command, append(os.Environ(), c.Env...)
	if c.CleanEnv {
		env = append([]string{}, c.Env...)
	}
	if c.Wrap != nil {
		var cleanup func()
		if argv, env, cleanup, err = c.Wrap(argv, env, cwd); err != nil {
			return "", fmt.Errorf("brain: wrap: %w", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stderr, cmd.WaitDelay = cwd, env, c.Log, 2*time.Second
	ownGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("brain: start %s: %w", argv[0], err)
	}
	defer func() {
		stdin.Close()
		killGroup(cmd)
		_ = cmd.Wait() // killed above: its exit status says nothing
	}()

	cl := &acpClient{cwd: cwd, allow: c.Allow, confine: c.Confine, log: c.Log, stats: StatsFrom(ctx)}
	conn := acp.NewClientSideConnection(cl, stdin, stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "warden", Version: version.Version},
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: false,
		},
	}); err != nil {
		return "", fmt.Errorf("brain: acp initialize: %w", err)
	}
	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}, Meta: c.SessionMeta})
	if err != nil {
		return "", fmt.Errorf("brain: acp session/new: %w", err)
	}
	var prompt []acp.ContentBlock
	if system != "" {
		prompt = append(prompt, acp.TextBlock(system))
	}
	resp, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sess.SessionId, Prompt: append(prompt, acp.TextBlock(task))})
	text := cl.text()
	if err != nil {
		return text, fmt.Errorf("brain: acp prompt: %w", err)
	}
	cl.stats.Finish(string(resp.StopReason))
	switch resp.StopReason {
	case acp.StopReasonRefusal:
		return text, errors.New("the agent declined the request")
	case acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		return text, fmt.Errorf("the agent stopped: %s", resp.StopReason)
	case acp.StopReasonCancelled:
		return text, errors.New("the agent turn was canceled")
	}
	return text, nil
}

// acpClient is Warden's side of the connection.
type acpClient struct {
	cwd     string
	allow   func(kind string, paths []string) bool
	confine bool
	log     io.Writer
	stats   *Stats // the agent's tool calls (its own tools): counted, steps unknown

	mu  sync.Mutex
	out strings.Builder
}

var _ acp.Client = (*acpClient)(nil)

func (c *acpClient) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func (c *acpClient) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.cwd, p)
	}
	return filepath.Clean(p)
}

// inside: p is in the session's cwd and not in its .git.
func (c *acpClient) inside(p string) bool {
	r, err := filepath.Rel(c.cwd, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) &&
		r != ".git" && !strings.HasPrefix(r, ".git"+string(filepath.Separator))
}

func (c *acpClient) ok(kind string, paths []string) bool {
	yes := c.allow != nil
	if c.confine {
		yes = yes && len(paths) > 0
		for _, p := range paths {
			yes = yes && c.inside(p)
		}
	}
	yes = yes && c.allow(kind, paths)
	fmt.Fprintf(c.log, "warden brain(acp): %s %v → %v\n", kind, paths, yes)
	return yes
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	if tc := n.Update.ToolCall; tc != nil {
		c.stats.Call(tc.Title, "", CallOK)
	}
	if u := n.Update.ToolCallUpdate; u != nil && u.Status != nil && *u.Status == acp.ToolCallStatusFailed {
		c.stats.failed()
	}
	if ch := n.Update.AgentMessageChunk; ch != nil && ch.Content.Text != nil {
		c.mu.Lock()
		c.out.WriteString(ch.Content.Text.Text)
		c.mu.Unlock()
	}
	return nil
}

func (c *acpClient) RequestPermission(_ context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	kind := "other"
	if r.ToolCall.Kind != nil {
		kind = string(*r.ToolCall.Kind)
	}
	paths := make([]string, 0, len(r.ToolCall.Locations))
	for _, l := range r.ToolCall.Locations {
		paths = append(paths, c.abs(l.Path))
	}
	want := []acp.PermissionOptionKind{acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways}
	if c.ok(kind, paths) {
		want = []acp.PermissionOptionKind{acp.PermissionOptionKindAllowOnce}
	}
	for _, k := range want {
		for _, o := range r.Options {
			if o.Kind == k {
				return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(o.OptionId)}, nil
			}
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (c *acpClient) ReadTextFile(_ context.Context, r acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	p := c.abs(r.Path)
	if !c.ok("read", []string{p}) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("read %s: not allowed", p)
	}
	bs, err := os.ReadFile(p)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	s := string(bs)
	if r.Line != nil || r.Limit != nil { // 1-based line, line count
		lines := strings.Split(s, "\n")
		start := 0
		if r.Line != nil && *r.Line > 0 {
			start = min(*r.Line-1, len(lines))
		}
		end := len(lines)
		if r.Limit != nil && *r.Limit > 0 {
			end = min(start+*r.Limit, end)
		}
		s = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: s}, nil
}

func (c *acpClient) WriteTextFile(_ context.Context, r acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	p := c.abs(r.Path)
	if !c.ok("edit", []string{p}) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("write %s: not allowed", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, os.WriteFile(p, []byte(r.Content), 0o644)
}

// No terminal capability is advertised; these only answer misbehaving agents.
var errNoTerminal = acp.NewMethodNotFound("terminal")

func (*acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNoTerminal
}
func (*acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNoTerminal
}
func (*acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNoTerminal
}
func (*acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNoTerminal
}
func (*acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNoTerminal
}
