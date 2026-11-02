package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// session: what one Do keeps across its turns besides the messages, so a
// small local model stays on a short, current context (the oh-my-pi way):
// tool names and arguments repaired before a call fails, the same call for
// the same result caught as a loop, a warning before the last turns, and old
// tool results elided once the prompt outgrows the budget.
type session struct {
	cfg     *EndpointConfig
	tools   []Tool
	seen    map[string]string // call (name and arguments) → its last result
	repeats map[string]int    // call → how often it returned the same result again
	results []result          // tool results, oldest first
}

// result: one tool result in the conversation; msg["content"] is its text,
// which elide may shorten in place.
type result struct {
	name string
	msg  map[string]any
}

// repeatLimit: the same call returning the same result this many times in
// one run ends it as a loop.
const repeatLimit = 3

// ErrStuck: the model ran but did not finish (a loop, out of turns), as
// opposed to an upstream that failed; errors.Is tells the two apart.
var ErrStuck = errors.New("the model did not finish")

// errLoop: the model kept repeating itself (the experiment log's "loop").
var errLoop = fmt.Errorf("stopped: repeated identical tool calls with the same result (%w)", ErrStuck)

func newSession(cfg *EndpointConfig, tools []Tool) *session {
	return &session{cfg: cfg, tools: tools, seen: map[string]string{}, repeats: map[string]int{}}
}

// call runs one tool call; loop is set once the model has made the same call
// for the same result repeatLimit times.
func (s *session) call(ctx context.Context, e *endpoint, name string, args map[string]any, raw string) (out string, ok, loop bool) {
	t := Find(s.tools, name)
	if t == nil || t.Run == nil {
		StatsFrom(ctx).Call(name, raw, CallUnknown)
		out = fmt.Sprintf("error: unknown tool %q. The tools are: %s.", name, strings.Join(s.names(), ", "))
		fmt.Fprintf(e.cfg.Log, "warden brain: %s → %s\n", name, out)
		return out, false, false
	}
	out, ok = t.Run(ctx, args)
	StatsFrom(ctx).Call(t.Name, raw, map[bool]CallOutcome{true: CallOK, false: CallFailed}[ok])
	fmt.Fprintf(e.cfg.Log, "warden brain: %s → %s\n", t.Name, firstLine(out))
	key := t.Name + "\x00" + canonical(args)
	if prev, was := s.seen[key]; was && prev == out {
		s.repeats[key]++
		if s.repeats[key]+1 >= repeatLimit {
			return out, ok, true
		}
		return "[the same result as your identical call earlier — it is above. Do not repeat a call; take the next step.]", ok, false
	}
	s.seen[key] = out
	return out, ok, false
}

// keep remembers a tool result message for elision.
func (s *session) keep(name string, msg map[string]any) {
	s.results = append(s.results, result{name, msg})
}

// warn: a note for the turn's last result when the run is about to end
// (turn counts from 0), so the model finishes instead of being cut off.
func (s *session) warn(turn int) string {
	switch s.cfg.MaxSteps - turn - 1 { // turns left
	case 1:
		return "\n\n[Warden: this is your last turn. Reply now with your summary and no tool call; work left undone is lost otherwise.]"
	case 3:
		return "\n\n[Warden: 3 turns left in this run. Finish the change you are on, then reply with your summary and no tool call.]"
	}
	return ""
}

// elide: once a prompt passed the budget, every tool result but the last
// KeepResults is replaced by a one-line note; the model calls the tool again
// if it still needs one. Done in one sweep, so a prefix cache is broken
// rarely.
func (s *session) elide(promptTokens int64) {
	if s.cfg.ContextBudget <= 0 || promptTokens <= int64(s.cfg.ContextBudget) {
		return
	}
	for i := 0; i < len(s.results)-s.cfg.KeepResults; i++ {
		r := s.results[i]
		text, _ := r.msg["content"].(string)
		if len(text) <= 300 || strings.HasPrefix(text, "[elided") {
			continue
		}
		r.msg["content"] = fmt.Sprintf("[elided: an older %s result (%d lines) was dropped to keep the context small; call the tool again if you still need it]",
			r.name, strings.Count(text, "\n")+1)
	}
}

func (s *session) names() []string {
	out := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.Name)
	}
	return out
}

// canonical: the arguments as JSON with sorted keys, so the same call made
// with its keys in another order is still the same call.
func canonical(args map[string]any) string {
	b, _ := json.Marshal(args) // encoding/json sorts map keys
	return string(b)
}

// parseArgs: a tool call's arguments as an object, repairing what local
// models commonly send instead: a fenced block, a JSON string holding the
// object, trailing text after it. The error tells the model how to fix it.
func parseArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "```json"), "```")
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "```"))
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	err := json.Unmarshal([]byte(raw), &args)
	if err == nil && args != nil {
		return args, nil
	}
	var inner string // double-encoded: "{\"path\": …}"
	if json.Unmarshal([]byte(raw), &inner) == nil {
		if json.Unmarshal([]byte(strings.TrimSpace(inner)), &args) == nil && args != nil {
			return args, nil
		}
	}
	if dec := json.NewDecoder(strings.NewReader(raw)); dec.Decode(&args) == nil && args != nil {
		return args, nil // an object followed by stray text
	}
	if err == nil {
		err = errors.New("not an object")
	}
	return nil, fmt.Errorf("arguments are not a JSON object: %v. Send exactly one JSON object; inside strings escape newlines as \\n, tabs as \\t and quotes as \\\"", err)
}

// A tool call left in the text by a server whose parser missed it: Hermes
// style (<tool_call>{"name":…,"arguments":{…}}</tool_call>) or Qwen's XML
// style (<function=NAME><parameter=KEY>VALUE</parameter></function>).
var (
	textCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*(?:</tool_call>|$)`)
	xmlFuncRe  = regexp.MustCompile(`(?s)<function=([^>\s]+)>(.*?)(?:</function>|$)`)
	xmlParamRe = regexp.MustCompile(`(?s)<parameter=([^>\s]+)>\n?(.*?)\n?</parameter>`)
)

// textCall: a call written in the reply text instead of the tool_calls
// field. found: the text has a call at all (parsed or not).
type textCall struct {
	Name string
	Args string // the arguments as JSON
}

func textCalls(content string) (calls []textCall, found bool) {
	for _, m := range textCallRe.FindAllStringSubmatch(content, -1) {
		found = true
		body := m[1]
		if f := xmlFuncRe.FindStringSubmatch(body); f != nil {
			args := map[string]any{}
			for _, p := range xmlParamRe.FindAllStringSubmatch(f[2], -1) {
				var v any = p[2] // text stays text; only a bare number or boolean is one
				if n, err := strconv.Atoi(strings.TrimSpace(p[2])); err == nil {
					v = n
				} else if b, err := strconv.ParseBool(strings.TrimSpace(p[2])); err == nil {
					v = b
				}
				args[p[1]] = v
			}
			b, _ := json.Marshal(args)
			calls = append(calls, textCall{f[1], string(b)})
			continue
		}
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(body), &c) == nil && c.Name != "" {
			a := string(c.Arguments)
			var s string
			if json.Unmarshal(c.Arguments, &s) == nil {
				a = s // arguments sent as a JSON string
			}
			calls = append(calls, textCall{c.Name, or(a, "{}")})
		}
	}
	return calls, found
}
