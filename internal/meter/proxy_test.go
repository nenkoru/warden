package meter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFor(t *testing.T, l *Ledger, upstream http.Handler) (*Proxy, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != up.Listener.Addr().String() {
			t.Errorf("Host = %s, want the upstream's", r.Host)
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	p, err := l.StartProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if !strings.HasPrefix(p.Addr, "127.0.0.1:") {
		t.Errorf("Addr = %s", p.Addr)
	}
	return p, &calls
}

func post(t *testing.T, p *Proxy, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+p.Addr+path, strings.NewReader(body))
	req.Header.Set("x-api-key", "sekrit")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// spentEventually waits for the proxy's Record, which runs as the response ends.
func spentEventually(t *testing.T, l *Ledger, want float64) {
	t.Helper()
	var got float64
	for range 200 {
		if got, _ = l.Spent(); near(got, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("spent = %v, want %v", got, want)
}

func TestProxyJSON(t *testing.T) {
	l := open(t, Config{PerDayUSD: 1000, StateDir: t.TempDir()})
	p, _ := proxyFor(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "beta=true" {
			t.Errorf("url = %s", r.URL)
		}
		if r.Header.Get("x-api-key") != "sekrit" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("headers not forwarded: %v", r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"hello"`) {
			t.Errorf("body = %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-sonnet-5-5","content":[{"type":"text","text":"hi"}],
			"usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":2000,"cache_creation_input_tokens":100}}`)
	}))
	resp := post(t, p, "/v1/messages?beta=true", `{"model":"claude-opus-5-5","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"text":"hi"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// billed at the responding model's price (sonnet), not the requested one
	spentEventually(t, l, (1000*2+500*10+2000*0.2+100*2.5)/1e6)
}

func TestProxySSE(t *testing.T) {
	l := open(t, Config{PerDayUSD: 1000, StateDir: t.TempDir()})
	release := make(chan struct{})
	p, _ := proxyFor(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1,\"cache_read_input_tokens\":1000,\"cache_creation_input_tokens\":10}}}\r\n\r\n")
		fl.Flush()
		select { // the client must see message_start before the stream ends
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("the proxy buffered the stream")
		}
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":200}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}")
	}))
	resp := post(t, p, "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":100,"stream":true}`)
	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, "message_start") {
		t.Fatalf("first line %q %v", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "message_stop") || !strings.Contains(string(rest), `"text":"hi"`) {
		t.Errorf("stream = %s", rest)
	}
	spentEventually(t, l, (100*4+200*20+1000*0.4+10*5)/1e6)
}

func TestProxyOverBudget(t *testing.T) {
	l := open(t, Config{PerDayUSD: 1, StateDir: t.TempDir()})
	p, calls := proxyFor(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream called over budget")
	}))
	// 64000 output tokens at opus' $20/M = $1.28 > $1
	resp := post(t, p, "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":64000,"messages":[]}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Type != "error" || e.Error.Type != "rate_limit_error" ||
		e.Error.Message != "warden budget: daily cap reached" {
		t.Errorf("body = %s (%v)", b, err)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d", calls.Load())
	}
}

func TestProxyPassThrough(t *testing.T) {
	l := open(t, Config{PerDayUSD: 1, StateDir: t.TempDir()})
	p, calls := proxyFor(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q,"key":%q,"usage":{"input_tokens":999999999}}`, r.URL.Path, r.Header.Get("x-api-key"))
	}))
	resp := post(t, p, "/v1/messages/count_tokens", `{"model":"claude-opus-5-5","max_tokens":64000}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"key":"sekrit"`) || calls.Load() != 1 {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if today, _ := l.Spent(); today != 0 {
		t.Errorf("count_tokens was metered: %v", today)
	}
}
