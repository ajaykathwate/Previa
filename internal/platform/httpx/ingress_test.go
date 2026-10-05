package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func serve(h http.Handler, method, target string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestClientIPTrustsForwardedHeadersOnlyFromProxies(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := New(quiet, Options{TrustedProxies: proxies})
	var seen string
	r.Get("/", func(_ http.ResponseWriter, r *http.Request) { seen = ClientIP(r) })
	for _, c := range []struct{ peer, forwarded, want string }{
		{"203.0.113.9:4000", "198.51.100.1", "203.0.113.9"},                       // untrusted peer: header ignored
		{"10.1.2.3:4000", "198.51.100.1", "198.51.100.1"},                         // trusted proxy
		{"10.1.2.3:4000", "198.51.100.7, 198.51.100.1, 10.9.9.9", "198.51.100.1"}, // nearest untrusted hop
		{"10.1.2.3:4000", "not-an-ip", "10.1.2.3"},                                // garbage stops at the proxy
	} {
		serve(r, "GET", "http://previa.test/", func(req *http.Request) {
			req.RemoteAddr = c.peer
			req.Header.Set("X-Forwarded-For", c.forwarded)
		})
		if seen != c.want {
			t.Fatalf("peer %s forwarded %q: client %s, want %s", c.peer, c.forwarded, seen, c.want)
		}
	}
}

func TestHostValidation(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := New(quiet, Options{AppHost: "previa.example.com", TrustedProxies: proxies, HostExempt: []string{"/webhooks/github"}})
	r.Get("/", func(http.ResponseWriter, *http.Request) {})
	r.Post("/webhooks/github", func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name, target, method, peer, forwarded string
		want                                  int
	}{
		{"app host", "https://previa.example.com/", "GET", "203.0.113.9:1", "", 200},
		{"explicit default port", "https://previa.example.com:443/", "GET", "203.0.113.9:1", "", 200},
		{"other host", "https://evil.example/", "GET", "203.0.113.9:1", "", http.StatusMisdirectedRequest},
		{"preview host", "https://pr-1-a-b.preview.example.net/", "GET", "203.0.113.9:1", "", http.StatusMisdirectedRequest},
		{"exempt webhook through a tunnel", "https://tunnel.example/webhooks/github", "POST", "203.0.113.9:1", "", 200},
		{"forwarded mismatch via proxy", "https://previa.example.com/", "GET", "10.0.0.2:1", "evil.example", http.StatusMisdirectedRequest},
		{"forwarded match via proxy", "https://previa.example.com/", "GET", "10.0.0.2:1", "previa.example.com", 200},
		{"forwarded from a stranger is ignored", "https://previa.example.com/", "GET", "203.0.113.9:1", "evil.example", 200},
	}
	for _, c := range cases {
		w := serve(r, c.method, c.target, func(req *http.Request) {
			req.RemoteAddr = c.peer
			if c.forwarded != "" {
				req.Header.Set("X-Forwarded-Host", c.forwarded)
			}
		})
		if w.Code != c.want {
			t.Fatalf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestSecurityHeadersAndNonce(t *testing.T) {
	r := New(quiet, Options{Secure: true})
	var nonce string
	r.Get("/", func(_ http.ResponseWriter, r *http.Request) {
		nonce = templ.GetNonce(r.Context())
		if Nonce(r.Context()) != nonce {
			t.Error("nonce accessors disagree")
		}
	})
	w := serve(r, "GET", "/", nil)
	csp := w.Header().Get("Content-Security-Policy")
	if nonce == "" || !strings.Contains(csp, "'nonce-"+nonce+"'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("policy %q with nonce %q", csp, nonce)
	}
	for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Strict-Transport-Security": "max-age=31536000"} {
		if got := w.Header().Get(header); got != want {
			t.Fatalf("%s = %q", header, got)
		}
	}
	other := serve(r, "GET", "/", nil).Header().Get("Content-Security-Policy")
	if other == csp {
		t.Fatal("the nonce did not change between requests")
	}
	if serve(New(quiet, Options{}), "GET", "/", nil).Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS over plain HTTP")
	}
}

func TestBodyLimit(t *testing.T) {
	r := New(quiet, Options{MaxBodyBytes: 10, BodyExempt: []string{"/webhooks/github"}})
	var readErr error
	handler := func(_ http.ResponseWriter, r *http.Request) { _, readErr = io.ReadAll(r.Body) }
	r.Post("/form", handler)
	r.Post("/webhooks/github", handler)
	post := func(target, body string, length int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(body))
		req.ContentLength = length
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := post("/form", strings.Repeat("x", 20), 20); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize body: %d", w.Code)
	}
	post("/form", strings.Repeat("x", 20), -1) // unknown length: cut off while reading
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("streamed oversize body read: %v", readErr)
	}
	post("/webhooks/github", strings.Repeat("x", 20), 20)
	if readErr != nil {
		t.Fatalf("an exempt path was limited: %v", readErr)
	}
}

func TestRateLimitPerClient(t *testing.T) {
	h := RateLimit([]string{"/auth/"}, time.Minute, 2)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	from := func(target, ip string) int {
		return serve(h, "GET", target, func(r *http.Request) { r.RemoteAddr = ip + ":1" }).Code
	}
	for range 2 { // the burst
		if from("/auth/github/login", "203.0.113.1") != 200 {
			t.Fatal("burst refused")
		}
	}
	w := serve(h, "GET", "/auth/github/callback", func(r *http.Request) { r.RemoteAddr = "203.0.113.1:1" })
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if from("/auth/github/login", "203.0.113.2") != 200 {
		t.Fatal("another client was limited")
	}
	for range 10 {
		if from("/repositories", "203.0.113.1") != 200 {
			t.Fatal("an unlimited path was limited")
		}
	}
}

func TestRateLimiterStaysBounded(t *testing.T) {
	now := time.Unix(0, 0)
	l := &ipLimiter{every: 1, burst: 1, max: 3, idle: time.Minute, clients: map[string]*ipEntry{}, now: func() time.Time { return now }}
	for i := range 50 {
		l.allow(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}).String())
		if len(l.clients) > 3 {
			t.Fatalf("%d clients tracked", len(l.clients))
		}
	}
}

func TestLimitConcurrency(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	h := LimitConcurrency(2, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); serve(h, "POST", "/webhooks/github", nil) }()
	}
	<-entered
	<-entered
	if w := serve(h, "POST", "/webhooks/github", nil); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("third concurrent request: %d", w.Code)
	}
	close(release)
	wg.Wait()
	go func() { <-entered }()
	if w := serve(h, "POST", "/webhooks/github", nil); w.Code != 200 {
		t.Fatalf("after the burst: %d", w.Code)
	}
}

func TestInternalEndpoints(t *testing.T) {
	failing := errors.New("dial postgres://user:pw@db/previa: refused")
	broken := false
	h := Internal([]Check{{Name: "database", Run: func(ctx context.Context) error {
		if broken {
			return failing
		}
		return nil
	}}}, func() map[string]any { return map[string]any{"capabilities": map[string]bool{"execution": true}} })
	if w := serve(h, "GET", "/livez", nil); w.Code != 200 {
		t.Fatalf("livez %d", w.Code)
	}
	w := serve(h, "GET", "/readyz", nil)
	var body struct {
		Status       string            `json:"status"`
		Checks       map[string]string `json:"checks"`
		Capabilities map[string]bool   `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Status != "ready" || !body.Capabilities["execution"] {
		t.Fatalf("readyz %d %s", w.Code, w.Body.String())
	}
	broken = true
	w = serve(h, "GET", "/readyz", nil)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "pw@") || !strings.Contains(w.Body.String(), "not_ready") {
		t.Fatalf("failing readyz %d %s", w.Code, w.Body.String())
	}
	if w := serve(h, "GET", "/metrics", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "go_goroutines") {
		t.Fatalf("metrics %d", w.Code)
	}

	stop, err := ServeInternal("127.0.0.1:0", h, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if err := Probe("127.0.0.1:1"); err == nil {
		t.Fatal("a probe of a closed port succeeded")
	}
}

func TestProbe(t *testing.T) {
	ready := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"checks":{"database":"down"}}`))
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := Probe(addr); err != nil {
		t.Fatal(err)
	}
	ready = false
	if err := Probe(addr); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("not-ready probe: %v", err)
	}
}
