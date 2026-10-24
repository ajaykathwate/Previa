package egress

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
	"slices"
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
	proxy := newProxy(fakeResolver{
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
		if res.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Previa egress denied") {
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

// A name often has several addresses and one may not answer (one of GitHub's
// release download addresses did): the proxy moves on to the next vetted one.
func TestEgressProxyFallsBackToTheNextVettedAddress(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served by the second address")
	}))
	defer backend.Close()
	// 185.199.109.133 has no route, as on the network that met it.
	dialer := &routingDialer{routes: map[string]string{
		"185.199.108.133:80":  strings.TrimPrefix(backend.URL, "http://"),
		"185.199.108.133:443": echoServer(t),
	}}
	proxy := newProxy(fakeResolver{
		"downloads.example": {"2606:50c0:8000::154", "185.199.109.133", "185.199.108.133"},
	}, dialer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}

	res, err := client.Get("http://downloads.example/asset")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "served by the second address" {
		t.Fatalf("http through a dead first address: %d %q", res.StatusCode, body)
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT downloads.example:443 HTTP/1.1\r\nHost: downloads.example:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if !strings.Contains(status, "200") {
		t.Fatalf("tunnel through a dead first address: %q", status)
	}
	if blank, _ := reader.ReadString('\n'); blank != "\r\n" {
		t.Fatalf("tunnel response headers: %q", blank)
	}
	_, _ = io.WriteString(conn, "ping")
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("tunnel echo %q %v", echo, err)
	}

	// IPv4 first, the dead address before the live one, IPv6 never needed;
	// nothing outside the vetted answer was dialled.
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	want := []string{"185.199.109.133:80", "185.199.108.133:80", "185.199.109.133:443", "185.199.108.133:443"}
	if !slices.Equal(dialer.dialed, want) {
		t.Fatalf("dialled %v, want %v", dialer.dialed, want)
	}
}

// hangingDialer never answers some addresses, like a host with no route.
type hangingDialer struct {
	mu     sync.Mutex
	hang   map[string]bool
	dialed []string
}

func (d *hangingDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	hang := d.hang[address]
	d.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func TestDialFirstGivesEachAddressItsOwnTimeLimit(t *testing.T) {
	d := &hangingDialer{hang: map[string]bool{"203.0.114.1:443": true, "203.0.114.2:443": true}}
	started := time.Now()
	conn, err := dialFirst(t.Context(), d, []string{"203.0.114.1:443", "203.0.114.2:443", "203.0.114.3:443"}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("two hanging addresses took %s", elapsed)
	}
	if !slices.Equal(d.dialed, []string{"203.0.114.1:443", "203.0.114.2:443", "203.0.114.3:443"}) {
		t.Fatalf("dialled %v", d.dialed)
	}
	all := &hangingDialer{hang: map[string]bool{"203.0.114.1:443": true}}
	if _, err := dialFirst(t.Context(), all, []string{"203.0.114.1:443"}, 50*time.Millisecond); err == nil {
		t.Fatal("an address that never answers must fail")
	}
}

func TestVettedOrdersIPv4FirstAndRefusesAnyInternalAddress(t *testing.T) {
	addrs := []netip.Addr{netip.MustParseAddr("2606:4700::1"), netip.MustParseAddr("104.16.0.34"), netip.MustParseAddr("::ffff:104.16.1.34")}
	targets, err := vetted(addrs, "443")
	if err != nil || !slices.Equal(targets, []string{"104.16.0.34:443", "104.16.1.34:443", "[2606:4700::1]:443"}) {
		t.Fatalf("targets %v, %v", targets, err)
	}
	if _, err := vetted(append(addrs, netip.MustParseAddr("10.0.0.1")), "443"); err != errDeniedAddress {
		t.Fatalf("a private address among public ones: %v", err)
	}
}
