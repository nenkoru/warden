package box

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func upstream(t *testing.T, tls bool) *httptest.Server {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello from upstream") })
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(h)
	} else {
		s = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s
}

func viaProxy(t *testing.T, px *proxy, base *http.Client) *http.Client {
	pu, _ := url.Parse("http://" + px.addr())
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	if base != nil {
		tr.TLSClientConfig = base.Transport.(*http.Transport).TLSClientConfig
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func get(c *http.Client, u string) (int, string, error) {
	resp, err := c.Get(u)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestProxyAllowed(t *testing.T) {
	px := &proxy{allow: []string{"api.github.com", "*.golang.org"}}
	for host, want := range map[string]bool{
		"api.github.com": true, "API.GitHub.com": true, "github.com": false,
		"proxy.golang.org": true, "a.b.golang.org": true, "golang.org": false, "evilgolang.org": false,
	} {
		if got := px.allowed(host); got != want {
			t.Errorf("allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestProxyPassesAllowed(t *testing.T) {
	px, err := startProxy([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer px.close()

	plain := upstream(t, false)
	code, body, err := get(viaProxy(t, px, nil), plain.URL)
	if err != nil || code != 200 || body != "hello from upstream" {
		t.Fatalf("plain: %d %q %v", code, body, err)
	}
	tls := upstream(t, true)
	code, body, err = get(viaProxy(t, px, tls.Client()), tls.URL)
	if err != nil || code != 200 || body != "hello from upstream" {
		t.Fatalf("connect: %d %q %v", code, body, err)
	}
	if r := px.refusals(); len(r) != 0 {
		t.Fatalf("refusals = %v", r)
	}
}

func TestProxyRefusesOthers(t *testing.T) {
	px, err := startProxy([]string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer px.close()

	plain := upstream(t, false)
	code, _, err := get(viaProxy(t, px, nil), plain.URL)
	if err != nil || code != http.StatusForbidden {
		t.Fatalf("plain: %d %v", code, err)
	}
	tls := upstream(t, true)
	if _, _, err := get(viaProxy(t, px, tls.Client()), tls.URL); err == nil {
		t.Fatal("CONNECT to a host off the list succeeded")
	}
	got := px.refusals()
	want := []string{strings.TrimPrefix(plain.URL, "http://"), strings.TrimPrefix(tls.URL, "https://")}
	if !slices.Equal(got, want) {
		t.Fatalf("refusals = %v, want %v", got, want)
	}
}

func TestRunners(t *testing.T) {
	av := Available()
	if av[len(av)-1] != "none" {
		t.Fatalf("Available() = %v", av)
	}
	missing := "bwrap"
	if runtime.GOOS == "linux" {
		missing = "sandbox-exec"
	}
	if _, err := Run(context.Background(), missing, Policy{}, "", []string{"true"}, nil); err == nil {
		t.Fatalf("named missing runner %s did not fail", missing)
	}
	res, err := Run(context.Background(), "none", Policy{}, t.TempDir(), []string{"sh", "-c", "echo hi"}, nil)
	if err != nil || res.Output != "hi\n" || res.Runner != "none" {
		t.Fatalf("none: %+v %v", res, err)
	}
	res, err = Run(context.Background(), "none", Policy{}, "", []string{"sh", "-c", "echo out; exit 3"}, nil)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || res.Output != "out\n" {
		t.Fatalf("exit: %+v %v", res, err)
	}
}

func TestRealPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS symlinks")
	}
	if got := realPath("/tmp/warden-does-not-exist/x"); got != "/private/tmp/warden-does-not-exist/x" {
		t.Fatalf("realPath = %q", got)
	}
}

func needSandbox(t *testing.T) {
	if !slices.Contains(Available(), "sandbox-exec") {
		t.Skip("sandbox-exec not available")
	}
}

func TestSandboxWrites(t *testing.T) {
	needSandbox(t)
	rw, outside := t.TempDir(), t.TempDir()
	p := Policy{ReadWrite: []string{rw}}
	in := filepath.Join(rw, "in.txt")
	res, err := Run(context.Background(), "auto", p, rw, []string{"sh", "-c", "echo ok > " + in + " && echo tmp > $TMPDIR/t && cat " + in}, nil)
	if err != nil || res.Output != "ok\n" || res.Runner != "sandbox-exec" {
		t.Fatalf("write inside: %+v %v", res, err)
	}
	out := filepath.Join(outside, "out.txt")
	if res, err := Run(context.Background(), "sandbox-exec", p, rw, []string{"sh", "-c", "echo no > " + out}, nil); err == nil {
		t.Fatalf("write outside succeeded: %+v", res)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("file outside ReadWrite was created")
	}
}

func TestSandboxHidesSecrets(t *testing.T) {
	needSandbox(t)
	home, _ := os.UserHomeDir()
	ssh := filepath.Join(home, ".ssh")
	if _, err := os.Stat(ssh); err != nil {
		t.Skip("no ~/.ssh")
	}
	if res, err := Run(context.Background(), "sandbox-exec", Policy{}, "", []string{"ls", ssh}, nil); err == nil {
		t.Fatalf("listing ~/.ssh succeeded: %q", res.Output)
	}
}

func curl(t *testing.T) string {
	c, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	return c
}

func TestSandboxNoNetwork(t *testing.T) {
	needSandbox(t)
	c := curl(t)
	s := upstream(t, false)
	if res, err := Run(context.Background(), "sandbox-exec", Policy{}, "", []string{c, "-sS", "-m", "5", "--noproxy", "*", s.URL}, nil); err == nil {
		t.Fatalf("network with Net=nil: %q", res.Output)
	}
}

func TestSandboxEgressViaProxy(t *testing.T) {
	needSandbox(t)
	c := curl(t)
	s := upstream(t, false)
	// --noproxy "" overrides NO_PROXY so curl goes through the proxy even for 127.0.0.1
	res, err := Run(context.Background(), "sandbox-exec", Policy{Net: []string{"127.0.0.1"}}, "", []string{c, "-sS", "-m", "5", "--noproxy", "", s.URL}, nil)
	if err != nil || res.Output != "hello from upstream" {
		t.Fatalf("allowed: %+v %v", res, err)
	}
	// direct connections stay blocked: only the proxy port is open
	if res, err := Run(context.Background(), "sandbox-exec", Policy{Net: []string{"127.0.0.1"}}, "", []string{c, "-sS", "-m", "5", "--noproxy", "*", s.URL}, nil); err == nil {
		t.Fatalf("direct connection succeeded: %q", res.Output)
	}
	res, err = Run(context.Background(), "sandbox-exec", Policy{Net: []string{"example.com"}}, "", []string{c, "-sS", "-m", "5", "-f", "--noproxy", "", s.URL}, nil)
	if err == nil || len(res.Refused) != 1 || res.Refused[0] != strings.TrimPrefix(s.URL, "http://") {
		t.Fatalf("refused: %+v %v", res, err)
	}
}

func TestAutoFailsClosed(t *testing.T) {
	if Available()[0] == "none" {
		if _, err := Run(context.Background(), "auto", Policy{}, "", []string{"true"}, nil); !errors.Is(err, ErrNoSandbox) {
			t.Fatalf("auto without a runner: %v", err)
		}
		return
	}
	r, err := Pick("auto")
	if err != nil || r == "none" {
		t.Fatalf("auto picked %q %v", r, err)
	}
}

func TestCommand(t *testing.T) {
	argv, env, cleanup, err := Command("none", Policy{Net: []string{"example.com"}}, []string{"echo", "hi"}, []string{"A=1"})
	defer cleanup()
	if err != nil || argv[0] != "echo" || !slices.Contains(env, "A=1") || !strings.Contains(strings.Join(env, " "), "HTTPS_PROXY=http://127.0.0.1:") {
		t.Fatalf("%v %v %v", argv, env, err)
	}
	if slices.Contains(Available(), "sandbox-exec") {
		argv, _, c2, err := Command("auto", Policy{}, []string{"echo", "hi"}, nil)
		defer c2()
		if err != nil || argv[0] != "sandbox-exec" {
			t.Fatalf("%v %v", argv, err)
		}
	}
}
