// Package a2a is a minimal A2A (Agent2Agent) 1.0 JSON-RPC agent and client: text in, text out.
//
// Checked against a2aproject/A2A main (docs/specification.md, specification/a2a.proto, 2026-10-27):
// method names, SendMessageRequest{message}, SendMessageResponse{task|message}, GetTaskRequest{id,historyLength},
// Part{text} without "kind", ROLE_*/TASK_STATE_* enums, AgentCard/AgentInterface/SecurityScheme field names,
// /.well-known/agent-card.json, error codes -32700..-32603, -32001 (task not found), -32005, -32009.
// Assumed (spec is silent): the JSON-RPC error for a missing/bad bearer token (we send -32600 with HTTP 401);
// 0.3 aliases answer in 0.3 shape (bare Task/Message with "kind", lowercase enums); no streaming or push.
package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/warden/internal/version"
)

const (
	WellKnown       = "/.well-known/agent-card.json"
	ProtocolVersion = "1.0"
	maxTasks        = 256
	maxBody         = 1 << 20
)

type Card struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description"`
	Version              string                    `json:"version"`
	SupportedInterfaces  []Interface               `json:"supportedInterfaces"`
	Capabilities         Capabilities              `json:"capabilities"`
	SecuritySchemes      map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []SecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                  `json:"defaultInputModes"`
	DefaultOutputModes   []string                  `json:"defaultOutputModes"`
	Skills               []Skill                   `json:"skills"`
}

type Interface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type Capabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard"`
}

type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

type SecurityScheme struct {
	HTTPAuth *HTTPAuthScheme `json:"httpAuthSecurityScheme,omitempty"`
}

type HTTPAuthScheme struct {
	Description  string `json:"description,omitempty"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

type SecurityRequirement struct {
	Schemes map[string]StringList `json:"schemes"`
}

type StringList struct {
	List []string `json:"list"`
}

// NewCard describes a text-only JSON-RPC agent at url that requires a bearer token.
func NewCard(name, description, url string) Card {
	return Card{
		Name: name, Description: description, Version: version.Version,
		SupportedInterfaces:  []Interface{{URL: url, ProtocolBinding: "JSONRPC", ProtocolVersion: ProtocolVersion}},
		SecuritySchemes:      map[string]SecurityScheme{"bearer": {HTTPAuth: &HTTPAuthScheme{Scheme: "Bearer"}}},
		SecurityRequirements: []SecurityRequirement{{Schemes: map[string]StringList{"bearer": {List: []string{}}}}},
		DefaultInputModes:    []string{"text/plain"},
		DefaultOutputModes:   []string{"text/plain"},
		Skills:               []Skill{{ID: "message", Name: "Message", Description: description, Tags: []string{"chat"}}},
	}
}

type part struct {
	Kind string `json:"kind,omitempty"` // 0.3 only
	Text string `json:"text"`
}

type message struct {
	Kind      string `json:"kind,omitempty"` // 0.3 only
	MessageID string `json:"messageId"`
	ContextID string `json:"contextId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	Role      string `json:"role"`
	Parts     []part `json:"parts"`
}

type artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []part `json:"parts"`
}

type taskStatus struct {
	State     string   `json:"state"`
	Message   *message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

type task struct {
	Kind      string     `json:"kind,omitempty"` // 0.3 only
	ID        string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    taskStatus `json:"status"`
	Artifacts []artifact `json:"artifacts,omitempty"`
	History   []message  `json:"history,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("a2a: %s (%d)", e.Message, e.Code) }

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func textOf(parts []part) string {
	var s []string
	for _, p := range parts {
		if (p.Kind == "" || p.Kind == "text") && p.Text != "" {
			s = append(s, p.Text)
		}
	}
	return strings.Join(s, "\n")
}

// ---- server ----

type stored struct {
	owner string
	t     task
}

type server struct {
	card      []byte
	tokens    map[string]string
	onMessage func(ctx context.Context, from, text string) (string, error)
	mu        sync.Mutex
	tasks     map[string]stored
	order     []string
}

// Handler serves the agent card at WellKnown and JSON-RPC on POST to any other path.
func Handler(card Card, tokens map[string]string, onMessage func(ctx context.Context, from, text string) (reply string, err error)) http.Handler {
	b, _ := json.Marshal(card)
	t := make(map[string]string, len(tokens))
	for k, v := range tokens {
		t[k] = v
	}
	return &server{card: b, tokens: t, onMessage: onMessage, tasks: map[string]stored{}}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == WellKnown && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.card)
	case r.URL.Path != WellKnown && r.Method == http.MethodPost:
		s.rpc(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) sender(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	from, found := "", false
	for k, v := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(k), []byte(tok)) == 1 {
			from, found = v, true
		}
	}
	return from, found
}

func reply(w http.ResponseWriter, status int, id json.RawMessage, result any, e *Error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) rpc(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, nil, nil, &Error{-32600, "request too large"})
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if !json.Valid(body) {
		reply(w, 200, nil, nil, &Error{-32700, "Invalid JSON payload"})
		return
	}
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" {
		reply(w, 200, nil, nil, &Error{-32600, "Request payload validation error"}) // batches unsupported
		return
	}
	if v := r.Header.Get("A2A-Version"); v != "" && v != "1" && !strings.HasPrefix(v, "1.") && !strings.HasPrefix(v, "0.3") {
		reply(w, 200, req.ID, nil, &Error{-32009, "version not supported: " + v})
		return
	}
	from, ok := s.sender(r)
	if !ok {
		reply(w, http.StatusUnauthorized, req.ID, nil, &Error{-32600, "unauthorized: valid bearer token required"})
		return
	}
	legacy := strings.Contains(req.Method, "/")
	var result any
	var e *Error
	switch req.Method {
	case "SendMessage", "message/send":
		result, e = s.send(r.Context(), from, req.Params, legacy)
	case "GetTask", "tasks/get":
		result, e = s.get(from, req.Params, legacy)
	default:
		e = &Error{-32601, "Method not found"}
	}
	reply(w, 200, req.ID, result, e)
}

func (s *server) send(ctx context.Context, from string, params json.RawMessage, legacy bool) (any, *Error) {
	var p struct {
		Message *message `json:"message"`
	}
	if json.Unmarshal(params, &p) != nil || p.Message == nil || len(p.Message.Parts) == 0 {
		return nil, &Error{-32602, "Invalid parameters: message with parts required"}
	}
	text := textOf(p.Message.Parts)
	if text == "" {
		return nil, &Error{-32005, "only text parts are supported"}
	}
	t := task{ID: newID(), ContextID: p.Message.ContextID}
	if t.ContextID == "" {
		t.ContextID = newID()
	}
	in := *p.Message
	in.Kind, in.Role, in.TaskID, in.ContextID = "", "ROLE_USER", t.ID, t.ContextID
	out, err := s.onMessage(ctx, from, text)
	state := "TASK_STATE_COMPLETED"
	if err != nil {
		state, out = "TASK_STATE_FAILED", err.Error()
	}
	msg := message{MessageID: newID(), ContextID: t.ContextID, TaskID: t.ID, Role: "ROLE_AGENT", Parts: []part{{Text: out}}}
	t.Status = taskStatus{State: state, Message: &msg, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if err == nil {
		t.Artifacts = []artifact{{ArtifactID: newID(), Name: "reply", Parts: []part{{Text: out}}}}
	}
	t.History = []message{in, msg}
	s.mu.Lock()
	s.tasks[t.ID] = stored{from, t}
	if s.order = append(s.order, t.ID); len(s.order) > maxTasks {
		delete(s.tasks, s.order[0])
		s.order = s.order[1:]
	}
	s.mu.Unlock()
	if legacy {
		return toLegacy(t), nil
	}
	return map[string]any{"task": t}, nil
}

func (s *server) get(from string, params json.RawMessage, legacy bool) (any, *Error) {
	var p struct {
		ID            string `json:"id"`
		HistoryLength *int   `json:"historyLength"`
	}
	if json.Unmarshal(params, &p) != nil || p.ID == "" {
		return nil, &Error{-32602, "Invalid parameters: id required"}
	}
	s.mu.Lock()
	st, ok := s.tasks[p.ID]
	s.mu.Unlock()
	if !ok || st.owner != from {
		return nil, &Error{-32001, "Task not found"}
	}
	t := st.t
	if n := p.HistoryLength; n != nil && *n >= 0 && *n < len(t.History) {
		t.History = t.History[len(t.History)-*n:]
	}
	if legacy {
		return toLegacy(t), nil
	}
	return t, nil
}

// toLegacy rewrites a task into 0.3 shape: "kind" discriminators and lowercase enums.
func toLegacy(t task) task {
	m := func(in message) message {
		in.Kind = "message"
		in.Role = strings.ToLower(strings.TrimPrefix(in.Role, "ROLE_"))
		in.Parts = append([]part(nil), in.Parts...)
		for i := range in.Parts {
			in.Parts[i].Kind = "text"
		}
		return in
	}
	t.Kind = "task"
	t.Status.State = strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(t.Status.State, "TASK_STATE_")), "_", "-")
	if t.Status.Message != nil {
		sm := m(*t.Status.Message)
		t.Status.Message = &sm
	}
	h := make([]message, len(t.History))
	for i := range t.History {
		h[i] = m(t.History[i])
	}
	t.History = h
	a := make([]artifact, len(t.Artifacts))
	for i, x := range t.Artifacts {
		x.Parts = m(message{Parts: x.Parts}).Parts
		a[i] = x
	}
	t.Artifacts = a
	return t
}

// ---- client ----

// Send delivers text to the agent at url and returns its reply. url may be the JSON-RPC endpoint,
// the agent's base URL, or the card URL; the card (if found) picks the JSONRPC interface.
// Falls back to 0.3 message/send when the card says 0.x or SendMessage is unknown.
func Send(ctx context.Context, url, token, text string) (string, error) {
	endpoint, legacy, err := resolve(ctx, url)
	if err != nil {
		return "", err
	}
	res, err := call(ctx, endpoint, token, legacy, text)
	var e *Error
	if !legacy && errors.As(err, &e) && e.Code == -32601 {
		res, err = call(ctx, endpoint, token, true, text)
	}
	if err != nil {
		return "", err
	}
	return replyText(res)
}

func resolve(ctx context.Context, url string) (endpoint string, legacy bool, err error) {
	isCard := strings.HasSuffix(url, ".json")
	cardURL := url
	if !isCard {
		cardURL = strings.TrimRight(url, "/") + WellKnown
	}
	var c struct {
		SupportedInterfaces []Interface `json:"supportedInterfaces"`
		URL                 string      `json:"url"` // 0.3
		PreferredTransport  string      `json:"preferredTransport"`
		ProtocolVersion     string      `json:"protocolVersion"`
	}
	if err := getJSON(ctx, cardURL, &c); err != nil {
		if isCard {
			return "", false, err
		}
		return url, false, nil
	}
	for _, i := range c.SupportedInterfaces {
		if i.ProtocolBinding == "JSONRPC" && (endpoint == "" || strings.HasPrefix(i.ProtocolVersion, "1.")) {
			endpoint, legacy = i.URL, strings.HasPrefix(i.ProtocolVersion, "0.")
		}
	}
	if endpoint == "" && c.URL != "" && (c.PreferredTransport == "" || c.PreferredTransport == "JSONRPC") {
		endpoint, legacy = c.URL, true
	}
	if endpoint == "" {
		return "", false, fmt.Errorf("a2a: card at %s has no JSONRPC interface", cardURL)
	}
	return endpoint, legacy, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("a2a: GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}

func call(ctx context.Context, endpoint, token string, legacy bool, text string) (json.RawMessage, error) {
	msg := message{MessageID: newID(), Role: "ROLE_USER", Parts: []part{{Text: text}}}
	method := "SendMessage"
	if legacy {
		method, msg.Kind, msg.Role, msg.Parts[0].Kind = "message/send", "message", "user", "text"
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"message": msg}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if !legacy {
		req.Header.Set("A2A-Version", ProtocolVersion)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*maxBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("a2a: POST %s: %s: %w", endpoint, resp.Status, err)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

func replyText(raw json.RawMessage) (string, error) {
	var env struct {
		Task    *task    `json:"task"`
		Message *message `json:"message"`
		Kind    string   `json:"kind"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("a2a: bad result: %w", err)
	}
	switch {
	case env.Task == nil && env.Message == nil && env.Kind == "task":
		env.Task = new(task)
		if err := json.Unmarshal(raw, env.Task); err != nil {
			return "", fmt.Errorf("a2a: bad task: %w", err)
		}
	case env.Task == nil && env.Message == nil && env.Kind == "message":
		env.Message = new(message)
		if err := json.Unmarshal(raw, env.Message); err != nil {
			return "", fmt.Errorf("a2a: bad message: %w", err)
		}
	}
	if env.Message != nil {
		return textOf(env.Message.Parts), nil
	}
	if env.Task == nil {
		return "", errors.New("a2a: result has neither task nor message")
	}
	t := env.Task
	state := strings.ToLower(strings.TrimPrefix(t.Status.State, "TASK_STATE_"))
	status := ""
	if t.Status.Message != nil {
		status = textOf(t.Status.Message.Parts)
	}
	switch state {
	case "failed", "rejected", "canceled":
		return "", fmt.Errorf("a2a: task %s %s: %s", t.ID, state, status)
	}
	var s []string
	for _, a := range t.Artifacts {
		if x := textOf(a.Parts); x != "" {
			s = append(s, x)
		}
	}
	if len(s) > 0 {
		return strings.Join(s, "\n"), nil
	}
	if status == "" && (state == "submitted" || state == "working") {
		return "", fmt.Errorf("a2a: task %s still %s", t.ID, state) // polling GetTask not implemented
	}
	return status, nil
}
