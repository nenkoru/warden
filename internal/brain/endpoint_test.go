package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var echo = Tool{
	Name: "echo", Description: "echo msg",
	Schema: map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
	Run: func(_ context.Context, a map[string]any) (string, bool) {
		s, ok := a["msg"].(string)
		return "echo:" + s, ok
	},
}

// script serves the i-th handler on the i-th request.
type script struct {
	t     *testing.T
	steps []func(hdr http.Header, body map[string]any) (int, string)
	n     int
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.n >= len(s.steps) {
		s.t.Errorf("unexpected request %d", s.n)
		w.WriteHeader(500)
		return
	}
	var body map[string]any
	bs, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(bs, &body); err != nil {
		s.t.Errorf("request body: %v", err)
	}
	code, out := s.steps[s.n](r.Header, body)
	s.n++
	w.WriteHeader(code)
	_, _ = io.WriteString(w, out)
}

func serve(t *testing.T, steps ...func(http.Header, map[string]any) (int, string)) (*httptest.Server, *script) {
	s := &script{t: t, steps: steps}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func jsonEq(t *testing.T, got any, want string) {
	t.Helper()
	g, _ := json.Marshal(got)
	var a, b any
	_ = json.Unmarshal(g, &a)
	_ = json.Unmarshal([]byte(want), &b)
	ga, _ := json.Marshal(a)
	wb, _ := json.Marshal(b)
	if string(ga) != string(wb) {
		t.Errorf("got  %s\nwant %s", ga, wb)
	}
}

const turn1 = `[{"type":"text","text":"looking "},{"type":"tool_use","id":"t1","name":"echo","input":{"msg":"hi"}},{"type":"tool_use","id":"t2","name":"nope","input":{}}]`

func TestAnthropicToolLoop(t *testing.T) {
	t.Setenv("TEST_KEY", "sekrit")
	srv, s := serve(t,
		func(h http.Header, b map[string]any) (int, string) {
			if h.Get("x-api-key") != "sekrit" || h.Get("Authorization") != "" || h.Get("anthropic-version") == "" {
				t.Errorf("headers: %v", h)
			}
			if _, ok := b["fallbacks"]; ok || h.Get("anthropic-beta") != "" {
				t.Error("fallbacks sent to a non-anthropic host")
			}
			jsonEq(t, b["tools"], `[{"name":"echo","description":"echo msg","input_schema":{"type":"object","properties":{"msg":{"type":"string"}}}}]`)
			if b["system"] != "sys" || b["model"] != "claude-opus-5-5" {
				t.Errorf("body: %v", b)
			}
			return 200, `{"content":` + turn1 + `,"stop_reason":"tool_use"}`
		},
		func(h http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("messages: %v", msgs)
			}
			jsonEq(t, msgs[1], `{"role":"assistant","content":`+turn1+`}`) // echoed verbatim
			jsonEq(t, msgs[2], `{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"echo:hi","is_error":false},
				{"type":"tool_result","tool_use_id":"t2","content":"error: unknown tool nope","is_error":true}]}`)
			return 200, `{"content":[{"type":"text","text":"done"}],"stop_reason":"pause_turn"}`
		},
		func(h http.Header, b map[string]any) (int, string) {
			if n := len(b["messages"].([]any)); n != 4 {
				t.Errorf("pause_turn: want 4 messages, got %d", n)
			}
			return 200, `{"content":[{"type":"text","text":"."}],"stop_reason":"end_turn"}`
		},
	)
	var log strings.Builder
	out, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL + "/", APIKeyEnv: "TEST_KEY", Log: &log}).
		Do(context.Background(), "sys", "task", []Tool{echo})
	if err != nil || out != "looking done." {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if s.n != 3 || !strings.Contains(log.String(), "echo → echo:hi") {
		t.Errorf("requests=%d log=%q", s.n, log.String())
	}
}

func TestAnthropicBearerAndStops(t *testing.T) {
	t.Setenv("TEST_KEY", "tok")
	for reason, wantErr := range map[string]string{"refusal": "declined", "max_tokens": "max_tokens", "end_turn": ""} {
		srv, _ := serve(t, func(h http.Header, b map[string]any) (int, string) {
			if h.Get("Authorization") != "Bearer tok" || h.Get("x-api-key") != "" {
				t.Errorf("headers: %v", h)
			}
			if _, ok := b["tools"]; ok {
				t.Error("tools sent with no tools")
			}
			return 200, `{"content":[{"type":"text","text":"x"}],"stop_reason":"` + reason + `"}`
		})
		out, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL, APIKeyEnv: "TEST_KEY", Auth: "bearer"}).
			Do(context.Background(), "", "q", nil)
		if out != "x" || (wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: out=%q err=%v", reason, out, err)
		}
	}
}

// redirect sends every request to srv, keeping the original Host for the check.
type redirect struct{ to *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestAnthropicFallbacks(t *testing.T) {
	no := false
	for _, c := range []struct {
		fallbacks *bool
		want      bool
	}{{nil, true}, {&no, false}} {
		srv, _ := serve(t, func(h http.Header, b map[string]any) (int, string) {
			_, inBody := b["fallbacks"]
			inHdr := h.Get("anthropic-beta") == "server-side-fallback-2026-07-01"
			if inBody != c.want || inHdr != c.want {
				t.Errorf("fallbacks=%v: body %v header %v", c.fallbacks, inBody, inHdr)
			}
			return 200, `{"content":[],"stop_reason":"end_turn"}`
		})
		u, _ := url.Parse(srv.URL)
		_, err := Endpoint(EndpointConfig{Provider: "anthropic", APIKeyEnv: "NO_SUCH_KEY_ENV", Fallbacks: c.fallbacks, HTTP: &http.Client{Transport: redirect{u}}}).
			Do(context.Background(), "", "q", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetry(t *testing.T) {
	defer func(f func(int) time.Duration) { backoff = f }(backoff)
	backoff = func(int) time.Duration { return time.Millisecond }
	srv, s := serve(t,
		func(http.Header, map[string]any) (int, string) { return 429, `slow down` },
		func(http.Header, map[string]any) (int, string) { return 503, `busy` },
		func(http.Header, map[string]any) (int, string) {
			return 200, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
		},
	)
	out, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL}).Do(context.Background(), "", "q", nil)
	if err != nil || out != "ok" || s.n != 3 {
		t.Fatalf("out=%q err=%v n=%d", out, err, s.n)
	}
	srv, _ = serve(t, func(http.Header, map[string]any) (int, string) { return 400, `bad request` })
	if _, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL}).Do(context.Background(), "", "q", nil); err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("400: %v", err)
	}
}

const call1 = `{"role":"assistant","content":null,"tool_calls":[
	{"id":"c1","type":"function","function":{"name":"echo","arguments":"{\"msg\":\"hi\"}"}},
	{"id":"c2","type":"function","function":{"name":"echo","arguments":"{bad"}}]}`

func TestOpenAIToolLoop(t *testing.T) {
	t.Setenv("TEST_KEY", "ok-key")
	srv, s := serve(t,
		func(h http.Header, b map[string]any) (int, string) {
			if h.Get("Authorization") != "Bearer ok-key" {
				t.Errorf("headers: %v", h)
			}
			if _, ok := b["fallbacks"]; ok {
				t.Error("fallbacks sent to openai")
			}
			jsonEq(t, b["tools"], `[{"type":"function","function":{"name":"echo","description":"echo msg","parameters":{"type":"object","properties":{"msg":{"type":"string"}}}}}]`)
			jsonEq(t, b["messages"], `[{"role":"system","content":"sys"},{"role":"user","content":"task"}]`)
			return 200, `{"choices":[{"message":` + call1 + `,"finish_reason":"tool_calls"}]}`
		},
		func(h http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			if len(msgs) != 5 {
				t.Fatalf("messages: %v", msgs)
			}
			jsonEq(t, msgs[2], call1)
			jsonEq(t, msgs[3], `{"role":"tool","tool_call_id":"c1","content":"echo:hi"}`)
			if m := msgs[4].(map[string]any); m["tool_call_id"] != "c2" || !strings.HasPrefix(m["content"].(string), "error: arguments are not a JSON object") {
				t.Errorf("bad-args result: %v", m)
			}
			return 200, `{"choices":[{"message":{"role":"assistant","content":"final"},"finish_reason":"stop"}]}`
		},
	)
	out, err := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", APIKeyEnv: "TEST_KEY"}).
		Do(context.Background(), "sys", "task", []Tool{echo})
	if err != nil || out != "final" || s.n != 2 {
		t.Fatalf("out=%q err=%v n=%d", out, err, s.n)
	}
}

func TestNormalize(t *testing.T) {
	c := EndpointConfig{Provider: "anthropic"}
	if err := c.Normalize(); err != nil || c.BaseURL != "https://api.anthropic.com" || c.Model != "claude-opus-5-5" ||
		c.APIKeyEnv != "ANTHROPIC_API_KEY" || c.Auth != "x-api-key" || c.MaxSteps != 25 || c.MaxTokens != 16000 {
		t.Errorf("anthropic defaults: %+v %v", c, err)
	}
	c = EndpointConfig{Provider: "openai", Model: "gpt"}
	if err := c.Normalize(); err != nil || c.BaseURL != "https://api.openai.com/v1" || c.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("openai defaults: %+v %v", c, err)
	}
	for _, bad := range []EndpointConfig{{Provider: "openai"}, {Provider: "anthropic", Auth: "basic"}, {Provider: "acp"}} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
	if _, err := Endpoint(EndpointConfig{}).Do(context.Background(), "", "q", nil); err == nil {
		t.Error("Do with a bad config: want an error")
	}
}
