// Package mcpx is Warden's MCP layer: a client manager for the servers a domain
// declares, and Warden itself exposed as an MCP server.
package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nenkoru/warden/internal/version"
)

// Server describes one MCP server: a stdio Command or a streamable HTTP URL.
type Server struct {
	Name    string
	Command []string
	Env     map[string]string
	URL     string // streamable HTTP endpoint
	Headers map[string]string
	// Wrap, if set, rewrites the command line and environment before start (sandboxing).
	Wrap func(argv, env []string) (argv2, env2 []string, err error)
}

// Tool is a tool a server offers.
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
}

// inheritedEnv is all a stdio server gets from Warden's own environment.
var inheritedEnv = []string{"PATH", "HOME", "LANG", "TMPDIR", "USER"}

// Manager connects to servers lazily and keeps one session per server until Close.
type Manager struct {
	client  *mcp.Client
	servers map[string]Server
	mu      sync.Mutex
	conns   map[string]*conn
}

type conn struct {
	mu sync.Mutex
	cs *mcp.ClientSession
}

func NewManager(servers []Server) *Manager {
	m := &Manager{
		client:  mcp.NewClient(&mcp.Implementation{Name: "warden", Version: version.Version}, nil),
		servers: map[string]Server{},
		conns:   map[string]*conn{},
	}
	for _, s := range servers {
		m.servers[s.Name] = s
	}
	return m
}

func (m *Manager) session(ctx context.Context, name string) (*mcp.ClientSession, error) {
	srv, ok := m.servers[name]
	if !ok {
		return nil, fmt.Errorf("mcp: unknown server %q", name)
	}
	m.mu.Lock()
	c := m.conns[name]
	if c == nil {
		c = &conn{}
		m.conns[name] = c
	}
	m.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs != nil {
		return c.cs, nil
	}
	t, err := transport(srv)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	cs, err := m.client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: connect: %w", name, err)
	}
	c.cs = cs
	return cs, nil
}

// drop forgets a dead session so the next call reconnects.
func (m *Manager) drop(name string, cs *mcp.ClientSession, err error) {
	if !errors.Is(err, mcp.ErrConnectionClosed) && !errors.Is(err, mcp.ErrSessionMissing) {
		return
	}
	m.mu.Lock()
	c := m.conns[name]
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.cs == cs {
		c.cs = nil
		cs.Close()
	}
	c.mu.Unlock()
}

func transport(s Server) (mcp.Transport, error) {
	if s.URL != "" {
		t := &mcp.StreamableClientTransport{Endpoint: s.URL}
		if len(s.Headers) > 0 {
			t.HTTPClient = &http.Client{Transport: headerRT{s.Headers, http.DefaultTransport}}
		}
		return t, nil
	}
	if len(s.Command) == 0 {
		return nil, errors.New("neither command nor url")
	}
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, k+"="+s.Env[k])
	}
	argv := slices.Clone(s.Command)
	if s.Wrap != nil {
		var err error
		if argv, env, err = s.Wrap(argv, env); err != nil {
			return nil, fmt.Errorf("wrap: %w", err)
		}
	}
	// Not CommandContext: the process outlives the call that started it.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return &mcp.CommandTransport{Command: cmd}, nil
}

type headerRT struct {
	h    map[string]string
	next http.RoundTripper
}

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range rt.h {
		r.Header.Set(k, v)
	}
	return rt.next.RoundTrip(r)
}

// Call runs a tool and returns its text content. A tool-level failure is
// isError=true with err nil; err is for transport and protocol failures.
func (m *Manager) Call(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	cs, err := m.session(ctx, server)
	if err != nil {
		return "", false, err
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		m.drop(server, cs, err)
		return "", false, fmt.Errorf("mcp %s/%s: %w", server, tool, err)
	}
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		case *mcp.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", res.IsError, err
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

// Tools lists every tool the server offers.
func (m *Manager) Tools(ctx context.Context, server string) ([]Tool, error) {
	cs, err := m.session(ctx, server)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			m.drop(server, cs, err)
			return nil, fmt.Errorf("mcp %s: list tools: %w", server, err)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, Schema: schema})
	}
	return out, nil
}

// ReadResource returns the text of a resource; binary parts are summarized.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (string, error) {
	cs, err := m.session(ctx, server)
	if err != nil {
		return "", err
	}
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		m.drop(server, cs, err)
		return "", fmt.Errorf("mcp %s: read %s: %w", server, uri, err)
	}
	var parts []string
	for _, c := range res.Contents {
		if c.Text != "" || len(c.Blob) == 0 {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%d bytes %s]", len(c.Blob), c.MIMEType))
		}
	}
	return strings.Join(parts, "\n"), nil
}

// Close ends every session (stdio servers get stdin closed, then SIGTERM).
func (m *Manager) Close() {
	m.mu.Lock()
	conns := m.conns
	m.conns = map[string]*conn{}
	m.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		if c.cs != nil {
			c.cs.Close()
			c.cs = nil
		}
		c.mu.Unlock()
	}
}
