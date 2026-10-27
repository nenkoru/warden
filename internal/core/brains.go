package core

// Several brains, chosen per task, with a daily budget.
//
//	[brains.local]  provider = "openai", base_url = "http://127.0.0.1:8000/v1", model = "…"  (free: $0, still counted)
//	[brains.claude] acp = ["npx", "-y", "@agentclientprotocol/claude-agent-acp@…"]           (paid: metered)
//	brain_for  = { triage = "local", review-pr = "local", work-issue = "auto" }
//	brain_auto = ["local", "claude"]                                                          (free first)
//	[budget]   per_day_usd = 1000, alert_every_usd = 100, burn_per_hour_usd = 100
//
// Every call goes through a usedBrain: it is refused when the brain is
// off-hours (unavailable = ["00:30-08:00", "sat", "sun"]), when its
// upstream refused it recently (HTTP 403: routed around for a while, one
// report line, no retry storm), or — for a paid brain — when today's budget
// is spent. Endpoint brains are metered through brain.EndpointConfig.Meter
// (a per-call cost from LiteLLM's x-litellm-response-cost header wins over
// the price table); an ACP Claude Code is metered by pointing its
// ANTHROPIC_BASE_URL at the meter's local proxy.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/builtin"
	"github.com/nenkoru/warden/internal/meter"
)

// brainSlot: one configured brain.
type brainSlot struct {
	name string
	cfg  BrainConfig
	raw  brain.Brain
	paid bool
	win  *windows

	mu   sync.Mutex
	cost float64 // the last response's cost from the upstream's header (-1: none)
}

func (s *brainSlot) setCost(c float64) { s.mu.Lock(); s.cost = c; s.mu.Unlock() }

func (s *brainSlot) takeCost() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cost
	s.cost = -1
	return c, c >= 0
}

// BrainUse: one brain's calls today (this process), for the report.
type BrainUse struct {
	Calls, Errors int
	In, Out       int64 // tokens (from the API's usage; approximate for ACP)
	Cost          float64
}

// exactModel: a price-table entry of $1 per million "input tokens", so an
// exact USD cost (LiteLLM's header) can be recorded as micro-dollars.
const exactModel = "warden:exact-usd"

// downFor: how long a brain whose upstream refused it (403) is routed around.
var downFor = 6 * time.Hour

func isFree(b BrainConfig) bool {
	if b.Free != nil {
		return *b.Free
	}
	if b.Provider == "acp" || b.BaseURL == "" {
		return false
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()
}

// openBrains: the budget ledger, then every brain ([brain] is "default").
func (in *Instance) openBrains() error {
	in.usage = map[string]*BrainUse{}
	in.slots = map[string]*brainSlot{}
	if bc := in.Cfg.Budget; bc.PerDayUSD > 0 || bc.AlertEveryUSD > 0 || bc.BurnPerHourUSD > 0 {
		prices := meter.DefaultPrices()
		for k, p := range bc.Prices {
			prices[k] = meter.Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
		}
		for _, m := range bc.FreeModels {
			prices[m] = meter.Price{}
		}
		prices[exactModel] = meter.Price{Input: 1}
		mc := meter.Config{PerDayUSD: bc.PerDayUSD, AlertEveryUSD: bc.AlertEveryUSD, BurnPerHourUSD: bc.BurnPerHourUSD, Prices: prices,
			StateDir: filepath.Join(in.Cfg.State, "meter"), Now: func() time.Time { return in.Now() }}
		if bc.TZ != "" {
			loc, err := time.LoadLocation(bc.TZ)
			if err != nil {
				return fmt.Errorf("budget.tz: %w", err)
			}
			mc.TZ = loc
		}
		l, err := meter.Open(mc)
		if err != nil {
			return err
		}
		in.Ledger = l
	}
	cfgs := map[string]BrainConfig{}
	if in.Cfg.Brain.Provider != "" {
		cfgs["default"] = in.Cfg.Brain
	}
	for n, b := range in.Cfg.Brains {
		cfgs[n] = b
	}
	for _, name := range sortedKeys(cfgs) {
		s, err := in.newSlot(name, cfgs[name])
		if err != nil {
			return err
		}
		if s != nil {
			in.slots[name] = s
		}
	}
	switch {
	case in.slots["default"] != nil:
		in.defaultBrain = "default"
	case in.Cfg.BrainFor["default"] != "" && in.slots[in.Cfg.BrainFor["default"]] != nil:
		in.defaultBrain = in.Cfg.BrainFor["default"]
	default:
		for _, n := range sortedKeys(in.slots) {
			in.defaultBrain = n
			break
		}
	}
	if in.defaultBrain != "" {
		in.Brain = in.used(in.slots[in.defaultBrain])
	}
	return nil
}

func (in *Instance) newSlot(name string, b BrainConfig) (*brainSlot, error) {
	win, err := parseWindows(b.Unavailable, b.TZ)
	if err != nil {
		return nil, err
	}
	s := &brainSlot{name: name, cfg: b, paid: !isFree(b), win: win, cost: -1}
	switch b.Provider {
	case "anthropic", "openai":
		ec := brain.EndpointConfig{Provider: b.Provider, BaseURL: b.BaseURL, Model: b.Model, APIKeyEnv: b.APIKeyEnv, Auth: b.Auth}
		if b.APIKeyFile != "" {
			ec.APIKeyEnv = "WARDEN_KEY_FROM_FILE_" + strings.ToUpper(name) // unset: the transport adds the key from the file
		}
		if err := ec.Normalize(); err != nil {
			return nil, fmt.Errorf("brain %s: %w", name, err)
		}
		ec.HTTP = &http.Client{Timeout: 10 * time.Minute, Transport: &brainTransport{slot: s, base: http.DefaultTransport}}
		ec.Meter = costMeter{in: in, slot: s}
		s.raw = brain.Endpoint(ec)
	case "acp":
		if in.Runner == "" {
			in.note(noSandboxLine("the ACP brain " + name + " (" + b.ACP[0] + ")"))
			return nil, nil
		}
		s.raw = in.acpBrain(name, b, s.paid)
	default:
		return nil, nil
	}
	if b.Quiet != "" {
		q, err := newQuietBrain(s.raw, b.Quiet, b.TZ, func() time.Time { return in.Now() })
		if err != nil {
			return nil, err
		}
		s.raw = q
	}
	return s, nil
}

// ---- the call path

type usedBrain struct {
	in   *Instance
	slot *brainSlot
}

func (in *Instance) used(s *brainSlot) brain.Brain { return &usedBrain{in: in, slot: s} }

func (u *usedBrain) Do(ctx context.Context, system, task string, tools []brain.Tool) (string, error) {
	in, s := u.in, u.slot
	if err := in.ready(s); err != nil {
		return "", err
	}
	out, err := s.raw.Do(ctx, system, task, tools)
	in.mu.Lock()
	use := in.useOf(s.name)
	use.Calls++
	if s.cfg.Provider == "acp" { // no usage from the protocol: approximate
		use.In += int64(len(system)+len(task)) / 4
		use.Out += int64(len(out)) / 4
	}
	if err != nil {
		use.Errors++
	}
	in.mu.Unlock()
	if err != nil && forbidden(err.Error()+" "+out) {
		until := in.Now().Add(downFor)
		in.mu.Lock()
		in.St.BrainDown[s.name] = until.Unix()
		in.mu.Unlock()
		in.noteOnce(fmt.Sprintf("brain %s unavailable: its upstream refused the request (403); routed to the next brain until %s", s.name, until.Format("15:04")))
	}
	return out, err
}

// forbidden: an upstream that refuses this host (a blocked region answers
// 403 "Request not allowed"), not a bad request.
func forbidden(s string) bool {
	return strings.Contains(s, "403") && (strings.Contains(s, "Request not allowed") || strings.Contains(strings.ToLower(s), "forbidden") || strings.Contains(s, "permission_error"))
}

func (in *Instance) useOf(name string) *BrainUse {
	u := in.usage[name]
	if u == nil {
		u = &BrainUse{}
		in.usage[name] = u
	}
	return u
}

// ready: whether a brain may be called now, and why not.
func (in *Instance) ready(s *brainSlot) error {
	now := in.Now()
	in.mu.Lock()
	down := in.St.BrainDown[s.name]
	in.mu.Unlock()
	if down > now.Unix() {
		return fmt.Errorf("brain %s unavailable (its upstream refused it) until %s", s.name, time.Unix(down, 0).Format("15:04"))
	}
	if s.win.off(now) {
		return fmt.Errorf("brain %s off-hours (%s): waiting", s.name, s.win)
	}
	if s.paid && in.Ledger != nil {
		if err := in.Ledger.Allow(0); err != nil {
			return fmt.Errorf("brain %s: %w — no paid model calls until the day ends", s.name, err)
		}
	}
	return nil
}

// ---- routing

// route: the brain name configured for a task ("" = the default, "auto").
func (in *Instance) route(task string) string {
	r := in.Cfg.BrainFor[task]
	if r == "default" {
		return ""
	}
	return r
}

// ladder: what "auto" climbs: brain_auto, else free brains then paid ones.
func (in *Instance) ladder() []string {
	if len(in.Cfg.BrainAuto) > 0 {
		return in.Cfg.BrainAuto
	}
	var free, paid []string
	for _, n := range sortedKeys(in.slots) {
		if in.slots[n].paid {
			paid = append(paid, n)
		} else {
			free = append(free, n)
		}
	}
	return append(free, paid...)
}

// brainFor: the brain for one of Warden's own judgement calls (triage,
// interpret, …): the routed one, "auto" → the first ready on the ladder.
func (in *Instance) brainFor(task string) brain.Brain {
	name := in.route(task)
	if name == "auto" {
		for _, n := range in.ladder() {
			if s := in.slots[n]; s != nil && in.ready(s) == nil {
				return in.used(s)
			}
		}
		name = ""
	}
	if name == "" {
		return in.Brain
	}
	if s := in.slots[name]; s != nil {
		return in.used(s)
	}
	return in.Brain
}

// brainReady: whether an act that needs a brain can have one now. A spent
// budget refuses (no more model calls today); off-hours or an unreachable
// upstream defers to a later tick.
func (in *Instance) brainReady(task string) (deferred bool, why error) {
	if len(in.slots) == 0 {
		if in.Brain != nil { // a brain set by a test or an embedder
			return false, nil
		}
		return false, errors.New("no brain configured")
	}
	var names []string
	switch r := in.route(task); r {
	case "auto":
		names = in.ladder()
	case "":
		names = []string{in.defaultBrain}
	default:
		names = []string{r}
	}
	var whys []string
	budget := false
	for _, n := range names {
		s := in.slots[n]
		if s == nil {
			continue
		}
		err := in.ready(s)
		if err == nil {
			return false, nil
		}
		budget = budget || errors.Is(err, meter.ErrOverBudget)
		whys = append(whys, err.Error())
	}
	if len(whys) == 0 {
		return false, errors.New("no brain configured for " + task)
	}
	return !budget, errors.New(strings.Join(whys, "; "))
}

// envBrains: the routing for built-in acts.
func (in *Instance) envBrains() *builtin.Brains {
	if len(in.slots) == 0 {
		return nil
	}
	return &builtin.Brains{
		Route: in.route,
		Auto:  in.ladder(),
		Get: func(name string) (builtin.NamedBrain, error) {
			if name == "" {
				name = in.defaultBrain
			}
			s := in.slots[name]
			if s == nil {
				return builtin.NamedBrain{Name: name}, fmt.Errorf("no brain %q", name)
			}
			nb := builtin.NamedBrain{Name: name, Brain: in.used(s), Paid: s.paid}
			return nb, in.ready(s)
		},
	}
}

// ---- the report's lines

// spendLine: "model spend today: $X of $Y".
func (in *Instance) spendLine() string {
	if in.Ledger == nil {
		return ""
	}
	today, hour := in.Ledger.Spent()
	s := fmt.Sprintf("model spend today: $%.2f", today)
	if cap := in.Cfg.Budget.PerDayUSD; cap > 0 {
		s += fmt.Sprintf(" of $%.0f", cap)
	}
	if hour > 0 {
		s += fmt.Sprintf(" ($%.2f in the last hour)", hour)
	}
	return s
}

// usageLine: every brain's calls this tick: "local 3 calls ≈12k tokens $0 · claude 1 call $0.42".
func (in *Instance) usageLine() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	var parts []string
	for _, n := range sortedKeys(in.usage) {
		u := in.usage[n]
		if u.Calls == 0 {
			continue
		}
		s := fmt.Sprintf("%s %d call%s", n, u.Calls, map[bool]string{true: "", false: "s"}[u.Calls == 1])
		if u.In+u.Out > 0 {
			s += fmt.Sprintf(" %s tokens", kTokens(u.In+u.Out))
		}
		if sl := in.slots[n]; sl != nil && !sl.paid {
			s += " ($0, free)"
		} else {
			s += fmt.Sprintf(" ($%.2f)", u.Cost)
		}
		if u.Errors > 0 {
			s += fmt.Sprintf(", %d failed", u.Errors)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

func kTokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.FormatInt(n, 10)
}

// ---- metering for endpoint brains (brain.Meter)

type costMeter struct {
	in   *Instance
	slot *brainSlot
}

func (m costMeter) free(model string) bool {
	return !m.slot.paid || slices.Contains(m.in.Cfg.Budget.FreeModels, model)
}

func (m costMeter) Estimate(model string, maxTokens, inputBytes int) float64 {
	if m.in.Ledger == nil || m.free(model) {
		return 0
	}
	return m.in.Ledger.Estimate(model, maxTokens, inputBytes)
}

func (m costMeter) Allow(estimate float64) error {
	if m.in.Ledger == nil || !m.slot.paid {
		return nil
	}
	return m.in.Ledger.Allow(estimate)
}

func (m costMeter) Record(model string, u meter.Usage) (float64, []meter.Alert) {
	in := m.in
	exact, hasExact := m.slot.takeCost()
	var cost float64
	var alerts []meter.Alert
	switch {
	case in.Ledger == nil || m.free(model):
	case hasExact:
		cost, alerts = in.Ledger.Record(exactModel, meter.Usage{Input: int64(math.Round(exact * 1e6))})
	default:
		cost, alerts = in.Ledger.Record(model, u)
	}
	in.mu.Lock()
	use := in.useOf(m.slot.name)
	use.In += u.Input + u.CacheRead + u.CacheWrite
	use.Out += u.Output
	use.Cost += cost
	in.mu.Unlock()
	return cost, alerts
}

// brainTransport: the API key from api_key_file at call time; LiteLLM's
// per-call cost header kept for the meter; an OpenAI-compatible server's
// reasoning fields dropped from responses so they are never sent back.
type brainTransport struct {
	slot *brainSlot
	base http.RoundTripper
}

func (t *brainTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cfg := t.slot.cfg
	if cfg.APIKeyFile != "" {
		b, err := os.ReadFile(cfg.APIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("brain %s: api_key_file: %w", t.slot.name, errors.Unwrap(err))
		}
		key := strings.TrimSpace(string(b))
		req = req.Clone(req.Context())
		switch {
		case cfg.Provider == "anthropic" && cfg.Auth != "bearer":
			req.Header.Set("x-api-key", key)
		default:
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if v := resp.Header.Get("x-litellm-response-cost"); v != "" {
		if c, err := strconv.ParseFloat(v, 64); err == nil && c >= 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
			t.slot.setCost(c)
		}
	}
	if cfg.Provider == "openai" && resp.StatusCode == 200 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		body = stripReasoning(body)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Del("Content-Length")
	}
	return resp, nil
}

// stripReasoning drops reasoning_content / reasoning from every choice's
// message (vLLM, LiteLLM): the model's scratchpad is not echoed back.
func stripReasoning(body []byte) []byte {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return body
	}
	choices, _ := v["choices"].([]any)
	changed := false
	for _, c := range choices {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		for _, k := range []string{"reasoning_content", "reasoning", "thinking"} {
			if _, ok := msg[k]; ok {
				delete(msg, k)
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	b, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return b
}

// ---- the metering proxy for an ACP agent

// meterProxy: the ledger's local Anthropic proxy (started once), and the
// loopback port the sandbox must let the agent reach.
func (in *Instance) meterProxy() (string, int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.proxyAddr != "" {
		return in.proxyAddr, in.proxyPort, nil
	}
	upstream := in.Cfg.Budget.Upstream
	if upstream == "" {
		upstream = "https://api.anthropic.com"
	}
	if p := in.Cfg.Budget.UpstreamProxy; p != "" {
		fwd, err := forwarder(upstream, p)
		if err != nil {
			return "", 0, err
		}
		in.cleanups = append(in.cleanups, func() { _ = fwd.Close() })
		upstream = "http://" + fwd.Addr().String()
	}
	px, err := in.Ledger.StartProxy(upstream)
	if err != nil {
		return "", 0, err
	}
	in.cleanups = append(in.cleanups, func() { _ = px.Close() })
	_, port, _ := net.SplitHostPort(px.Addr)
	in.proxyAddr = px.Addr
	in.proxyPort, _ = strconv.Atoi(port)
	return in.proxyAddr, in.proxyPort, nil
}

type fwdServer struct {
	ln  net.Listener
	srv *http.Server
}

func (f *fwdServer) Addr() net.Addr { return f.ln.Addr() }
func (f *fwdServer) Close() error   { return f.srv.Close() }

// forwarder: a loopback reverse proxy to upstream that goes out through an
// HTTPS proxy (for a host whose direct route to the API is blocked).
func forwarder(upstream, via string) (*fwdServer, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("budget.upstream: %q", upstream)
	}
	pu, err := url.Parse(via)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("budget.upstream_proxy: %q", via)
	}
	rp := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(u); pr.Out.Host = u.Host },
		Transport:     &http.Transport{Proxy: http.ProxyURL(pu), ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second},
		FlushInterval: -1,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &fwdServer{ln: ln, srv: &http.Server{Handler: rp, ReadHeaderTimeout: 30 * time.Second}}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// ---- availability windows

// windows: when a brain must not be called.
type windows struct {
	spans [][2]int // minutes of the day [from, to), across midnight when from > to
	days  map[time.Weekday]bool
	loc   *time.Location
	text  string
}

var dayNames = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func parseWindows(specs []string, tz string) (*windows, error) {
	w := &windows{days: map[time.Weekday]bool{}, loc: time.Local}
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("tz: %w", err)
		}
		w.loc = l
	}
	for _, s := range specs {
		s = strings.ToLower(strings.TrimSpace(s))
		if d, ok := dayNames[s[:min(3, len(s))]]; ok && !strings.ContainsAny(s, "0123456789") {
			w.days[d] = true
			continue
		}
		f, t, ok := strings.Cut(s, "-")
		from, err1 := hhmm(strings.ReplaceAll(strings.TrimSpace(f), ":", ""))
		to, err2 := hhmm(strings.ReplaceAll(strings.TrimSpace(t), ":", ""))
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%q is neither HH:MM-HH:MM nor a weekday", s)
		}
		w.spans = append(w.spans, [2]int{from, to})
	}
	var parts []string
	for _, sp := range w.spans {
		parts = append(parts, fmt.Sprintf("%02d:%02d–%02d:%02d", sp[0]/60, sp[0]%60, sp[1]/60, sp[1]%60))
	}
	var ds []string
	for d := range w.days {
		ds = append(ds, d.String()[:3])
	}
	sort.Strings(ds)
	parts = append(parts, ds...)
	w.text = strings.Join(parts, ", ") + " " + w.loc.String()
	return w, nil
}

func (w *windows) off(t time.Time) bool {
	if w == nil {
		return false
	}
	t = t.In(w.loc)
	if w.days[t.Weekday()] {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	for _, sp := range w.spans {
		if sp[0] <= sp[1] && m >= sp[0] && m < sp[1] || sp[0] > sp[1] && (m >= sp[0] || m < sp[1]) {
			return true
		}
	}
	return false
}

func (w *windows) String() string {
	if w == nil {
		return ""
	}
	return w.text
}
