package experiment

import (
	"regexp"
	"strconv"
)

// The failure categories, in the order the report lists them.
const (
	ToolCall     = "tool-call"     // an unknown tool, arguments that are not a JSON object
	ContextLimit = "context-limit" // the prompt outgrew (or neared) the model's context
	WrongEdit    = "wrong-edit"    // CI or the local checks red after the change, no change, a protected path
	Loop         = "loop"          // max_steps, the same tool call again and again
	Timeout      = "timeout"       // a deadline
	GaveUp       = "gave-up"       // handed to the owner, or the model said it can't
	Other        = "other"         // a failure none of the above explains
)

// Order: every category, in report order.
var Order = []string{ToolCall, ContextLimit, WrongEdit, Loop, Timeout, GaveUp, Other}

var patterns = []struct {
	cat string
	re  *regexp.Regexp
}{
	// brain/endpoint.go: "error: unknown tool X", "error: arguments are not a JSON object"
	{ToolCall, regexp.MustCompile(`(?i)unknown tool|arguments are not a json object|invalid (tool )?arguments|invalid tool call|tool_use_failed|failed to parse tool`)},
	{ContextLimit, regexp.MustCompile(`(?i)context[ _]length|maximum context|context window|too many tokens|prompt is too long|context_length_exceeded`)},
	{WrongEdit, regexp.MustCompile(`(?i)checks (stayed )?red|ci red|made no change|changed nothing|protected paths|contained a credential`)},
	// brain/endpoint.go: "stopped after N steps (max_steps)"
	{Loop, regexp.MustCompile(`(?i)max_steps|max steps|stopped after \d+ steps|repeated (identical )?tool calls`)},
	{Timeout, regexp.MustCompile(`(?i)deadline exceeded|timed out|timeout`)},
	{GaveUp, regexp.MustCompile(`(?i)\bi can(?:not|'t|’t)\b|\bi(?: am|'m|’m) (?:unable|not able)\b|\bunable to (?:complete|do|make|implement|fix)\b|\bwill not make\b`)},
}

// DefaultContextTokens: the model's context when data has no context_tokens.
const DefaultContextTokens = 262144

// Categories: why an act on a task failed, from its phase, outcome, text
// (summary, log, errors) and data (the brain's counts: unknown_tool,
// bad_args, repeats, finish_reason, max_prompt_tokens against
// context_tokens); several may hold; none for a success. A fix round always
// counts wrong-edit (CI was red after the push); a give-up always counts
// gave-up.
func Categories(phase string, ok bool, text string, data map[string]string) []string {
	hit := map[string]bool{}
	switch phase {
	case "fix":
		hit[WrongEdit] = true
	case "giveup":
		hit[GaveUp] = true
	}
	if !ok {
		for _, p := range patterns {
			if p.re.MatchString(text) {
				hit[p.cat] = true
			}
		}
		num := func(k string) int64 { n, _ := strconv.ParseInt(data[k], 10, 64); return n }
		if num("unknown_tool")+num("bad_args") > 0 {
			hit[ToolCall] = true
		}
		if num("repeats") >= 2 || data["finish_reason"] == "max_steps" {
			hit[Loop] = true
		}
		limit := num("context_tokens")
		if limit <= 0 {
			limit = DefaultContextTokens
		}
		if p := num("max_prompt_tokens"); p*10 >= limit*9 { // within 10% of the context
			hit[ContextLimit] = true
		}
		if len(hit) == 0 {
			hit[Other] = true
		}
	}
	var out []string
	for _, c := range Order {
		if hit[c] {
			out = append(out, c)
		}
	}
	return out
}
