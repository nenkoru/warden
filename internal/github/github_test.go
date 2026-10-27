package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenkoru/warden/internal/github"
	"github.com/nenkoru/warden/internal/github/githubtest"
)

const tok = "github_pat_TOPSECRET42"

func tokenFile(t *testing.T, mode os.FileMode) string {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenFile(t *testing.T) {
	if _, err := github.New("", "acme/proj", tokenFile(t, 0o644)); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a world-readable token file must be refused: %v", err)
	}
	if _, err := github.New("", "acme/proj; rm", tokenFile(t, 0o600)); err == nil {
		t.Fatal("a bad repo name must be refused")
	}
	if _, err := github.New("", "acme/proj", ""); err == nil {
		t.Fatal("no token file")
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	// a server that echoes the Authorization header back in its error message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"bad header ` + r.Header.Get("Authorization") + `"}`))
	}))
	defer srv.Close()
	c, err := github.New(srv.URL, "acme/proj", tokenFile(t, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.OpenIssues(context.Background())
	if err == nil || strings.Contains(err.Error(), tok) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error: %v", err)
	}
	if c.Redact("x "+tok+" y") != "x [REDACTED] y" || !c.Contains("a"+tok) {
		t.Fatal("Redact/Contains")
	}
}

func TestClientAgainstFake(t *testing.T) {
	f := githubtest.New(t, tok, "warden-bot", "acme/proj")
	c, err := github.New(f.URL(), "acme/proj", tokenFile(t, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if l, err := c.Login(ctx); err != nil || l != "warden-bot" {
		t.Fatal(l, err)
	}
	n := f.AddIssue("t", "b", "x", "NONE", "bug")
	if err := c.Comment(ctx, n, "token is "+tok); err != nil {
		t.Fatal(err)
	}
	f.With(func(f *githubtest.Fake) {
		if strings.Contains(f.Comments[n][0], tok) {
			t.Fatal("a comment carried the token")
		}
	})
	if _, err := c.PR(ctx, 99); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if ok, err := c.FileExists(ctx, "PAUSE", "main"); ok || err != nil {
		t.Fatal(ok, err)
	}
}
