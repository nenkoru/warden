package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/meter"
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
			return 200, `{"content":` + turn1 + `,"stop_reason":"tool_use","usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":5}}`
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
	st := &Stats{}
	out, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL + "/", APIKeyEnv: "TEST_KEY", Log: &log}).
		Do(WithStats(context.Background(), st), "sys", "task", []Tool{echo})
	if err != nil || out != "looking done." {
		t.Fatalf("out=%q err=%v", out, err)
	}
	jsonEq(t, st.Data(), `{"steps":"3","tool_calls":"2","unknown_tool":"1","bad_args":"0","tool_errors":"0","repeats":"0","max_prompt_tokens":"1000","finish_reason":"end_turn"}`)
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
	st := &Stats{}
	out, err := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", APIKeyEnv: "TEST_KEY"}).
		Do(WithStats(context.Background(), st), "sys", "task", []Tool{echo})
	if err != nil || out != "final" || s.n != 2 {
		t.Fatalf("out=%q err=%v n=%d", out, err, s.n)
	}
	jsonEq(t, st.Data(), `{"steps":"2","tool_calls":"2","unknown_tool":"0","bad_args":"1","tool_errors":"0","repeats":"0","max_prompt_tokens":"0","finish_reason":"stop"}`)
}

// A model that repeats one failing call until max_steps: every call counted
// as failed and repeated, the largest prompt kept, the end is max_steps.
func TestEndpointStatsLoop(t *testing.T) {
	again := func(prompt int) func(http.Header, map[string]any) (int, string) {
		return func(http.Header, map[string]any) (int, string) {
			return 200, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"echo","arguments":"{\"msg\":1}"}}]},"finish_reason":"tool_calls"}],` +
				`"usage":{"prompt_tokens":` + strconv.Itoa(prompt) + `,"completion_tokens":3}}`
		}
	}
	srv, _ := serve(t, again(10), again(300), again(20))
	st := &Stats{}
	ctx := WithStats(context.Background(), st)
	_, err := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", MaxSteps: 3}).Do(ctx, "", "task", []Tool{echo})
	if err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("err=%v", err)
	}
	jsonEq(t, st.Data(), `{"steps":"3","tool_calls":"3","unknown_tool":"0","bad_args":"0","tool_errors":"3","repeats":"2","max_prompt_tokens":"300","finish_reason":"max_steps"}`)
	if StatsFrom(context.Background()) != nil || (*Stats)(nil).Data() != nil {
		t.Fatal("no Stats: nothing counted, nothing to read")
	}
	StatsFrom(context.Background()).Call("x", "", CallOK) // a nil sink is safe
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

func TestEndpointMeter(t *testing.T) {
	prices := meter.Prices{"m": {Input: 1, Output: 10, CacheRead: 0.1, CacheWrite: 2}}
	l, err := meter.Open(meter.Config{PerDayUSD: 1000, AlertEveryUSD: 0.005, Prices: prices, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t,
		func(http.Header, map[string]any) (int, string) {
			return 200, `{"content":` + turn1 + `,"stop_reason":"tool_use","model":"m",
				"usage":{"input_tokens":1000,"output_tokens":100,"cache_read_input_tokens":10000,"cache_creation_input_tokens":500}}`
		},
		func(http.Header, map[string]any) (int, string) {
			return 200, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2000,"output_tokens":300}}`
		},
	)
	var log strings.Builder
	out, err := Endpoint(EndpointConfig{Provider: "anthropic", BaseURL: srv.URL, Model: "m", MaxTokens: 100, Meter: l, Log: &log}).
		Do(context.Background(), "", "q", []Tool{echo})
	if err != nil || !strings.HasSuffix(out, "ok") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	want := (1000*1+100*10+10000*0.1+500*2)/1e6 + (2000*1+300*10)/1e6
	if today, _ := l.Spent(); today < want-1e-9 || today > want+1e-9 {
		t.Errorf("spent = %v, want %v", today, want)
	}
	if !strings.Contains(log.String(), "\nwarden budget: $0 spent on models today ($0.01") {
		t.Errorf("alerts not logged: %q", log.String())
	}

	// openai usage, then a cap that refuses the next call before it is sent
	srv, s := serve(t, func(http.Header, map[string]any) (int, string) {
		return 200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`
	})
	l2, err := meter.Open(meter.Config{PerDayUSD: 0.0025, Prices: prices, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", MaxTokens: 100, Meter: l2})
	if out, err := b.Do(context.Background(), "", "q", nil); err != nil || out != "hi" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if today, _ := l2.Spent(); today < 0.002-1e-9 || today > 0.002+1e-9 {
		t.Errorf("openai spent = %v", today)
	}
	_, err = b.Do(context.Background(), "", "q", nil) // $0.002 + $0.001 estimate > $0.0025
	if err == nil || !strings.Contains(err.Error(), "daily model budget reached") || s.n != 1 {
		t.Errorf("over budget: err=%v requests=%d", err, s.n)
	}
}
