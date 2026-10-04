package egress

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeHTTPAndTCPReadiness(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target := strings.TrimPrefix(server.URL, "http://")
	ctx := t.Context()
	if err := Probe(ctx, target, "/health", time.Second); err != nil {
		t.Fatalf("healthy app: %v", err)
	}
	// A redirect counts as a response; the probe never follows it anywhere.
	before := hits
	if err := Probe(ctx, target, "/redirect", time.Second); err != nil || hits != before+1 {
		t.Fatalf("redirect: %v (hits %d)", err, hits-before)
	}
	if err := Probe(ctx, target, "/broken", time.Second); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("unhealthy app: %v", err)
	}
	if err := Probe(ctx, target, "", time.Second); err != nil {
		t.Fatalf("TCP fallback: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	if err := Probe(ctx, closed, "", 500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "nothing is accepting connections") {
		t.Fatalf("closed port: %v", err)
	}
	for _, bad := range []struct{ target, path string }{
		{"example.com:80", "/"}, {"127.0.0.1", "/"}, {"127.0.0.1:0", "/"},
		{target, "health"}, {target, "//evil.example/x"}, {target, "/x?y=1"}, {target, "/x y"},
	} {
		if err := Probe(ctx, bad.target, bad.path, time.Second); err == nil {
			t.Fatalf("accepted target %q path %q", bad.target, bad.path)
		}
	}
}
