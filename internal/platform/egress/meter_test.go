package egress

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidHost(t *testing.T) {
	for entry, want := range map[string]bool{
		"registry.npmjs.org": true, ".pythonhosted.org": true, "proxy.golang.org": true,
		"": false, "localhost": false, "Registry.npmjs.org": false, "*.example.com": false,
		"10.0.0.1": false, "example.com:443": false, "exa mple.com": false, "..example.com": false,
	} {
		if got := ValidHost(entry); got != want {
			t.Errorf("ValidHost(%q) = %v, want %v", entry, got, want)
		}
	}
}

// A metered build network counts its traffic both ways, reaches only its
// listed hosts, and reports its totals once.
func TestMeteredNetworkCountsTrafficAndKeepsToItsHosts(t *testing.T) {
	const page = "hello from the internet"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, page)
	}))
	defer backend.Close()
	dialer := &routingDialer{routes: map[string]string{
		"93.184.216.34:80":  strings.TrimPrefix(backend.URL, "http://"),
		"93.184.216.34:443": echoServer(t),
	}}
	proxy := newProxy(fakeResolver{
		"registry.example":   {"93.184.216.34"},
		"a.mirror.example":   {"93.184.216.34"},
		"elsewhere.example":  {"93.184.216.34"},
		"registry.example.x": {"93.184.216.34"},
	}, dialer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	// The test's connections arrive on loopback, so loopback is the network.
	network := netip.MustParseAddr("127.0.0.1")
	if err := startMeter(t.Context(), server.URL, network, []string{"bad host"}); err == nil {
		t.Fatal("an invalid host list must be refused")
	}
	if err := startMeter(t.Context(), server.URL, network, []string{"registry.example", ".mirror.example"}); err != nil {
		t.Fatal(err)
	}

	status := func(target, body string) int {
		t.Helper()
		res, err := client.Post(target, "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	if got := status("http://registry.example/pkg", "12345"); got != http.StatusOK {
		t.Fatalf("a listed host: %d", got)
	}
	if got := status("http://a.mirror.example/pkg", ""); got != http.StatusOK {
		t.Fatalf("a listed domain's subdomain: %d", got)
	}
	for _, target := range []string{"http://elsewhere.example/", "http://registry.example.x/", "http://93.184.216.34/"} {
		if got := status(target, ""); got != http.StatusForbidden {
			t.Fatalf("%s is not listed and must be refused, got %d", target, got)
		}
	}

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "CONNECT registry.example:443 HTTP/1.1\r\nHost: registry.example:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	if res, err := http.ReadResponse(reader, nil); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("tunnel to a listed host: %v %v", res, err)
	}
	_, _ = io.WriteString(conn, "ping")
	echoed := make([]byte, 4)
	if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != "ping" {
		t.Fatalf("tunnel echo: %q %v", echoed, err)
	}
	_ = conn.Close()

	var usage Usage
	deadline := time.Now().Add(5 * time.Second)
	for {
		// The tunnel's counts land when its copies finish.
		if m := proxy.meters.get(network); m != nil && m.up.Load() >= 9 && m.down.Load() >= int64(2*len(page)+4) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if usage, err = endMeter(t.Context(), server.URL, network); err != nil {
		t.Fatal(err)
	}
	if !usage.Known || usage.Up < 9 || usage.Down < int64(2*len(page)+4) || usage.Denied != 3 ||
		strings.Join(usage.DeniedHosts, " ") != "elsewhere.example registry.example.x 93.184.216.34" {
		t.Fatalf("usage = %+v; want 5+4 bytes up, two pages and the echo down, 3 refused hosts", usage)
	}
	if again, err := endMeter(t.Context(), server.URL, network); err != nil || again.Known {
		t.Fatalf("an ended meter is forgotten: %+v %v", again, err)
	}
	if got := status("http://elsewhere.example/", ""); got != http.StatusOK {
		t.Fatalf("an unmetered network reaches any public host: %d", got)
	}
}

// Only a command inside the egress container (loopback) controls meters;
// a build or preview network never can.
func TestMeterControlIsLoopbackOnly(t *testing.T) {
	proxy := newProxy(fakeResolver{}, &routingDialer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		local, remote string
		want          int
	}{
		{"172.18.0.2:3128", "172.18.0.5:40000", http.StatusForbidden},
		{"127.0.0.1:3128", "172.18.0.5:40000", http.StatusForbidden},
		{"127.0.0.1:3128", "127.0.0.1:40000", http.StatusNoContent},
		{"[::ffff:127.0.0.1]:3128", "[::ffff:127.0.0.1]:40000", http.StatusNoContent},
		{"[::ffff:172.18.0.2]:3128", "[::ffff:127.0.0.1]:40000", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, meterPath+"?network=172.18.0.2", nil)
		local := net.TCPAddrFromAddrPort(netip.MustParseAddrPort(tc.local))
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("local %s remote %s: %d, want %d", tc.local, tc.remote, rec.Code, tc.want)
		}
	}
}

func TestStaleMetersAreForgotten(t *testing.T) {
	ms := newMeters()
	now := time.Now()
	ms.nowFunc = func() time.Time { return now }
	old, fresh := netip.MustParseAddr("172.18.0.2"), netip.MustParseAddr("172.19.0.2")
	ms.start(old, nil)
	now = now.Add(staleMeter + time.Minute)
	ms.start(fresh, nil)
	if ms.get(old) != nil || ms.get(fresh) == nil {
		t.Fatal("a registration no worker ended is forgotten after staleMeter")
	}
}
