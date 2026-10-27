package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A feed that fails shows in the report once per distinct error ("broken:
// 403 Forbidden — will retry next tick"), not on every tick, and says so once
// when it works again. Here the feed refuses requests without Warden's
// User-Agent until it is "fixed" to refuse everyone, then to answer.
func TestBrokenFeedToldOnce(t *testing.T) {
	mode := "ua" // ua: 403 unless the User-Agent starts with warden/; deny: always 403; ok
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "deny" || !strings.HasPrefix(r.UserAgent(), "warden/") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`<rss><channel><item><guid>a</guid><title>Old news</title></item></channel></rss>`))
	}))
	defer srv.Close()
	d := filepath.Join(t.TempDir(), "news")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "domain.toml"), []byte("name = \"news\"\n[[observe]]\nname = \"none\"\nrun = { cmd = [\"true\"] }\n[feeds]\nrss = [\""+srv.URL+"\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, t.TempDir(), `name = "n"
domain = "`+d+`"
mode = "act"
`)
	in := open(t, cfg)
	latest := filepath.Join(cfg.State, "reports", "latest.md")
	tick := func() (string, string) {
		t.Helper()
		tk, err := in.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(latest)
		return strings.Join(tk.Notes, "\n"), string(b)
	}
	broken := "feed " + srv.URL + " broken: 403 Forbidden — will retry next tick"

	if notes, rep := tick(); strings.Contains(notes, "broken") || !strings.Contains(rep, "first look") {
		t.Fatalf("Warden's User-Agent must pass: %s\n%s", notes, rep)
	}
	mode = "deny"
	if _, rep := tick(); !strings.Contains(rep, broken) {
		t.Fatalf("broken feed not in the report:\n%s", rep)
	}
	if notes, rep := tick(); strings.Contains(notes, "broken") || strings.Contains(rep, "broken") {
		t.Fatalf("the same error told twice:\n%s", rep)
	}
	mode = "ok"
	if notes, _ := tick(); !strings.Contains(notes, "feed "+srv.URL+" works again") {
		t.Fatalf("recovery not told: %s", notes)
	}
	mode = "deny"
	if notes, _ := tick(); !strings.Contains(notes, broken) {
		t.Fatalf("broken again after a recovery must be told again: %s", notes)
	}
}
