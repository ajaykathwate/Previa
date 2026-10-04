package builds

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicAddressExcludesEveryInternalRange(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "1.1.1.1": true, "2606:4700:4700::1111": true, "::ffff:1.1.1.1": true,
		"10.0.0.1": false, "172.16.5.4": false, "172.17.0.1": false, "192.168.65.254": false, // private and Docker
		"127.0.0.1": false, "::1": false, "0.0.0.0": false, "::": false,
		"169.254.169.254": false, "fe80::1": false, "fd00:ec2::254": false, // metadata and link-local
		"100.64.0.1": false, "192.0.0.1": false, "192.0.2.1": false, "198.51.100.1": false, "203.0.113.1": false,
		"198.18.0.1": false, "224.0.0.1": false, "255.255.255.255": false, "240.0.0.1": false,
		"::ffff:10.0.0.1": false, "::ffff:127.0.0.1": false, "64:ff9b::a00:1": false, "64:ff9b:1::1": false,
		"2002:a00:1::": false, "2001::1": false, "2001:db8::1": false, "fec0::1": false, "ff02::1": false, "100::1": false,
	} {
		if got := PublicAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddress(%s) = %v, want %v", addr, got, want)
		}
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	answers, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var addrs []netip.Addr
	for _, a := range answers {
		addrs = append(addrs, netip.MustParseAddr(a))
	}
	return addrs, nil
}

// routingDialer stands in for the internet: it sends vetted public addresses
// to local servers and records every address the proxy dials.
type routingDialer struct {
	mu     sync.Mutex
	routes map[string]string
	dialed []string
}

func (d *routingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	target, ok := d.routes[address]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unexpected dial to %s", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}

func TestEgressProxyReachesOnlyVettedPublicDestinations(t *testing.T) {
	var forwarded http.Header
	var forwardedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, forwardedHost = r.Header.Clone(), r.Host
		_, _ = io.WriteString(w, "hello from the internet")
	}))
	defer backend.Close()
	dialer := &routingDialer{routes: map[string]string{
		"93.184.216.34:80":  strings.TrimPrefix(backend.URL, "http://"),
		"93.184.216.34:443": echoServer(t),
	}}
	proxy := newEgressProxy(fakeResolver{
		"public.example":   {"93.184.216.34"},
		"internal.example": {"10.0.0.5"},
		"mixed.example":    {"93.184.216.34", "10.0.0.1"}, // rebinding-style mixed answer
		"metadata.example": {"169.254.169.254"},
		"loopback.example": {"::1"},
	}, dialer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://public.example/hello", nil)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Connection", "X-Private")
	req.Header.Set("X-Private", "hop-by-hop")
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "hello from the internet" || forwardedHost != "public.example" {
		t.Fatalf("allowed request: %d %q host %q", res.StatusCode, body, forwardedHost)
	}
	if forwarded.Get("Proxy-Authorization") != "" || forwarded.Get("X-Private") != "" {
		t.Fatalf("hop-by-hop headers forwarded: %v", forwarded)
	}

	for _, target := range []string{"http://internal.example/", "http://mixed.example/", "http://metadata.example/latest/meta-data/", "http://127.0.0.1/", "http://[::1]/", "http://loopback.example/", "http://public.example:8080/", "http://nowhere.example/"} {
		res, err := httpClient.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Previa build egress denied") {
			t.Fatalf("%s: %d %q", target, res.StatusCode, body)
		}
	}

	tunnel := func(target string) (int, net.Conn, *bufio.Reader) {
		t.Helper()
		conn, err := net.Dial("tcp", proxyURL.Host)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		reader := bufio.NewReader(conn)
		res, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, conn, reader
	}
	status, conn, reader := tunnel("public.example:443")
	if status != http.StatusOK {
		t.Fatalf("tunnel status %d", status)
	}
	_, _ = io.WriteString(conn, "ping")
	echoed := make([]byte, 4)
	if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != "ping" {
		t.Fatalf("tunnel echo %q %v", echoed, err)
	}
	_ = conn.Close()
	for _, target := range []string{"internal.example:443", "metadata.example:443", "[::1]:443", "10.1.2.3:443", "public.example:22", "mixed.example:443"} {
		status, conn, _ := tunnel(target)
		_ = conn.Close()
		if status != http.StatusForbidden {
			t.Fatalf("CONNECT %s: status %d", target, status)
		}
	}

	res, err = http.Get(server.URL + "/not-a-proxy-request")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct request: %d", res.StatusCode)
	}
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	for _, address := range dialer.dialed {
		if address != "93.184.216.34:80" && address != "93.184.216.34:443" {
			t.Fatalf("proxy dialed a non-vetted address: %s", address)
		}
	}
}
