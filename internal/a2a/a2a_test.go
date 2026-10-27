package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	h = Handler(NewCard("warden", "watches things", srv.URL+"/"), map[string]string{"tok": "alice"},
		func(_ context.Context, from, text string) (string, error) {
			if text == "boom" {
				return "", errors.New("kaput")
			}
			return from + " said: " + text, nil
		})
	t.Cleanup(srv.Close)
	return srv
}

func rpc(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func errCode(m map[string]any) int {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return int(c)
}

const sendBody = `{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}}`

func TestCard(t *testing.T) {
	srv := newServer(t)
	resp, err := http.Get(srv.URL + WellKnown)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c Card
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "warden" || len(c.SupportedInterfaces) != 1 || c.SupportedInterfaces[0].ProtocolBinding != "JSONRPC" ||
		c.SupportedInterfaces[0].ProtocolVersion != "1.0" || c.SecuritySchemes["bearer"].HTTPAuth.Scheme != "Bearer" ||
		c.DefaultInputModes[0] != "text/plain" || c.Skills[0].ID == "" {
		t.Fatalf("card: %+v", c)
	}
}

func TestSendMessageAuthAndGetTask(t *testing.T) {
	srv := newServer(t)
	for _, tok := range []string{"", "wrong"} {
		code, out := rpc(t, srv.URL, tok, sendBody)
		if code != http.StatusUnauthorized || out["error"] == nil {
			t.Fatalf("token %q: %d %v", tok, code, out)
		}
	}
	code, out := rpc(t, srv.URL, "tok", sendBody)
	if code != 200 || out["id"].(float64) != 7 {
		t.Fatalf("%d %v", code, out)
	}
	task := out["result"].(map[string]any)["task"].(map[string]any)
	if task["status"].(map[string]any)["state"] != "TASK_STATE_COMPLETED" {
		t.Fatalf("task %v", task)
	}
	text := task["artifacts"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	if text != "alice said: hello" {
		t.Fatalf("reply %v", text)
	}
	id := task["id"].(string)
	_, out = rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":8,"method":"GetTask","params":{"id":"`+id+`","historyLength":1}}`)
	got := out["result"].(map[string]any)
	if got["id"] != id || len(got["history"].([]any)) != 1 {
		t.Fatalf("GetTask %v", out)
	}
	_, out = rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":9,"method":"GetTask","params":{"id":"nope"}}`)
	if errCode(out) != -32001 {
		t.Fatalf("missing task: %v", out)
	}
}

func TestLegacyAliases(t *testing.T) {
	srv := newServer(t)
	_, out := rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":"a","method":"message/send","params":{"message":{"kind":"message","messageId":"m","role":"user","parts":[{"kind":"text","text":"hi"}]}}}`)
	task := out["result"].(map[string]any)
	if task["kind"] != "task" || task["status"].(map[string]any)["state"] != "completed" {
		t.Fatalf("legacy send %v", out)
	}
	_, out = rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":"b","method":"tasks/get","params":{"id":"`+task["id"].(string)+`"}}`)
	if out["result"].(map[string]any)["kind"] != "task" {
		t.Fatalf("legacy get %v", out)
	}
}

func TestErrors(t *testing.T) {
	srv := newServer(t)
	for body, want := range map[string]int{
		`{not json`: -32700,
		`{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{}}`:                                   -32601,
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{}}`:                                  -32602,
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"parts":[{"url":"x"}]}}}`: -32005,
	} {
		if _, out := rpc(t, srv.URL, "tok", body); errCode(out) != want {
			t.Errorf("%s: want %d got %v", body, want, out)
		}
	}
}

func TestSendRoundTrip(t *testing.T) {
	srv := newServer(t)
	ctx := context.Background()
	for _, u := range []string{srv.URL, srv.URL + "/", srv.URL + WellKnown} {
		got, err := Send(ctx, u, "tok", "ping")
		if err != nil || got != "alice said: ping" {
			t.Fatalf("%s: %q %v", u, got, err)
		}
	}
	if _, err := Send(ctx, srv.URL, "bad", "ping"); err == nil {
		t.Fatal("bad token accepted")
	}
	if _, err := Send(ctx, srv.URL, "tok", "boom"); err == nil || !strings.Contains(err.Error(), "kaput") {
		t.Fatalf("failed task: %v", err)
	}
}

func TestSendLegacyServer(t *testing.T) {
	// a 0.3-only agent: no well-known card, only message/send, bare Message result
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if req.Method != "message/send" {
			res["error"] = map[string]any{"code": -32601, "message": "Method not found"}
		} else {
			res["result"] = map[string]any{"kind": "message", "messageId": "x", "role": "agent", "parts": []any{map[string]any{"kind": "text", "text": "old hi"}}}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()
	if got, err := Send(context.Background(), srv.URL, "", "hi"); err != nil || got != "old hi" {
		t.Fatalf("%q %v", got, err)
	}
}
