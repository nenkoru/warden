package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenkoru/warden/internal/brain"
	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
	"github.com/nenkoru/warden/internal/meter"
)

const ghSecret = "github_pat_CORESECRET987654321"

type maint struct {
	fake  *githubtest.Fake
	dir   string
	hooks string // captured hook payloads (one JSON per line)
	cfg   *Config
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newMaint: the github-repo pack against a fake GitHub and a local bare
// remote; hooks append their event JSON to one file. extra is appended to
// warden.toml.
func newMaint(t *testing.T, top, extra string) *maint {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git")
	}
	m := &maint{fake: githubtest.New(t, ghSecret, "warden-bot", "acme/proj"), dir: t.TempDir()}
	m.hooks = filepath.Join(m.dir, "hooks.jsonl")
	tok := filepath.Join(m.dir, "token")
	_ = os.WriteFile(tok, []byte(ghSecret), 0o600)
	bare, seed := filepath.Join(m.dir, "remote.git"), filepath.Join(m.dir, "seed")
	runGit(t, m.dir, "init", "-q", "--bare", "-b", "main", bare)
	runGit(t, m.dir, "init", "-q", "-b", "main", seed)
	_ = os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644)
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-q", "-m", "init")
	runGit(t, seed, "push", "-q", bare, "main")
	runGit(t, m.dir, "clone", "-q", bare, filepath.Join(m.dir, "repo"))
	m.fake.With(func(f *githubtest.Fake) {
		f.Branches["main"] = runGit(t, m.dir, "--git-dir", bare, "rev-parse", "main")
	})
	hook := func(ev string) string {
		return fmt.Sprintf("[[hooks]]\nevent = %q\ncommand = [\"sh\", \"-c\", \"(cat; echo) >> %s\"]\n", ev, m.hooks)
	}
	body := fmt.Sprintf(`name = "proj"
domain = %q
mode = "act"
sandbox = "none"
%s
rules = [{ act = "work-issue", then = "auto" }, { act = "merge-pr", then = "auto" }, { act = "review-pr", then = "auto" }, { act = "release", then = "auto" }, { act = "delegate", then = "auto" }]
[params]
repo = "acme/proj"
token_file = "token"
owner = "boss"
api = %q
root = "repo"
%s
%s
%s`, filepath.Join(packs(t), "github-repo"), top, m.fake.URL(), hook("Report"), hook("Digest"), extra)
	m.cfg = writeConfig(t, m.dir, body)
	return m
}

func (m *maint) payloads(t *testing.T, event string) []map[string]any {
	b, _ := os.ReadFile(m.hooks)
	var out []map[string]any
	for _, l := range strings.Split(string(b), "\n") {
		var p map[string]any
		if json.Unmarshal([]byte(l), &p) == nil {
			if _, isDigest := p["lines"]; isDigest == (event == "Digest") {
				out = append(out, p)
			}
		}
	}
	return out
}

func attentionOf(p map[string]any) string {
	b, _ := json.Marshal(p["attention"])
	return string(b)
}

func noToken(t *testing.T, m *maint) {
	t.Helper()
	m.fake.With(func(f *githubtest.Fake) {
		for _, b := range f.Bodies {
			if strings.Contains(b, ghSecret) {
				t.Fatal("token in a request body")
			}
		}
	})
	_ = filepath.Walk(m.cfg.State, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), ghSecret) {
				t.Fatalf("token in %s", p)
			}
		}
		return nil
	})
	if b, _ := os.ReadFile(m.hooks); strings.Contains(string(b), ghSecret) {
		t.Fatal("token in a hook payload")
	}
}

func TestMaintainerPausedTickDoesNotAct(t *testing.T) {
	m := newMaint(t, "", "")
	n := m.fake.AddPR("fix: x", "warden-bot", "warden/issue-9-x", "own1", []string{"README.md"}, "green")
	m.fake.AddIssue("stop for now", "", "boss", "OWNER", "warden-pause")
	in := open(t, m.cfg)
	tk, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.fake.With(func(f *githubtest.Fake) {
		if len(f.Merged) != 0 {
			t.Fatalf("merged while paused: %v", f.Merged)
		}
	})
	rep, _ := os.ReadFile(filepath.Join(m.cfg.State, "reports", "latest.md"))
	if !strings.Contains(string(rep), "Paused by the owner: issue #2 carries the warden-pause label") {
		t.Fatalf("report:\n%s", rep)
	}
	for _, it := range tk.Items {
		if it.Result != nil {
			t.Fatalf("an act ran while paused: %+v", it.Result)
		}
	}
	// the owner lifts the pause: the next tick merges
	m.fake.With(func(f *githubtest.Fake) { f.Issues[2].Labels = nil })
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.fake.With(func(f *githubtest.Fake) {
		if len(f.Merged) != 1 || f.Merged[0] != n {
			t.Fatalf("not merged after the pause: %v", f.Merged)
		}
	})
	noToken(t, m)
}

func TestMaintainerPauseFailsClosed(t *testing.T) {
	m := newMaint(t, "", "")
	m.fake.AddPR("fix: x", "warden-bot", "x", "own1", []string{"README.md"}, "green")
	m.cfg.Params["token_file"] = filepath.Join(m.dir, "missing")
	in := open(t, m.cfg)
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.paused, "could not be checked") {
		t.Fatalf("an unreadable kill switch must pause: %q", in.paused)
	}
}

func TestMaintainerRedMainNoReleaseAndPing(t *testing.T) {
	m := newMaint(t, "", "")
	m.fake.MergedPR("feat: y", "feat/y", m.fake.Now.Add(-time.Hour))
	var sha string
	m.fake.With(func(f *githubtest.Fake) { sha = f.Branches["main"] })
	m.fake.AddRun(github.WorkflowRun{Name: "CI", HeadBranch: "main", HeadSHA: sha, Event: "push", Status: "completed", Conclusion: "failure", WorkflowID: 1})
	in := open(t, m.cfg)
	if _, err := in.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.fake.With(func(f *githubtest.Fake) {
		for _, p := range f.PRs {
			if strings.HasPrefix(p.Head.Ref, "warden/release-") {
				t.Fatal("a release PR on red main")
			}
		}
	})
	ps := m.payloads(t, "Report")
	if len(ps) != 1 || !strings.Contains(attentionOf(ps[0]), "main is red") {
		t.Fatalf("red main must ping: %v", ps)
	}
	noToken(t, m)
}

func TestMaintainerBudgetCapRefusesBrainActsAndPings(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("the model was called over the cap") }))
	defer llm.Close()
	m := newMaint(t, "", fmt.Sprintf("[brains.claude]\nprovider = \"anthropic\"\nbase_url = %q\nfree = false\n[budget]\nper_day_usd = 1\n", llm.URL))
	m.fake.AddIssue("A bigger change", "please", "boss", "OWNER", "warden")
	in := open(t, m.cfg)
	if in.Ledger == nil {
		t.Fatal("no ledger with [budget]")
	}
	in.Ledger.Record("claude-opus-5-5", meter.Usage{Output: 1_000_000}) // $20: over the $1 cap
	tk, err := in.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var refused *Result
	for _, it := range tk.Items {
		if it.Result != nil && it.Fix != nil && it.Fix.Act == "work-issue" {
			refused = it.Result
		}
	}
	if refused == nil || !refused.Refused || !strings.Contains(refused.Summary, "daily model budget") {
		t.Fatalf("work-issue over the cap: %+v", refused)
	}
	ps := m.payloads(t, "Report")
	if len(ps) != 1 || !strings.Contains(attentionOf(ps[0]), "daily cap") {
		t.Fatalf("the cap must ping: %v", ps)
	}
	rep, _ := os.ReadFile(filepath.Join(m.cfg.State, "reports", "latest.md"))
	if !strings.Contains(string(rep), "model spend today: $20.00 of $1") {
		t.Fatalf("report header:\n%s", rep)
	}
}

func TestMaintainerDigestOncePerDay(t *testing.T) {
	m := newMaint(t, `digest = "daily"`, "")
	m.fake.AddPR("fix: x", "warden-bot", "x", "own1", []string{"README.md"}, "green")
	in := open(t, m.cfg)
	day := time.Date(2026, 10, 27, 10, 0, 0, 0, time.Local)
	for _, at := range []time.Time{day, day.Add(time.Hour), day.Add(24 * time.Hour), day.Add(26 * time.Hour)} {
		in.Now = func() time.Time { return at }
		if _, err := in.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if at.Day() == 27 && len(m.payloads(t, "Digest")) != 0 {
			t.Fatal("a digest before the day ended")
		}
	}
	ds := m.payloads(t, "Digest")
	if len(ds) != 1 || ds[0]["day"] != "2026-10-27" || !strings.Contains(fmt.Sprint(ds[0]["lines"]), "merged PR #1") {
		t.Fatalf("digests: %v", ds)
	}
}

// ---- brains

func TestWindows(t *testing.T) {
	w, err := parseWindows([]string{"00:30-08:00", "sat", "sun"}, "Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"2026-10-27T03:00:00Z": true,  // Tue 06:00 MSK
		"2026-10-27T06:00:00Z": false, // Tue 09:00 MSK
		"2026-10-26T21:29:00Z": false, // Tue 00:29 MSK
		"2026-10-26T21:30:00Z": true,  // Tue 00:30 MSK
		"2026-10-31T12:00:00Z": true,  // Saturday
		"2026-11-01T20:59:00Z": true,  // Sun 23:59 MSK
		"2026-11-01T21:00:00Z": false, // Mon 00:00 MSK
	}
	for at, off := range cases {
		tm, _ := time.Parse(time.RFC3339, at)
		if w.off(tm) != off {
			t.Errorf("%s: off=%v, want %v", at, w.off(tm), off)
		}
	}
	night, _ := parseWindows([]string{"2200-0600"}, "UTC")
	if !night.off(time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)) || night.off(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("across midnight")
	}
	if _, err := parseWindows([]string{"lunch"}, ""); err == nil {
		t.Fatal("a bad window must fail")
	}
}

// fakeOpenAI: an OpenAI-compatible server in the shape of vLLM/LiteLLM: a
// tool call with content null and a reasoning field, then an answer.
func fakeOpenAI(t *testing.T, calls *atomic.Int32, cost string, sawReasoning *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "private thoughts") {
			sawReasoning.Store(true)
		}
		if r.Header.Get("Authorization") != "Bearer key-from-file" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if cost != "" {
			w.Header().Set("x-litellm-response-cost", cost)
		}
		if n%2 == 1 {
			_, _ = io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"reasoning_content":"private thoughts","tool_calls":[{"id":"c1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":20},"extra_field":{"x":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done","reasoning_content":"more"}}],"usage":{"prompt_tokens":150,"completion_tokens":5}}`)
	}))
}

func TestBrainsLocalOffHoursCostAndReasoning(t *testing.T) {
	var calls atomic.Int32
	var saw atomic.Bool
	srv := fakeOpenAI(t, &calls, "0.25", &saw)
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "llm.key"), []byte("key-from-file\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "d", "domain.toml"), []byte("name = \"d\"\n"), 0o644)
	cfg := writeConfig(t, dir, fmt.Sprintf(`domain = "d"
sandbox = "none"
brain_for = { triage = "local", review = "paidlocal" }
[params]
llm_url = %q
[brains.local]
provider = "openai"
base_url = "{llm_url}"
model = "qwen-local"
api_key_file = "llm.key"
free = true
unavailable = ["00:30-08:00", "sat", "sun"]
tz = "Europe/Moscow"
[brains.paidlocal]
provider = "openai"
base_url = "{llm_url}"
model = "claude-via-litellm"
api_key_file = "llm.key"
free = false
[budget]
per_day_usd = 100
free_models = ["qwen-local"]
`, srv.URL))
	in := open(t, cfg)
	tools := []brain.Tool{{Name: "ping", Run: func(context.Context, map[string]any) (string, bool) { return "pong", true }}}
	in.Now = func() time.Time { return time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC) } // 06:00 Moscow, a Tuesday
	if _, err := in.brainFor("triage").Do(context.Background(), "s", "t", tools); err == nil || !strings.Contains(err.Error(), "off-hours") || calls.Load() != 0 {
		t.Fatalf("inside the window: err=%v calls=%d", err, calls.Load())
	}
	if later, why := in.brainReady("triage"); !later || why == nil {
		t.Fatal("off-hours must defer, not refuse")
	}
	in.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) } // Saturday
	if _, err := in.brainFor("triage").Do(context.Background(), "s", "t", tools); err == nil {
		t.Fatal("a weekend must be off")
	}
	in.Now = func() time.Time { return time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC) } // 12:00 Moscow
	out, err := in.brainFor("triage").Do(context.Background(), "s", "t", tools)
	if err != nil || out != "done" || calls.Load() != 2 {
		t.Fatalf("outside the window: %q %v calls=%d", out, err, calls.Load())
	}
	if saw.Load() {
		t.Fatal("the reasoning field was sent back to the model")
	}
	if today, _ := in.Ledger.Spent(); today != 0 {
		t.Fatalf("a free brain spent $%v", today)
	}
	// a paid brain on the same proxy: the header's cost wins over the price table
	if _, err := in.brainFor("review").Do(context.Background(), "s", "t", tools); err != nil {
		t.Fatal(err)
	}
	if today, _ := in.Ledger.Spent(); today < 0.4999 || today > 0.5001 {
		t.Fatalf("spent $%v, want $0.50 from x-litellm-response-cost", today)
	}
	if u := in.usageLine(); !strings.Contains(u, "local 1 call 275 tokens ($0, free)") || !strings.Contains(u, "paidlocal 1 call 275 tokens ($0.50)") {
		t.Fatalf("usage: %s", u)
	}
}

func TestBrainForbiddenRoutesAround(t *testing.T) {
	var claudeCalls, localCalls atomic.Int32
	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeCalls.Add(1)
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"forbidden","message":"Request not allowed"}}`)
	}))
	defer claude.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer local.Close()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "d", "domain.toml"), []byte("name = \"d\"\n"), 0o644)
	cfg := writeConfig(t, dir, fmt.Sprintf(`domain = "d"
sandbox = "none"
brain_for = { triage = "auto" }
brain_auto = ["claude", "local"]
[brains.claude]
provider = "anthropic"
base_url = %q
[brains.local]
provider = "openai"
base_url = %q
model = "m"
`, claude.URL, local.URL))
	in := open(t, cfg)
	if _, err := in.brainFor("triage").Do(context.Background(), "s", "t", nil); err == nil {
		t.Fatal("403 must fail")
	}
	for range 3 {
		if out, err := in.brainFor("triage").Do(context.Background(), "s", "t", nil); err != nil || out != "ok" {
			t.Fatalf("%q %v", out, err)
		}
	}
	if claudeCalls.Load() != 1 || localCalls.Load() != 3 {
		t.Fatalf("no retry storm: claude=%d local=%d", claudeCalls.Load(), localCalls.Load())
	}
	if !strings.Contains(strings.Join(in.notes, "\n"), "brain claude unavailable") {
		t.Fatalf("notes: %v", in.notes)
	}
}

func TestSingleBrainStillWorks(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "d", "domain.toml"), []byte("name = \"d\"\n"), 0o644)
	cfg := writeConfig(t, dir, "domain = \"d\"\nsandbox = \"none\"\n[brain]\nprovider = \"openai\"\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\n")
	in := open(t, cfg)
	if in.Brain == nil || in.defaultBrain != "default" || in.brainFor("work-issue") != in.Brain {
		t.Fatal("[brain] alone is the brain for everything")
	}
	if !isFree(in.slots["default"].cfg) {
		t.Fatal("a loopback endpoint is free by default")
	}
}

func TestParamsArraysAndTables(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "d", "domain.toml"), []byte("name = \"d\"\n"), 0o644)
	cfg := writeConfig(t, dir, "domain = \"d\"\n[params]\nprotected = [\"a/**\", \"b\"]\ndelegate_labels = { infra = \"Ops\" }\ntoken_file = \"tok\"\nn = 3\n")
	if cfg.Params["protected"] != `["a/**","b"]` || cfg.Params["delegate_labels"] != `{"infra":"Ops"}` || cfg.Params["n"] != "3" ||
		cfg.Params["token_file"] != filepath.Join(cfg.Dir, "tok") {
		t.Fatalf("%v", cfg.Params)
	}
}
