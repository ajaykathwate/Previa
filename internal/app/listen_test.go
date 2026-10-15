package app

import (
	"net"
	"testing"
)

func TestHTTPListenerHonorsIPv4Wildcard(t *testing.T) {
	listener, err := listenHTTP("0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	if addr.IP.To4() == nil {
		t.Fatalf("explicit IPv4 wildcard became an IPv6 listener: %s", addr)
	}
}

func TestHTTPListenerPreservesIPv6(t *testing.T) {
	listener, err := listenHTTP("[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	if addr.IP.To4() != nil || !addr.IP.IsLoopback() {
		t.Fatalf("IPv6 loopback binding changed: %s", addr)
	}
}
