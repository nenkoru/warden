package brain

import (
	"context"
	"strconv"
	"sync"
)

// Stats: what the brain calls made with a context did (WithStats), for the
// experiment log: model turns, tool calls and how each went, the largest
// prompt, how the last turn ended. Safe for concurrent use; read it with
// Data once the calls returned.
type Stats struct {
	mu              sync.Mutex
	Steps           int    // model turns (requests answered)
	ToolCalls       int    // every tool call the model made
	UnknownTool     int    // a tool that does not exist
	BadArgs         int    // arguments that are not a JSON object
	ToolFailed      int    // a known tool that ran and failed
	Repeats         int    // calls identical (name and arguments) to the one before
	FinishReason    string // how the last turn ended (stop, tool_calls, max_tokens, max_steps, …)
	PromptTokensMax int64  // the largest prompt of a turn, in tokens

	last string
}

// CallOutcome: how one tool call went.
type CallOutcome int

const (
	CallOK      CallOutcome = iota
	CallFailed              // the tool ran and failed
	CallUnknown             // no such tool
	CallBadArgs             // the arguments did not parse
)

type statsKey struct{}

// WithStats: the calls made with ctx add to s.
func WithStats(ctx context.Context, s *Stats) context.Context {
	return context.WithValue(ctx, statsKey{}, s)
}

// StatsFrom: the Stats set by WithStats, or nil (every method takes nil).
func StatsFrom(ctx context.Context) *Stats {
	s, _ := ctx.Value(statsKey{}).(*Stats)
	return s
}

// Step counts a model turn and its prompt size.
func (s *Stats) Step(promptTokens int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Steps++
	s.PromptTokensMax = max(s.PromptTokensMax, promptTokens)
}

// Call counts a tool call; args is its arguments' text (the same text twice
// in a row is a repeat).
func (s *Stats) Call(name, args string, o CallOutcome) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ToolCalls++
	switch o {
	case CallFailed:
		s.ToolFailed++
	case CallUnknown:
		s.UnknownTool++
	case CallBadArgs:
		s.BadArgs++
	}
	if k := name + "\x00" + args; k == s.last {
		s.Repeats++
	} else {
		s.last = k
	}
}

// Finish records how the last turn ended.
func (s *Stats) Finish(reason string) {
	if s == nil || reason == "" {
		return
	}
	s.mu.Lock()
	s.FinishReason = reason
	s.mu.Unlock()
}

// failed counts a failure reported after the call (an ACP tool update).
func (s *Stats) failed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ToolFailed++
	s.mu.Unlock()
}

// Data: the counts as the experiment log's fields (steps, tool_calls,
// unknown_tool, bad_args, tool_errors, repeats, max_prompt_tokens,
// finish_reason).
func (s *Stats) Data() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]string{"steps": strconv.Itoa(s.Steps), "tool_calls": strconv.Itoa(s.ToolCalls), "unknown_tool": strconv.Itoa(s.UnknownTool),
		"bad_args": strconv.Itoa(s.BadArgs), "tool_errors": strconv.Itoa(s.ToolFailed), "repeats": strconv.Itoa(s.Repeats),
		"max_prompt_tokens": strconv.FormatInt(s.PromptTokensMax, 10)}
	if s.FinishReason != "" {
		d["finish_reason"] = s.FinishReason
	}
	return d
}
