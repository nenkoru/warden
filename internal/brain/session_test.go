package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, raw := range []string{
		`{"path":"a.go"}`,
		"```json\n{\"path\":\"a.go\"}\n```",
		`"{\"path\":\"a.go\"}"`, // double-encoded
		`{"path":"a.go"}}`,      // a stray brace after the object
		` {"path":"a.go"} trailing words`,
	} {
		args, err := parseArgs(raw)
		if err != nil || args["path"] != "a.go" {
			t.Errorf("%q: %v %v", raw, args, err)
		}
	}
	if args, err := parseArgs(""); err != nil || len(args) != 0 {
		t.Errorf("empty: %v %v", args, err)
	}
	for _, raw := range []string{`{"path": "a.go`, `[1,2]`, `path=a.go`} {
		if _, err := parseArgs(raw); err == nil || !strings.Contains(err.Error(), "escape newlines") {
			t.Errorf("%q: want a parse error with the hint, got %v", raw, err)
		}
	}
}

func TestTextCalls(t *testing.T) {
	calls, found := textCalls("I will read it.\n<tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}\n</tool_call>")
	if !found || len(calls) != 1 || calls[0].Name != "read" || calls[0].Args != `{"path": "a.go"}` {
		t.Errorf("hermes: %+v %v", calls, found)
	}
	calls, _ = textCalls(`<tool_call>{"name":"read","arguments":"{\"path\":\"b.go\"}"}</tool_call>`)
	if len(calls) != 1 || calls[0].Args != `{"path":"b.go"}` {
		t.Errorf("string arguments: %+v", calls)
	}
	calls, _ = textCalls("<tool_call>\n<function=edit>\n<parameter=path>\na.go\n</parameter>\n<parameter=old_string>\nx := 1\n</parameter>\n" +
		"<parameter=new_string>\nx := 2\n</parameter>\n<parameter=replace_all>\nfalse\n</parameter>\n</function>\n</tool_call>")
	if len(calls) != 1 || calls[0].Name != "edit" || calls[0].Args != `{"new_string":"x := 2","old_string":"x := 1","path":"a.go","replace_all":false}` {
		t.Errorf("xml: %+v", calls)
	}
	if calls, found := textCalls("<tool_call>{not json</tool_call>"); !found || len(calls) != 0 {
		t.Errorf("broken: %+v %v", calls, found)
	}
	if _, found := textCalls("just an answer"); found {
		t.Error("plain text has no call")
	}
}

func oaCall(id, name, args string, prompt int) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":%d}}`, id, name, a, prompt)
}

const oaDone = `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`

// A tool call by another harness's name, one left in the reply text, extra
// request fields, and the same call again answered with a short note.
func TestOpenAIRepairs(t *testing.T) {
	srv, s := serve(t,
		func(_ http.Header, b map[string]any) (int, string) {
			jsonEq(t, b["chat_template_kwargs"], `{"enable_thinking":true}`)
			if b["model"] != "m" {
				t.Errorf("model: %v", b["model"])
			}
			return 200, oaCall("c1", "say", `{"msg":"hi"}`, 10) // an alias of echo
		},
		func(_ http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			jsonEq(t, msgs[len(msgs)-1], `{"role":"tool","tool_call_id":"c1","content":"echo:hi"}`)
			return 200, `{"choices":[{"message":{"role":"assistant","content":"<tool_call>{\"name\":\"echo\",\"arguments\":{\"msg\":\"hi\"}}</tool_call>"},"finish_reason":"stop"}]}`
		},
		func(_ http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			am := msgs[len(msgs)-2].(map[string]any)
			if tcs, _ := am["tool_calls"].([]any); len(tcs) != 1 || am["content"] != "" {
				t.Errorf("the text call as an assistant call: %v", am)
			}
			if m := msgs[len(msgs)-1].(map[string]any); !strings.Contains(m["content"].(string), "same result as your identical call") {
				t.Errorf("repeat: %v", m)
			}
			return 200, `{"choices":[{"message":{"role":"assistant","content":"<tool_call>{oops</tool_call>"},"finish_reason":"stop"}]}`
		},
		func(_ http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			if m := msgs[len(msgs)-1].(map[string]any); m["role"] != "user" || !strings.Contains(m["content"].(string), "could not be parsed") {
				t.Errorf("a retry prompt: %v", m)
			}
			return 200, oaDone
		},
	)
	al := echo
	al.Aliases = []string{"say"}
	st := &Stats{}
	out, err := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}, "model": "ignored"}}).
		Do(WithStats(context.Background(), st), "", "task", []Tool{al})
	if err != nil || out != "done" || s.n != 4 {
		t.Fatalf("out=%q err=%v n=%d", out, err, s.n)
	}
	if st.UnknownTool != 0 || st.BadArgs != 1 || st.ToolCalls != 3 {
		t.Errorf("stats: %+v", st.Data())
	}
}

// The run warns before its last turns, ends as stuck at max_steps, and
// elides old tool results once a prompt passes the budget.
func TestOpenAIWarnElideMaxSteps(t *testing.T) {
	long := strings.Repeat("line\n", 100)
	big := Tool{Name: "big", Run: func(_ context.Context, a map[string]any) (string, bool) {
		return fmt.Sprint(a["n"]) + "\n" + long, true
	}}
	var steps []func(http.Header, map[string]any) (int, string)
	for i := range 5 {
		steps = append(steps, func(_ http.Header, b map[string]any) (int, string) {
			msgs := b["messages"].([]any)
			var tools []string
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
					tools = append(tools, mm["content"].(string))
				}
			}
			switch i {
			case 2:
				if !strings.Contains(tools[1], "3 turns left") || strings.HasPrefix(tools[0], "[elided") {
					t.Errorf("warning, nothing elided yet: %q", tools)
				}
			case 3: // the prompt of turn 2 was over the budget: all but the last result elided
				if !strings.HasPrefix(tools[0], "[elided: an older big result (102 lines)") || !strings.HasPrefix(tools[1], "[elided") || strings.HasPrefix(tools[2], "[elided") {
					t.Errorf("elision: %q", tools)
				}
			case 4:
				if !strings.Contains(tools[3], "this is your last turn") {
					t.Errorf("last-turn warning: %q", tools[3])
				}
			}
			return 200, oaCall(fmt.Sprint("c", i), "big", fmt.Sprintf(`{"n":%d}`, i), 100+i*1000)
		})
	}
	srv, _ := serve(t, steps...)
	st := &Stats{}
	_, err := Endpoint(EndpointConfig{Provider: "openai", BaseURL: srv.URL, Model: "m", MaxSteps: 5, ContextBudget: 2000, KeepResults: 1}).
		Do(WithStats(context.Background(), st), "", "task", []Tool{big})
	if !errors.Is(err, ErrStuck) || !strings.Contains(err.Error(), "max_steps") || st.FinishReason != "max_steps" {
		t.Fatalf("err=%v finish=%s", err, st.FinishReason)
	}
}
