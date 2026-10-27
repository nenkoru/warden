package mcpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The test binary doubles as a fake stdio MCP server when MCPX_FAKE=1.
func TestMain(m *testing.M) {
	if os.Getenv("MCPX_FAKE") == "1" {
		if err := fakeServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return text(in.Text), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "events"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return text(`[{"type":"Warning","reason":"BackOff"}]`), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "env"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return text(os.Getenv("FAKE_VAR") + "|" + os.Getenv("WARDEN_SECRET") + "|" + os.Getenv("WRAPPED")), nil, nil
		})
	type out struct {
		N int `json:"n"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "structured"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, out, error) {
			return &mcp.CallToolResult{}, out{N: 7}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fail"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("boom")
		})
	s.AddResource(&mcp.Resource{URI: "fake://hello", Name: "hello"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "fake://hello", Text: "hello world"}}}, nil
		})
	return s
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func call(t *testing.T, m *Manager, server, tool string, args map[string]any) (string, bool) {
	t.Helper()
	out, isErr, err := m.Call(ctx(t), server, tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out, isErr
}

func TestStdio(t *testing.T) {
	t.Setenv("WARDEN_SECRET", "s3cret")
	m := NewManager([]Server{
		{Name: "fake", Command: []string{os.Args[0]}, Env: map[string]string{"MCPX_FAKE": "1", "FAKE_VAR": "x"}},
		{Name: "wrapped", Command: []string{os.Args[0]}, Env: map[string]string{"MCPX_FAKE": "1"},
			Wrap: func(argv, env []string) ([]string, []string, error) { return argv, append(env, "WRAPPED=yes"), nil }},
	})
	defer m.Close()

	if out, isErr := call(t, m, "fake", "echo", map[string]any{"text": "hi"}); out != "hi" || isErr {
		t.Errorf("echo = %q %v", out, isErr)
	}
	if out, _ := call(t, m, "fake", "events", nil); !strings.Contains(out, `"BackOff"`) {
		t.Errorf("events = %q", out)
	}
	if out, _ := call(t, m, "fake", "env", nil); out != "x||" {
		t.Errorf("env = %q, want x|| (secret must not leak)", out)
	}
	if out, _ := call(t, m, "wrapped", "env", nil); out != "||yes" {
		t.Errorf("wrapped env = %q", out)
	}
	if out, _ := call(t, m, "fake", "structured", nil); out != `{"n":7}` {
		t.Errorf("structured = %q", out)
	}
	if out, isErr := call(t, m, "fake", "fail", nil); !isErr || !strings.Contains(out, "boom") {
		t.Errorf("fail = %q %v", out, isErr)
	}

	tools, err := m.Tools(ctx(t), "fake")
	if err != nil {
		t.Fatal(err)
	}
	var echo *Tool
	for i := range tools {
		if tools[i].Name == "echo" {
			echo = &tools[i]
		}
	}
	if len(tools) != 5 || echo == nil || echo.Description != "echoes" || !strings.Contains(string(echo.Schema), `"text"`) {
		t.Errorf("tools = %+v", tools)
	}

	if out, err := m.ReadResource(ctx(t), "fake", "fake://hello"); err != nil || out != "hello world" {
		t.Errorf("resource = %q %v", out, err)
	}
	if _, _, err := m.Call(ctx(t), "nope", "echo", nil); err == nil {
		t.Error("unknown server: want error")
	}
}

func TestHTTP(t *testing.T) {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer() }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	m := NewManager([]Server{{Name: "web", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer tok"}}})
	defer m.Close()
	if out, _ := call(t, m, "web", "echo", map[string]any{"text": "over http"}); out != "over http" {
		t.Errorf("echo = %q", out)
	}
	if out, _ := call(t, m, "web", "events", nil); !strings.Contains(out, "Warning") {
		t.Errorf("events = %q", out)
	}
}

type fakeControl struct{ signals []string }

func (f *fakeControl) Status() string { return "ok: 2 findings" }
func (f *fakeControl) Report() string { return "# Report\nall good" }
func (f *fakeControl) Pending() []PendingItem {
	return []PendingItem{{ID: "a1", What: "bump nginx", Why: "CVE"}}
}
func (f *fakeControl) Approve(id string, yes bool) (string, error) {
	if id != "a1" {
		return "", errors.New("no such item " + id)
	}
	if yes {
		return "approved " + id, nil
	}
	return "declined " + id, nil
}
func (f *fakeControl) Signal(from, text string) (string, error) {
	f.signals = append(f.signals, from+":"+text)
	return "noted", nil
}

func TestServe(t *testing.T) {
	c := &fakeControl{}
	toServer, clientOut := io.Pipe()
	clientIn, fromServer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx(t), c, toServer, fromServer) }()

	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cl.Connect(ctx(t), &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	callText := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx(t), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}

	if out, _ := callText("warden_status", nil); out != "ok: 2 findings" {
		t.Errorf("status = %q", out)
	}
	if out, _ := callText("warden_pending", nil); !strings.Contains(out, `"id":"a1"`) {
		t.Errorf("pending = %q", out)
	}
	if out, isErr := callText("warden_approve", map[string]any{"id": "a1", "approve": true}); out != "approved a1" || isErr {
		t.Errorf("approve = %q %v", out, isErr)
	}
	if out, _ := callText("warden_approve", map[string]any{"id": "a1", "approve": false}); out != "declined a1" {
		t.Errorf("decline = %q", out)
	}
	if out, isErr := callText("warden_approve", map[string]any{"id": "zz", "approve": true}); !isErr || !strings.Contains(out, "no such item") {
		t.Errorf("approve unknown = %q %v", out, isErr)
	}
	if out, _ := callText("warden_signal", map[string]any{"text": "nginx 1.27.1 out"}); out != "noted" || len(c.signals) != 1 || c.signals[0] != ":nginx 1.27.1 out" {
		t.Errorf("signal = %q %v", out, c.signals)
	}

	res, err := cs.ReadResource(ctx(t), &mcp.ReadResourceParams{URI: ReportURI})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Contents[0]; got.Text != "# Report\nall good" || got.MIMEType != "text/markdown" {
		t.Errorf("resource = %+v", got)
	}

	cs.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Serve did not return after client closed")
	}
}
