package box

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// proxy is an egress allowlist: HTTP CONNECT and plain HTTP on 127.0.0.1.
type proxy struct {
	ln    net.Listener
	srv   *http.Server
	allow []string
	tr    *http.Transport

	mu      sync.Mutex
	refused []string
	conns   map[net.Conn]struct{} // hijacked tunnels, closed with the proxy
}

func startProxy(allow []string) (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("box: egress proxy: %w", err)
	}
	px := &proxy{ln: ln, conns: map[net.Conn]struct{}{},
		tr: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}}
	for _, h := range allow {
		px.allow = append(px.allow, strings.ToLower(strings.TrimSpace(h)))
	}
	px.srv = &http.Server{Handler: px, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = px.srv.Serve(ln) }() // ends with close()
	return px, nil
}

func (px *proxy) addr() string { return px.ln.Addr().String() }
func (px *proxy) port() int    { return px.ln.Addr().(*net.TCPAddr).Port }

func (px *proxy) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = px.srv.Shutdown(ctx)
	px.srv.Close()
	px.mu.Lock()
	for c := range px.conns {
		c.Close()
	}
	px.mu.Unlock()
	px.tr.CloseIdleConnections()
}

func (px *proxy) refusals() []string {
	px.mu.Lock()
	defer px.mu.Unlock()
	return slices.Clone(px.refused)
}

// allowed: an exact host, or "*.suffix" for any subdomain of suffix.
func (px *proxy) allowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, a := range px.allow {
		if a == host || strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
	}
	return false
}

func (px *proxy) refuse(w http.ResponseWriter, target string) {
	px.mu.Lock()
	if !slices.Contains(px.refused, target) {
		px.refused = append(px.refused, target)
	}
	px.mu.Unlock()
	http.Error(w, "warden: egress to "+target+" is not allowed", http.StatusForbidden)
}

func (px *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		px.connect(w, r)
		return
	}
	if r.URL.Host == "" {
		http.Error(w, "warden: not a proxy request", http.StatusBadRequest)
		return
	}
	if !px.allowed(r.URL.Hostname()) {
		px.refuse(w, r.URL.Host)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := px.tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "warden: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // the client went away: nothing to tell it
}

func (px *proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "warden: bad CONNECT target", http.StatusBadRequest)
		return
	}
	if !px.allowed(host) {
		px.refuse(w, r.Host)
		return
	}
	up, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, "warden: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "warden: cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	px.track(up, down)
	defer px.untrack(up, down)
	defer up.Close()
	defer down.Close()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, buf); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(down, up); closeWrite(down); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func (px *proxy) track(cs ...net.Conn) {
	px.mu.Lock()
	defer px.mu.Unlock()
	for _, c := range cs {
		px.conns[c] = struct{}{}
	}
}

func (px *proxy) untrack(cs ...net.Conn) {
	px.mu.Lock()
	defer px.mu.Unlock()
	for _, c := range cs {
		delete(px.conns, c)
	}
}
