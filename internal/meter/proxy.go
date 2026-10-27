package meter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proxy is a local metering reverse proxy for an Anthropic-compatible API.
// Point an agent at it (ANTHROPIC_BASE_URL=http://<Addr>) and every
// /v1/messages call is checked against the cap first and recorded after.
type Proxy struct {
	Addr string // 127.0.0.1:port
	srv  *http.Server
	ln   net.Listener
}

const (
	maxRequest = 64 << 20 // a metered request body we're willing to hold
	maxJSON    = 32 << 20 // a non-streaming response we parse for usage
	maxLine    = 4 << 20  // one SSE line
)

type reqInfoKey struct{}

type reqInfo struct{ model string }

// StartProxy listens on 127.0.0.1:0 and forwards every request to upstream
// (e.g. https://api.anthropic.com) with its method, path, query and headers.
// Only POST /v1/messages is metered; everything else passes through.
func (l *Ledger) StartProxy(upstream string) (*Proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("meter: bad upstream %q", upstream)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u) // also sets Host to the upstream's
			pr.Out.Host = u.Host
			if pr.In.Context().Value(reqInfoKey{}) != nil {
				// let the transport negotiate (and undo) compression, so the
				// usage we parse is plain text
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		FlushInterval: -1, // stream every write through
		ModifyResponse: func(resp *http.Response) error {
			info, _ := resp.Request.Context().Value(reqInfoKey{}).(*reqInfo)
			if info == nil || resp.StatusCode != http.StatusOK {
				return nil
			}
			sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			mb := &meteredBody{rc: resp.Body, l: l, sse: sse, model: info.model}
			if v, err := strconv.ParseFloat(resp.Header.Get(CostHeader), 64); err == nil { // an exact per-call cost wins over the price table
				mb.cost, mb.hasCost = v, true
			}
			resp.Body = mb
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			apiError(w, http.StatusBadGateway, "api_error", "warden meter: upstream unreachable")
		},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxRequest+1))
			_ = r.Body.Close()
			if err != nil {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "warden meter: can't read the request")
				return
			}
			if len(body) > maxRequest {
				apiError(w, http.StatusRequestEntityTooLarge, "request_too_large", "warden meter: request too large")
				return
			}
			var req struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			_ = json.Unmarshal(body, &req) // malformed: upstream will say so; estimate from the size
			if err := l.Allow(l.Estimate(req.Model, req.MaxTokens, len(body))); err != nil {
				if errors.Is(err, ErrOverBudget) {
					apiError(w, http.StatusTooManyRequests, "rate_limit_error", "warden budget: daily cap reached")
				} else {
					apiError(w, http.StatusServiceUnavailable, "api_error", "warden budget: the ledger is unavailable")
				}
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
			r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, &reqInfo{model: req.Model}))
		}
		rp.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("meter: %w", err)
	}
	p := &Proxy{Addr: ln.Addr().String(), ln: ln, srv: &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// Close stops the proxy and drops open connections.
func (p *Proxy) Close() error { return p.srv.Close() }

func apiError(w http.ResponseWriter, code int, typ, msg string) {
	bs, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(bs)
}

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (w wireUsage) usage() Usage {
	return Usage{Input: w.InputTokens, Output: w.OutputTokens, CacheRead: w.CacheReadInputTokens, CacheWrite: w.CacheCreationInputTokens}
}

// meteredBody passes a response through untouched while reading its usage,
// and records it once when the body ends (EOF, error or Close).
type meteredBody struct {
	rc      io.ReadCloser
	l       *Ledger
	sse     bool
	mu      sync.Mutex
	model   string
	u       Usage
	seen    bool
	buf     []byte // SSE: the partial line; JSON: the body so far
	once    sync.Once
	cost    float64 // from CostHeader, if the upstream sent it
	hasCost bool
}

// CostHeader: LiteLLM's per-call cost in USD.
const CostHeader = "x-litellm-response-cost"

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.feed(p[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *meteredBody) Close() error {
	b.finish()
	return b.rc.Close()
}

func (b *meteredBody) feed(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sse {
		if len(b.buf)+len(p) <= maxJSON {
			b.buf = append(b.buf, p...)
		}
		return
	}
	b.buf = append(b.buf, p...)
	for {
		i := bytes.IndexByte(b.buf, '\n')
		if i < 0 {
			break
		}
		b.line(b.buf[:i])
		b.buf = b.buf[i+1:]
	}
	if len(b.buf) > maxLine { // a huge content line: not a usage event
		b.buf = nil
	}
	if len(b.buf) == 0 {
		b.buf = nil // let the consumed backing array go
	}
}

func (b *meteredBody) line(ln []byte) {
	ln = bytes.TrimRight(ln, "\r")
	data, ok := bytes.CutPrefix(ln, []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"message_`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Model string     `json:"model"`
			Usage *wireUsage `json:"usage"`
		} `json:"message"`
		Usage *wireUsage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message.Model != "" {
			b.model = ev.Message.Model
		}
		if ev.Message.Usage != nil {
			b.u, b.seen = ev.Message.Usage.usage(), true
		}
	case "message_delta":
		if w := ev.Usage; w != nil { // cumulative counts
			b.seen = true
			b.u.Output = w.OutputTokens
			if w.InputTokens > 0 {
				b.u.Input = w.InputTokens
			}
			if w.CacheReadInputTokens > 0 {
				b.u.CacheRead = w.CacheReadInputTokens
			}
			if w.CacheCreationInputTokens > 0 {
				b.u.CacheWrite = w.CacheCreationInputTokens
			}
		}
	}
}

func (b *meteredBody) finish() {
	b.once.Do(func() {
		b.mu.Lock()
		if b.sse {
			if len(b.buf) > 0 {
				b.line(b.buf)
			}
		} else {
			var resp struct {
				Model string     `json:"model"`
				Usage *wireUsage `json:"usage"`
			}
			if json.Unmarshal(b.buf, &resp) == nil && resp.Usage != nil {
				b.u, b.seen = resp.Usage.usage(), true
				if resp.Model != "" {
					b.model = resp.Model
				}
			}
		}
		b.buf = nil
		model, u, seen := b.model, b.u, b.seen
		b.mu.Unlock()
		switch {
		case b.hasCost:
			b.l.RecordCost(model, u, b.cost)
		case seen:
			b.l.Record(model, u)
		}
	})
}
