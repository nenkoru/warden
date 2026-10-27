package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Some feeds (CISA's among them) refuse requests without a descriptive
// User-Agent; every request Warden makes carries "warden/<version> (+URL)".
func TestFeedSendsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		if !strings.HasPrefix(got, "warden/") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><item><title>A</title><guid>g1</guid></item></channel></rss>`))
	}))
	defer srv.Close()
	if resp, err := http.Get(srv.URL); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the fixture must refuse Go's default User-Agent: %v %v", resp, err)
	}
	items, err := Feed(context.Background(), srv.URL, &Env{Web: &Web{HTTP: srv.Client()}})
	if err != nil || len(items) != 1 {
		t.Fatalf("feed with Warden's User-Agent: %v %v", items, err)
	}
	if !strings.Contains(got, "(+https://github.com/nenkoru/warden)") {
		t.Errorf("User-Agent %q", got)
	}
}
