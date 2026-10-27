package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nenkoru/warden/internal/meter"
	"github.com/nenkoru/warden/internal/version"
)

// EndpointConfig: a model endpoint driven by Warden's own tool loop. Plain
// HTTP, no SDK, so any compatible server works: Anthropic, GLM via Z.ai's
// Anthropic-compatible API (Auth "bearer"), OpenRouter, a local Ollama/vLLM.
type EndpointConfig struct {
	Provider  string // anthropic | openai
	BaseURL   string // default: https://api.anthropic.com | https://api.openai.com/v1
	Model     string // default (anthropic): claude-opus-5-5; required for openai
	APIKeyEnv string // default: ANTHROPIC_API_KEY | OPENAI_API_KEY (empty key: no auth header)
	Auth      string // anthropic: "x-api-key" (default) or "bearer" (Authorization: Bearer)
	MaxSteps  int    // tool-loop turns per call (default 25)
	MaxTokens int    // per response (default 16000)
	// Fallbacks (anthropic, api.anthropic.com only): let the API re-serve a
	// refused request on another model (server-side fallback, default on).
	Fallbacks *bool
	HTTP      *http.Client // default: 10-minute timeout
	Log       io.Writer    // one line per tool call; default: discard
	// Meter, if set, is asked before every request (Allow, with Estimate of
	// it) and told the usage after every response (Record). A *meter.Ledger
	// is one.
	Meter Meter
}

// Meter is a model budget: see internal/meter.
type Meter interface {
	Estimate(model string, maxTokens, inputBytes int) float64
	Allow(estimateUSD float64) error
	Record(model string, u meter.Usage) (float64, []meter.Alert)
}

// Normalize validates the config and fills in the defaults.
func (c *EndpointConfig) Normalize() error {
	switch c.Provider {
	case "anthropic":
		c.BaseURL = or(c.BaseURL, "https://api.anthropic.com")
		c.Model = or(c.Model, "claude-opus-5-5")
		c.APIKeyEnv = or(c.APIKeyEnv, "ANTHROPIC_API_KEY")
		c.Auth = or(c.Auth, "x-api-key")
		if c.Auth != "x-api-key" && c.Auth != "bearer" {
			return fmt.Errorf("brain: auth must be x-api-key or bearer, not %q", c.Auth)
		}
	case "openai":
		c.BaseURL = or(c.BaseURL, "https://api.openai.com/v1")
		c.APIKeyEnv = or(c.APIKeyEnv, "OPENAI_API_KEY")
		if c.Model == "" {
			return errors.New("brain: provider openai needs a model")
		}
	default:
		return fmt.Errorf("brain: provider must be anthropic or openai, not %q", c.Provider)
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = 25
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 16000
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Endpoint returns a Brain over cfg. A config that fails Normalize yields a
// Brain whose Do returns that error.
func Endpoint(cfg EndpointConfig) Brain {
	err := cfg.Normalize()
	return &endpoint{cfg: cfg, err: err}
}

type endpoint struct {
	cfg EndpointConfig
	err error
}

func (e *endpoint) Do(ctx context.Context, system, task string, tools []Tool) (string, error) {
	if e.err != nil {
		return "", e.err
	}
	if e.cfg.Provider == "anthropic" {
		return e.anthropic(ctx, system, task, tools)
	}
	return e.openai(ctx, system, task, tools)
}

// run runs one tool call and counts it in ctx's Stats (raw: the arguments' text).
func (e *endpoint) run(ctx context.Context, tools []Tool, name string, args map[string]any, raw string) (string, bool) {
	out, ok, how := "error: unknown tool "+name, false, CallUnknown
	if t := find(tools, name); t != nil && t.Run != nil {
		out, ok = t.Run(ctx, args)
		how = map[bool]CallOutcome{true: CallOK, false: CallFailed}[ok]
	}
	StatsFrom(ctx).Call(name, raw, how)
	fmt.Fprintf(e.cfg.Log, "warden brain: %s → %s\n", name, firstLine(out))
	return out, ok
}

// maxSteps: the loop ran out of turns.
func (e *endpoint) maxSteps(ctx context.Context) error {
	StatsFrom(ctx).Finish("max_steps")
	return fmt.Errorf("stopped after %d steps (max_steps)", e.cfg.MaxSteps)
}

// record tells the meter one response's usage; model "" is the configured one.
func (e *endpoint) record(model string, u meter.Usage) {
	if e.cfg.Meter == nil {
		return
	}
	_, alerts := e.cfg.Meter.Record(or(model, e.cfg.Model), u)
	for _, a := range alerts {
		fmt.Fprintln(e.cfg.Log, a.Text) // "warden budget: …"
	}
}

// backoff before retry attempt n (0-based); a variable so tests can shorten it.
var backoff = func(n int) time.Duration { return time.Duration(5<<n) * time.Second }

func (e *endpoint) post(ctx context.Context, url string, hdr map[string]string, body, out any) error {
	bs, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if m := e.cfg.Meter; m != nil {
		if err := m.Allow(m.Estimate(e.cfg.Model, e.cfg.MaxTokens, len(bs))); errors.Is(err, meter.ErrOverBudget) {
			return fmt.Errorf("daily model budget reached: %w", err)
		} else if err != nil {
			return fmt.Errorf("model budget: %w", err)
		}
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ { // 429 / 5xx: a few patient retries
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bs))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", version.UserAgent())
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err = e.cfg.HTTP.Do(req)
		if err == nil && resp.StatusCode != 429 && resp.StatusCode < 500 {
			break
		}
		if attempt == 3 {
			if err != nil {
				return err
			}
			break
		}
		if err == nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s: %s", url, resp.Status, tail(string(rb), 400))
	}
	return json.Unmarshal(rb, out)
}

// ---- Anthropic Messages API

func (e *endpoint) anthropic(ctx context.Context, system, task string, tools []Tool) (string, error) {
	hdr := map[string]string{"anthropic-version": "2023-06-01"}
	if k := os.Getenv(e.cfg.APIKeyEnv); k != "" && e.cfg.Auth == "bearer" {
		hdr["Authorization"] = "Bearer " + k
	} else if k != "" {
		hdr["x-api-key"] = k
	}
	body := map[string]any{"model": e.cfg.Model, "max_tokens": e.cfg.MaxTokens}
	if system != "" {
		body["system"] = system
	}
	if u, err := url.Parse(e.cfg.BaseURL); err == nil && u.Host == "api.anthropic.com" && (e.cfg.Fallbacks == nil || *e.cfg.Fallbacks) {
		hdr["anthropic-beta"] = "server-side-fallback-2026-07-01"
		body["fallbacks"] = "default" // a refusal is re-served by a fallback model in the same call
	}
	var ts []map[string]any
	for _, t := range tools {
		ts = append(ts, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.schema()})
	}
	if len(ts) > 0 {
		body["tools"] = ts
	}
	// the conversation is append-only: assistant turns go back exactly as received
	msgs := []any{map[string]any{"role": "user", "content": task}}
	var text strings.Builder
	for range e.cfg.MaxSteps {
		body["messages"] = msgs
		var resp struct {
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
			Model      string          `json:"model"`
			Usage      struct {
				Input      int64 `json:"input_tokens"`
				Output     int64 `json:"output_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := e.post(ctx, e.cfg.BaseURL+"/v1/messages", hdr, body, &resp); err != nil {
			return text.String(), err
		}
		e.record(resp.Model, meter.Usage{Input: resp.Usage.Input, Output: resp.Usage.Output,
			CacheRead: resp.Usage.CacheRead, CacheWrite: resp.Usage.CacheWrite})
		st := StatsFrom(ctx)
		st.Step(resp.Usage.Input + resp.Usage.CacheRead + resp.Usage.CacheWrite)
		st.Finish(resp.StopReason)
		var blocks []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}
		if err := json.Unmarshal(resp.Content, &blocks); err != nil {
			return text.String(), fmt.Errorf("bad content in the response: %w", err)
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": resp.Content})
		var results []map[string]any
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				text.WriteString(bl.Text)
			case "tool_use":
				raw, _ := json.Marshal(bl.Input)
				out, ok := e.run(ctx, tools, bl.Name, bl.Input, string(raw))
				results = append(results, map[string]any{"type": "tool_result", "tool_use_id": bl.ID, "content": out, "is_error": !ok})
			}
		}
		switch resp.StopReason {
		case "tool_use":
			msgs = append(msgs, map[string]any{"role": "user", "content": results}) // every result in one message
		case "pause_turn":
			// a server-side pause: send the conversation back as it is to continue
		case "refusal":
			return text.String(), errors.New("the model declined the request")
		case "max_tokens":
			return text.String(), errors.New("the response hit max_tokens")
		default: // end_turn, stop_sequence
			return text.String(), nil
		}
	}
	return text.String(), e.maxSteps(ctx)
}

// ---- OpenAI-compatible Chat Completions

func (e *endpoint) openai(ctx context.Context, system, task string, tools []Tool) (string, error) {
	hdr := map[string]string{}
	if k := os.Getenv(e.cfg.APIKeyEnv); k != "" {
		hdr["Authorization"] = "Bearer " + k
	}
	body := map[string]any{"model": e.cfg.Model, "max_tokens": e.cfg.MaxTokens}
	var ts []map[string]any
	for _, t := range tools {
		ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.schema()}})
	}
	if len(ts) > 0 {
		body["tools"] = ts
	}
	var msgs []any
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": task})
	var text strings.Builder
	for range e.cfg.MaxSteps {
		body["messages"] = msgs
		var resp struct {
			Choices []struct {
				Message      json.RawMessage `json:"message"`
				FinishReason string          `json:"finish_reason"`
			} `json:"choices"`
			Model string `json:"model"`
			Usage struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := e.post(ctx, e.cfg.BaseURL+"/chat/completions", hdr, body, &resp); err != nil {
			return text.String(), err
		}
		e.record(resp.Model, meter.Usage{Input: resp.Usage.Prompt, Output: resp.Usage.Completion})
		st := StatsFrom(ctx)
		st.Step(resp.Usage.Prompt)
		if len(resp.Choices) > 0 {
			st.Finish(resp.Choices[0].FinishReason)
		}
		if len(resp.Choices) == 0 {
			return text.String(), errors.New("no choices in the response")
		}
		var m struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"` // a JSON string, not an object
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(resp.Choices[0].Message, &m); err != nil {
			return text.String(), fmt.Errorf("bad message in the response: %w", err)
		}
		msgs = append(msgs, resp.Choices[0].Message)
		if m.Content != nil {
			text.WriteString(*m.Content)
		}
		if len(m.ToolCalls) == 0 {
			switch resp.Choices[0].FinishReason {
			case "length":
				return text.String(), errors.New("the response hit max_tokens")
			case "content_filter":
				return text.String(), errors.New("the model declined the request")
			}
			return text.String(), nil
		}
		for _, tc := range m.ToolCalls {
			var args map[string]any
			out := ""
			raw := strings.TrimSpace(tc.Function.Arguments)
			if raw == "" {
				raw = "{}"
			}
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				out = "error: arguments are not a JSON object: " + err.Error()
				st.Call(tc.Function.Name, raw, CallBadArgs)
				fmt.Fprintf(e.cfg.Log, "warden brain: %s → %s\n", tc.Function.Name, out)
			} else {
				out, _ = e.run(ctx, tools, tc.Function.Name, args, raw)
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": out})
		}
	}
	return text.String(), e.maxSteps(ctx)
}
