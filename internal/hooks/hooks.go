// Package hooks runs Claude Code-shaped hooks: event + regex matcher +
// command, JSON payload on stdin, exit 2 or {"decision":"block"} blocks.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Events: Wake, Finding, PreAction, PostAction, Ask, Report, Signal.
type Hook struct {
	Event   string   `toml:"event"`
	Matcher string   `toml:"matcher"` // regex on the subject; "" matches all
	Command []string `toml:"command"` // {field} is replaced from the payload's top-level string fields
	Timeout string   `toml:"timeout"` // Go duration, default 30s
	From    string   `toml:"-"`       // extension that brought it
}

type Outcome struct {
	Block  bool
	Reason string
	Ran    []string
	Errors []string
}

const defaultTimeout = 30 * time.Second

var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// Fire runs, in order, every hook for event whose matcher matches subject.
// All matching hooks run even after one blocks; block reasons are joined.
func Fire(ctx context.Context, hooks []Hook, event, subject string, payload any) Outcome {
	var o Outcome
	in, err := json.Marshal(payload)
	if err != nil {
		o.Errors = append(o.Errors, "payload: "+err.Error())
		return o
	}
	fields := map[string]string{"event": event, "subject": subject}
	var top map[string]any
	if json.Unmarshal(in, &top) == nil {
		for k, v := range top {
			if s, ok := v.(string); ok {
				fields[k] = s
			}
		}
	}
	var reasons []string
	for _, h := range hooks {
		if h.Event != event || len(h.Command) == 0 {
			continue
		}
		name := h.label()
		if h.Matcher != "" {
			re, err := regexp.Compile(h.Matcher)
			if err != nil {
				o.Errors = append(o.Errors, fmt.Sprintf("%s: matcher: %v", name, err))
				continue
			}
			if !re.MatchString(subject) {
				continue
			}
		}
		timeout := defaultTimeout
		if h.Timeout != "" {
			if timeout, err = time.ParseDuration(h.Timeout); err != nil || timeout <= 0 {
				o.Errors = append(o.Errors, fmt.Sprintf("%s: bad timeout %q", name, h.Timeout))
				continue
			}
		}
		o.Ran = append(o.Ran, name)
		block, reason, err := run(ctx, h, fields, in, timeout)
		if err != nil {
			o.Errors = append(o.Errors, name+": "+err.Error())
		}
		if block {
			o.Block = true
			if reason == "" {
				reason = "blocked by " + name
			}
			reasons = append(reasons, reason)
		}
	}
	o.Reason = strings.Join(reasons, "; ")
	return o
}

func (h Hook) label() string {
	s := strings.Join(h.Command, " ")
	if h.From != "" {
		s = h.From + ": " + s
	}
	return s
}

func run(ctx context.Context, h Hook, fields map[string]string, in []byte, timeout time.Duration) (block bool, reason string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := make([]string, len(h.Command))
	for i, a := range h.Command {
		argv[i] = placeholder.ReplaceAllStringFunc(a, func(m string) string {
			if v, ok := fields[m[1:len(m)-1]]; ok {
				return v
			}
			return m
		})
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), &stdout, &stderr
	cmd.WaitDelay = time.Second // a backgrounded child holding the pipes can't hang us
	runErr := cmd.Run()

	var d struct{ Decision, Reason string }
	if out := bytes.TrimSpace(stdout.Bytes()); len(out) > 0 && out[0] == '{' && json.Unmarshal(out, &d) == nil && d.Decision == "block" {
		return true, d.Reason, nil
	}
	if runErr == nil {
		return false, "", nil
	}
	msg := strings.TrimSpace(stderr.String())
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return false, "", fmt.Errorf("timed out after %s", timeout)
	case errors.As(runErr, &ee) && ee.ExitCode() == 2:
		return true, msg, nil
	case msg != "":
		return false, "", fmt.Errorf("%w: %s", runErr, msg)
	default:
		return false, "", runErr
	}
}
