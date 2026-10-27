package brain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// The test binary doubles as a fake ACP agent when FAKE_ACP_AGENT is set.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_ACP_AGENT"); mode != "" {
		a := &fakeAgent{mode: mode}
		a.conn = acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
		<-a.conn.Done()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeAgent struct {
	mode string
	conn *acp.AgentSideConnection
	cwd  string
	fs   bool
	meta map[string]any
}

func (a *fakeAgent) Initialize(_ context.Context, r acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.fs = r.ClientCapabilities.Fs.ReadTextFile && r.ClientCapabilities.Fs.WriteTextFile && !r.ClientCapabilities.Terminal
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (a *fakeAgent) NewSession(_ context.Context, r acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.cwd, a.meta = r.Cwd, r.Meta
	return acp.NewSessionResponse{SessionId: "s1"}, nil
}

func (a *fakeAgent) Prompt(ctx context.Context, r acp.PromptRequest) (acp.PromptResponse, error) {
	if a.mode == "hang" {
		_ = os.WriteFile(filepath.Join(a.cwd, "pid"), fmt.Appendf(nil, "%d", os.Getpid()), 0o644)
		select {}
	}
	say := func(s string) {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: r.SessionId, Update: acp.UpdateAgentMessageText(s)})
	}
	ask := func(path string) string {
		edit := acp.ToolKindEdit
		resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: r.SessionId,
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "c", Kind: &edit, Locations: []acp.ToolCallLocation{{Path: path}}},
			Options: []acp.PermissionOption{
				{OptionId: "yes", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "always", Name: "Always", Kind: acp.PermissionOptionKindAllowAlways},
				{OptionId: "no", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil || resp.Outcome.Selected == nil {
			return fmt.Sprint("err", err)
		}
		return string(resp.Outcome.Selected.OptionId)
	}
	write := func(path string) bool {
		_, err := a.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{SessionId: r.SessionId, Path: path, Content: "a\nb\nc"})
		return err == nil
	}
	say("hello ")
	inside, outside := filepath.Join(a.cwd, "sub", "in.txt"), filepath.Join(a.cwd, "..", "out.txt")
	report := fmt.Sprintf("blocks=%d fs=%v in=%s out=%s write-in=%v write-out=%v", len(r.Prompt), a.fs, ask(inside), ask(outside), write(inside), write(outside))
	line := 2
	got, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{SessionId: r.SessionId, Path: inside, Line: &line, Limit: &line})
	report += fmt.Sprintf(" read=%q/%v", got.Content, err == nil)
	if a.mode == "confine" {
		_, home := os.LookupEnv("HOME")
		gitw := write(filepath.Join(a.cwd, ".git", "config"))
		report += fmt.Sprintf(" meta=%v home=%v git=%v note=%v", a.meta["k"], home, gitw, strings.Contains(r.Prompt[len(r.Prompt)-1].Text.Text, "not available in this session"))
	}
	say(report)
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *fakeAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *fakeAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *fakeAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *fakeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *fakeAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *fakeAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *fakeAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *fakeAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

// inside: an allow rule like the action floors: edits/reads inside dir only.
func inside(dir string, seen *[]string) func(string, []string) bool {
	return func(kind string, paths []string) bool {
		*seen = append(*seen, kind+":"+strings.Join(paths, ","))
		if kind != "edit" && kind != "read" || len(paths) == 0 {
			return false
		}
		for _, p := range paths {
			if r, err := filepath.Rel(dir, p); err != nil || r == ".." || strings.HasPrefix(r, "../") {
				return false
			}
		}
		return true
	}
}

func TestACP(t *testing.T) {
	dir := t.TempDir()
	var seen []string
	var wrapped bool
	out, err := ACP(ACPConfig{
		Command: []string{os.Args[0]},
		Env:     []string{"FAKE_ACP_AGENT=ok"},
		Cwd:     dir,
		Allow:   inside(dir, &seen),
		Timeout: 30 * time.Second,
		Wrap: func(argv, env []string, cwd string) ([]string, []string, func(), error) {
			wrapped = cwd == dir
			return argv, env, nil, nil
		},
	}).Do(context.Background(), "sys", "task", []Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	want := `hello blocks=2 fs=true in=yes out=no write-in=true write-out=false read="b\nc"/true`
	if out != want || !wrapped {
		t.Fatalf("got  %q\nwant %q (wrapped=%v)", out, want, wrapped)
	}
	if bs, _ := os.ReadFile(filepath.Join(dir, "sub", "in.txt")); string(bs) != "a\nb\nc" {
		t.Errorf("inside file: %q", bs)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "out.txt")); err == nil {
		t.Error("outside file was written")
	}
	// the outside path reaches Allow cleaned (no "..")
	if want := "edit:" + filepath.Join(filepath.Dir(dir), "out.txt"); !strings.Contains(strings.Join(seen, " "), want) {
		t.Errorf("Allow saw %v, want %s", seen, want)
	}
}

// WithDir moves the session into the worktree; Confine refuses anything
// outside it (and its .git) whatever Allow says; CleanEnv passes Env only.
func TestACPConfineWithDir(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	cleaned := false
	out, err := ACP(ACPConfig{
		Command: []string{os.Args[0]}, Env: []string{"FAKE_ACP_AGENT=confine"}, CleanEnv: true,
		Cwd: elsewhere, Confine: true, SessionMeta: map[string]any{"k": "v"}, Timeout: 30 * time.Second,
		Allow: func(string, []string) bool { return true },
		Wrap: func(argv, env []string, cwd string) ([]string, []string, func(), error) {
			return argv, env, func() { cleaned = true }, nil
		},
	}).Do(WithDir(context.Background(), dir), "sys", "task", []Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	want := `hello blocks=2 fs=true in=yes out=no write-in=true write-out=false read="b\nc"/true meta=v home=false git=false note=true`
	if out != want || !cleaned {
		t.Fatalf("got  %q\nwant %q (cleanup ran: %v)", out, want, cleaned)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "in.txt")); err != nil {
		t.Error("the session did not run in the WithDir directory")
	}
}

func TestACPTimeoutKills(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	_, err := ACP(ACPConfig{Command: []string{os.Args[0]}, Env: []string{"FAKE_ACP_AGENT=hang"}, Cwd: dir, Timeout: time.Second}).
		Do(context.Background(), "", "task", nil)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	bs, _ := os.ReadFile(filepath.Join(dir, "pid"))
	var pid int
	_, _ = fmt.Sscan(string(bs), &pid)
	if pid == 0 {
		t.Fatal("the agent never ran")
	}
	if p, _ := os.FindProcess(pid); p.Signal(syscall.Signal(0)) == nil {
		t.Errorf("agent %d still alive", pid)
	}
}

func TestACPNoCommand(t *testing.T) {
	if _, err := ACP(ACPConfig{}).Do(context.Background(), "", "q", nil); err == nil {
		t.Error("want an error")
	}
}
