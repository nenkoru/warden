package mcpx

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nenkoru/warden/internal/version"
)

// PendingItem is an action waiting for a human decision.
type PendingItem struct {
	ID   string `json:"id"`
	What string `json:"what"`
	Why  string `json:"why"`
}

// Control is what a running Warden exposes to other harnesses.
type Control interface {
	Status() string
	Report() string
	Pending() []PendingItem
	Approve(id string, yes bool) (string, error)
	Signal(from, text string) (string, error)
}

// ReportURI is the resource holding the latest report as Markdown.
const ReportURI = "warden://report/latest"

type (
	noArgs      struct{}
	approveArgs struct {
		ID      string `json:"id" jsonschema:"id of the pending item"`
		Approve bool   `json:"approve" jsonschema:"true to approve, false to decline"`
	}
	signalArgs struct {
		From string `json:"from,omitempty" jsonschema:"who sends the signal"`
		Text string `json:"text" jsonschema:"the news, alert or message"`
	}
)

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// NewServer builds the Warden MCP server around c.
func NewServer(c Control) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "warden", Version: version.Version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "warden_status", Description: "Warden's current state: last tick, findings, pending count."},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return text(c.Status()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "warden_report", Description: "The latest report (Markdown)."},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return text(c.Report()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "warden_pending", Description: "Actions waiting for a human decision, as JSON [{id, what, why}]."},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			p := c.Pending()
			if p == nil {
				p = []PendingItem{}
			}
			b, err := json.Marshal(p)
			return text(string(b)), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "warden_approve", Description: "Approve or decline a pending action by id."},
		func(_ context.Context, _ *mcp.CallToolRequest, a approveArgs) (*mcp.CallToolResult, any, error) {
			out, err := c.Approve(a.ID, a.Approve)
			return text(out), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "warden_signal", Description: "Hand Warden a signal (news, alert, message) to consider on its next tick."},
		func(_ context.Context, _ *mcp.CallToolRequest, a signalArgs) (*mcp.CallToolResult, any, error) {
			out, err := c.Signal(a.From, a.Text)
			return text(out), nil, err
		})
	s.AddResource(&mcp.Resource{URI: ReportURI, Name: "report", Description: "Warden's latest report", MIMEType: "text/markdown"},
		func(_ context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: ReportURI, MIMEType: "text/markdown", Text: c.Report()}}}, nil
		})
	return s
}

// Serve runs Warden as an MCP server over newline-delimited JSON on in/out
// (stdin/stdout when nil) until the client disconnects or ctx ends.
func Serve(ctx context.Context, c Control, in io.Reader, out io.Writer) error {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	rc, ok := in.(io.ReadCloser)
	if !ok || in == os.Stdin {
		rc = io.NopCloser(in)
	}
	wc, ok := out.(io.WriteCloser)
	if !ok || out == os.Stdout {
		wc = nopWriteCloser{out}
	}
	return NewServer(c).Run(ctx, &mcp.IOTransport{Reader: rc, Writer: wc})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
