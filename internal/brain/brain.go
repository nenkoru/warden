// Package brain is Warden's optional model: a judgement call on a task, with
// tools. Two backends: Endpoint (a raw-HTTP tool loop against any
// Anthropic- or OpenAI-compatible API) and ACP (an external ACP agent
// process, started per call).
package brain

import (
	"context"
	"strings"
)

// Tool is something the model may call. Run returns the result text and
// whether it succeeded (false is reported to the model as an error result).
type Tool struct {
	Name, Description string
	// Aliases: other names a model may call it by (read_file, str_replace,
	// …); a call by an alias runs the tool.
	Aliases []string
	Schema  map[string]any // JSON schema of the arguments object; nil = no arguments
	Run     func(ctx context.Context, args map[string]any) (string, bool)
}

// Brain answers one task. tools nil = a plain answer.
type Brain interface {
	Do(ctx context.Context, system, task string, tools []Tool) (string, error)
}

func (t Tool) schema() map[string]any {
	if t.Schema != nil {
		return t.Schema
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// Find: the tool called name, else the one with it as an alias (any case).
func Find(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	for i := range tools {
		for _, a := range tools[i].Aliases {
			if strings.EqualFold(a, name) || strings.EqualFold(tools[i].Name, name) {
				return &tools[i]
			}
		}
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
