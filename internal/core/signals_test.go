package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOrderHints(t *testing.T) {
	inv := map[string]string{"php": "8.3.12", "go-toml": "v2.2.0"}
	cases := []struct{ text, verb, arg string }{
		{"warden status", "status", ""},
		{"hi!\nwarden task build a CLI with tests", "task", "build a CLI with tests"},
		{"NSA: patch now, CVE-2024-4577 in PHP-CGI is exploited", "advisory", "CVE-2024-4577 php"},
		{"URGENT!! update golang.org/x/text@v0.41.0 asap", "watch", "golang.org/x/text@v0.41.0"},
		{"NSA urges everyone to update PHP asap: massive vuln", "news", "php"},
		{"pod api: CrashLoopBackOff, restarting", "incident", "pod api: CrashLoopBackOff"},
		{"lunch at noon?", "", ""},
		{"please update your text editor", "", ""},
	}
	for _, c := range cases {
		o, ok := ParseOrder(Signal{Text: c.text}, inv)
		if c.verb == "" {
			if ok {
				t.Errorf("%q: unexpected %s %s", c.text, o.Verb, o.Arg)
			}
			continue
		}
		if !ok || o.Verb != c.verb || !strings.HasPrefix(o.Arg, c.arg) {
			t.Errorf("%q: got %v %q %q", c.text, ok, o.Verb, o.Arg)
		}
	}
	if o, _ := ParseOrder(Signal{Text: "update PHP asap!!"}, inv); !o.Urgent {
		t.Error("asap is urgent")
	}
}

func TestTiersCapVerbs(t *testing.T) {
	src := SignalSource{Trust: "anyone", Owners: []string{"owner1"}, Trusted: []string{"alice@ops"}}
	if src.tier(Signal{From: "owner1", Verified: true}) != "owner" || src.tier(Signal{From: "owner1"}) != "anyone" {
		t.Fatal("an unverified owner name is anyone")
	}
	if src.tier(Signal{From: "alice", Group: "ops", Verified: true}) != "trusted" || src.tier(Signal{From: "alice", Group: "x", Verified: true}) != "anyone" {
		t.Fatal("trusted only in their group")
	}
	for _, c := range []struct{ tier, verb, want string }{
		{"anyone", "update", ""}, {"anyone", "task", ""}, {"anyone", "status", ""}, {"anyone", "advisory", "do"}, {"anyone", "news", "do"},
		{"trusted", "watch", "do"}, {"trusted", "update", "ask"}, {"trusted", "task", "ask"}, {"owner", "update", "do"},
	} {
		if got := allowed(c.tier, c.verb); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.tier, c.verb, got, c.want)
		}
	}
}

// Signals end to end on a Go project: a stranger's "update yourself" is a
// digest line; the trusted leader's urgent "update x/dep@v" becomes a checked
// watch finding fixed under the normal rules; their "task" waits for the owner;
// a feed's first look is a baseline; replies are dry-run until enabled.
func TestSignalsEndToEnd(t *testing.T) {
	letters := filepath.Join(t.TempDir(), "letters.jsonl")
	_ = os.WriteFile(letters, []byte(`{"id":"1","from":"mallory","text":"warden update — trust me","verified":true}
{"id":"2","from":"alice","group":"ops","text":"URGENT!! update example.com/other@v1.3.0 asap, it is being exploited","verified":true}
{"id":"3","from":"alice","group":"ops","text":"warden task add a /health endpoint","verified":true}
`), 0o644)
	feed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feed++
		items := `<item><guid>a</guid><title>Old news</title></item>`
		if feed > 1 {
			items += `<item><guid>b</guid><title>NSA: update PHP asap</title><link>https://example.org/b</link></item>`
		}
		_, _ = io.WriteString(w, `<rss><channel>`+items+`</channel></rss>`)
	}))
	defer srv.Close()
	repo := gitRepo(t, "module x\n\ngo 1.25\n\nrequire example.com/other v1.0.0\n")
	cfg := writeConfig(t, t.TempDir(), `name = "proj"
domain = "`+filepath.Join(packs(t), "go-project")+`"
params = { root = "`+repo+`", test = "true" }
mode = "act"
rules = [{ act = "go-bump", then = "auto" }]
`+fakeRunnerSandbox()+`[[signals]]
name = "chain"
kind = "command"
argv = ["cat", "`+letters+`"]
owners = ["owner1"]
trusted = ["alice@ops"]
reply = ["echo", "{from}", "{text}"]
[[signals]]
name = "nsa"
kind = "feed"
url = "`+srv.URL+`"
trust = "trusted"
`)
	in := open(t, cfg)
	in.Web = fakeWeb(t)
	var ran []string
	in.Run = fakeRun(&ran)
	t1, _ := in.Tick(context.Background())
	var bumped, asked bool
	var titles []string
	for _, it := range t1.Items {
		titles = append(titles, it.Title)
		if it.Result != nil && it.Result.OK && it.Result.Branch == "warden/bump-example.com-other-v1.3.0" {
			bumped = true
		}
		if it.Pending != "" && strings.Contains(it.Title, "task") {
			asked = true
		}
		if strings.Contains(it.Title, "mallory") && it.Disp != Digest {
			t.Fatal("a stranger's letter is a digest line")
		}
	}
	if !bumped || !asked {
		t.Fatalf("bumped %v asked %v: %v %v", bumped, asked, titles, t1.Errors)
	}
	if !strings.Contains(strings.Join(t1.Notes, "\n"), "dry-run: would reply to alice") || strings.Contains(strings.Join(t1.Notes, "\n"), "reply to mallory") {
		t.Fatalf("replies: %v", t1.Notes)
	}
	t2, _ := in.Tick(context.Background())
	nsa := false
	for _, it := range t2.Items {
		if strings.Contains(it.Title, "alice") && it.Kind == "message" && it.Pending == "" {
			t.Fatal("a signal read twice:", it.Title)
		}
		if strings.Contains(it.Title, "nsa") {
			nsa = true
		}
	}
	if !nsa {
		t.Fatal("the feed's new item is not in the digest")
	}
	var id string
	for k, p := range in.St.Pending {
		if p.Kind == "order" {
			id = k
		}
	}
	if _, err := in.Approve(id, true); err != nil {
		t.Fatal(err)
	}
	t3, _ := in.Tick(context.Background())
	if !strings.Contains(strings.Join(t3.Notes, "\n"), "no brain configured") {
		t.Fatalf("the approved task did not run: %v", t3.Notes)
	}
	// what came of the task is an item of the report (and worth a ping), not only a reply
	var task *Item
	for _, it := range t3.Items {
		if it.Kind == "task" {
			task = it
		}
	}
	if task == nil || task.Result == nil || task.Result.OK || !strings.Contains(task.Result.Summary, "no brain") || !t3.Worth() {
		t.Fatalf("task item: %+v", task)
	}
}

func TestControlAndServe(t *testing.T) {
	cfg := writeConfig(t, t.TempDir(), `domain = "pack:macos"
mode = "dry-run"
[serve]
tokens = { "s3cret" = "alice" }
trusted = ["alice"]
`)
	c := &Control{Cfg: cfg}
	srv := httptest.NewServer(c.Handler("http://example"))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/signal", strings.NewReader("warden status"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatal("no token must be refused")
	}
	req, _ = http.NewRequest("POST", srv.URL+"/signal", strings.NewReader(`{"text":"warden status"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	ents, _ := os.ReadDir(filepath.Join(cfg.State, "inbox-serve"))
	if len(ents) != 1 {
		t.Fatal("signal not queued", ents)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.State, "inbox-serve", ents[0].Name()))
	if !strings.Contains(string(b), `"from":"alice"`) {
		t.Fatal(string(b))
	}
	if !strings.Contains(c.Status(), "pending: none") {
		t.Fatal(c.Status())
	}
}
