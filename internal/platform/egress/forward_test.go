package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

var loopback = netip.MustParseAddr("127.0.0.1")

// forwardFixture accepts connections on a loopback port and hands each to the
// forwarder, as its listeners do; the allow file for 127.0.0.1 plays the
// preview network's approvals.
type forwardFixture struct {
	t        *testing.T
	f        *Forwarder
	port     int
	dialer   *routingDialer
	stateDir string
}

// newForwardFixture starts a forwarder. configure adjusts its limits before it
// accepts anything: its connections read them, so they cannot change later.
func newForwardFixture(t *testing.T, resolver resolver, configure ...func(*forwardLimits)) *forwardFixture {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	x := &forwardFixture{t: t, dialer: &routingDialer{routes: map[string]string{}}, stateDir: t.TempDir()}
	x.port = listener.Addr().(*net.TCPAddr).Port
	x.f = newForwarder(x.stateDir, resolver, x.dialer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, c := range configure {
		c(&x.f.limits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go x.f.accept(ctx, listener)
	return x
}

func (x *forwardFixture) allow(destinations ...Destination) {
	x.t.Helper()
	if err := WriteAllowFile(AllowDir(x.stateDir), loopback, destinations); err != nil {
		x.t.Fatal(err)
	}
}

// exchange connects as a preview would and returns what came back for msg,
// or the error that ended the connection.
func (x *forwardFixture) exchange(msg string) (string, error) {
	conn, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(x.port))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, msg); err != nil {
		return "", err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (x *forwardFixture) count(outcome string) int64 {
	return x.f.Snapshot().Connections[outcome]
}

func (x *forwardFixture) waitCount(outcome string, want int64) {
	x.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for x.count(outcome) < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := x.count(outcome); got != want {
		x.t.Fatalf("%s: %d, want %d (%+v)", outcome, got, want, x.f.Snapshot())
	}
}

// waitIdle waits until no connection holds a slot. A connection's outcome is
// counted just before its slot is released, so a count alone does not mean
// the slot is free again.
func (x *forwardFixture) waitIdle() {
	x.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		x.f.mu.Lock()
		held := x.f.total
		x.f.mu.Unlock()
		if held == 0 {
			return
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("%d connections still hold a slot", held)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwarderRelaysOnlyApprovedPublicDestinations(t *testing.T) {
	x := newForwardFixture(t, fakeResolver{
		"db.example.com":       {"93.184.216.34"},
		"internal.example.com": {"10.0.0.5"},
		"mixed.example.com":    {"93.184.216.34", "10.0.0.1"},
		"metadata.example.com": {"169.254.169.254"},
	})
	target := "93.184.216.34:" + strconv.Itoa(x.port)
	x.dialer.routes[target] = echoServer(t)

	// Nothing approved: refused before any lookup or dial.
	if _, err := x.exchange("ping"); err == nil {
		t.Fatal("an unapproved connection was relayed")
	}
	x.waitCount(OutcomeUnapproved, 1)

	// Approved: relayed to the vetted address, bytes counted both ways.
	x.allow(Destination{Host: "db.example.com", Port: x.port})
	if got, err := x.exchange("hello"); err != nil || got != "hello" {
		t.Fatalf("approved destination: %q %v", got, err)
	}
	x.waitCount(OutcomeForwarded, 1)
	if s := x.f.Snapshot(); s.BytesUp != 5 || s.BytesDown != 5 {
		t.Fatalf("bytes %+v", s)
	}

	// Names that resolve to any internal address are refused and never dialled.
	for i, host := range []string{"internal.example.com", "mixed.example.com", "metadata.example.com"} {
		x.allow(Destination{Host: host, Port: x.port})
		if _, err := x.exchange("hello"); err == nil {
			t.Fatalf("%s was relayed", host)
		}
		x.waitCount(OutcomeDenied, int64(i+1))
	}
	// A name that does not resolve.
	x.allow(Destination{Host: "missing.example.com", Port: x.port})
	if _, err := x.exchange("hello"); err == nil {
		t.Fatal("an unresolvable name was relayed")
	}
	x.waitCount(OutcomeUnresolved, 1)
	x.dialer.mu.Lock()
	defer x.dialer.mu.Unlock()
	for _, dialed := range x.dialer.dialed {
		if dialed != target {
			t.Fatalf("dialed %s", dialed)
		}
	}
}

// rebindingResolver answers with a public address first and an internal one
// afterwards, as a DNS rebinding attack would.
type rebindingResolver struct {
	mu    sync.Mutex
	calls int
}

func (r *rebindingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 1 {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func TestForwarderDialsTheAddressItVetted(t *testing.T) {
	resolver := &rebindingResolver{}
	x := newForwardFixture(t, resolver)
	target := "93.184.216.34:" + strconv.Itoa(x.port)
	x.dialer.routes[target] = echoServer(t)
	x.allow(Destination{Host: "rebind.example.com", Port: x.port})
	if got, err := x.exchange("first"); err != nil || got != "first" {
		t.Fatalf("first connection: %q %v", got, err)
	}
	// The second lookup answers loopback: refused, never dialled.
	if _, err := x.exchange("second"); err == nil {
		t.Fatal("a rebound name was relayed")
	}
	x.waitCount(OutcomeDenied, 1)
	if resolver.calls != 2 || len(x.dialer.dialed) != 1 || x.dialer.dialed[0] != target {
		t.Fatalf("lookups %d, dials %v", resolver.calls, x.dialer.dialed)
	}
}

func TestForwarderBoundsConnections(t *testing.T) {
	x := newForwardFixture(t, fakeResolver{"db.example.com": {"93.184.216.34"}}, func(l *forwardLimits) {
		l.perNetwork = 1
		l.recheck = 50 * time.Millisecond
		l.maxBytes = 8 // the held connection's two bytes stay below it
	})
	x.dialer.routes["93.184.216.34:"+strconv.Itoa(x.port)] = echoServer(t)
	x.allow(Destination{Host: "db.example.com", Port: x.port})

	held, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(x.port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, err := io.WriteString(held, "x"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(held, buf); err != nil {
		t.Fatal(err)
	}
	// A second connection on the same network exceeds its limit.
	if _, err := x.exchange("y"); err == nil {
		t.Fatal("the per-network limit was not enforced")
	}
	x.waitCount(OutcomeLimit, 1)

	// Withdrawing the approval closes the open connection.
	x.allow()
	_ = held.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := held.Read(buf); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection outlived its approval: %v", err)
	}
	x.waitCount(OutcomeRevoked, 1)
	x.waitIdle() // the per-network limit is one: the next connection needs this slot

	// A connection is cut at its byte cap.
	x.allow(Destination{Host: "db.example.com", Port: x.port})
	if _, err := x.exchange("more than eight bytes"); err == nil {
		t.Fatal("the byte cap was not enforced")
	}
	x.waitCount(OutcomeByteLimited, 1)
}

func freePort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	return probe.Addr().(*net.TCPAddr).Port
}

// dialWithin connects to a loopback port, retrying until it listens.
func dialWithin(port int, wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestForwarderServesTheRecordedPortsAndWritesCounters(t *testing.T) {
	first, second := freePort(t), freePort(t)
	state := t.TempDir()
	if err := WritePortsFile(PortsPath(state), []int{first}); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(state, fakeResolver{}, &routingDialer{routes: map[string]string{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.Serve(ctx) }()
	conn, err := dialWithin(first, 5*time.Second)
	if err != nil {
		t.Fatal("the forwarder did not listen on its port")
	}
	_ = conn.Close()
	// A new list opens the new port and closes the old one.
	if err := WritePortsFile(PortsPath(state), []int{second}); err != nil {
		t.Fatal(err)
	}
	if conn, err := dialWithin(second, 5*time.Second); err != nil {
		t.Fatal("the forwarder did not open the newly recorded port")
	} else {
		_ = conn.Close()
	}
	closed := false
	for deadline := time.Now().Add(5 * time.Second); !closed && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		conn, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(first))
		if err != nil {
			closed = true
		} else {
			_ = conn.Close()
		}
	}
	if !closed {
		t.Fatal("the forwarder kept listening on a port no longer recorded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.Snapshot().Connections[OutcomeUnapproved] == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	stats, err := ReadStats(StatsPath(state))
	if err != nil || stats.Connections[OutcomeUnapproved] < 1 || len(stats.Connections) != len(Outcomes) {
		t.Fatalf("counters %+v %v", stats, err)
	}
}

func TestSyncAllowFilesKeepsExactlyTheLiveNetworks(t *testing.T) {
	dir := t.TempDir()
	stale, live := netip.MustParseAddr("172.30.0.2"), netip.MustParseAddr("172.31.0.2")
	if err := WriteAllowFile(dir, stale, []Destination{{Host: "old.example.com", Port: 5432}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.tmp-leftover", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncAllowFiles(dir, map[netip.Addr][]Destination{live: {{Host: "db.example.com", Port: 5432}}, netip.MustParseAddr("172.29.0.2"): nil}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadAllowFile(dir, stale); got != nil {
		t.Fatal("a stale network kept its approvals")
	}
	if got, _ := ReadAllowFile(dir, live); len(got) != 1 || got[0].Host != "db.example.com" {
		t.Fatalf("live approvals %v", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("allow directory holds %d entries", len(entries))
	}
}

func TestPortsFile(t *testing.T) {
	path := t.TempDir() + "/ports.json"
	if ports, err := ReadPortsFile(path); err != nil || ports != nil {
		t.Fatalf("no file: %v %v", ports, err)
	}
	if err := WritePortsFile(path, []int{5432, 6379}); err != nil {
		t.Fatal(err)
	}
	if ports, err := ReadPortsFile(path); err != nil || len(ports) != 2 {
		t.Fatalf("read back %v %v", ports, err)
	}
	for _, bad := range [][]int{{0}, {70000}, {3128}} {
		if err := WritePortsFile(path, bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestAllowFiles(t *testing.T) {
	dir := t.TempDir()
	network := netip.MustParseAddr("172.21.0.2")
	if got, err := ReadAllowFile(dir, network); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	approved := []Destination{{Host: "db.example.com", Port: 5432}, {Host: "cache.example.com", Port: 6380}}
	if err := WriteAllowFile(dir, network, approved); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadAllowFile(dir, network); err != nil || len(got) != 2 || got[1] != approved[1] {
		t.Fatalf("read back %v %v", got, err)
	}
	if err := WriteAllowFile(dir, network, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadAllowFile(dir, network); got != nil {
		t.Fatal("withdrawing kept the approvals")
	}
	for _, bad := range [][]Destination{
		{{Host: "10.0.0.1", Port: 5432}},                                           // an address, not a name
		{{Host: "localhost", Port: 5432}},                                          // one label
		{{Host: "db.example.com", Port: 0}},                                        // no port
		{{Host: "DB.example.com", Port: 1}},                                        // not normalized
		{{Host: "a.example.com", Port: 5432}, {Host: "b.example.com", Port: 5432}}, // one port twice
	} {
		if err := WriteAllowFile(dir, network, bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := WriteAllowFile(dir, netip.MustParseAddr("fd00::1"), approved); err == nil {
		t.Error("accepted an IPv6 network address")
	}
}

func TestParseDestination(t *testing.T) {
	if d, err := ParseDestination(" DB.Example.com:5432 "); err != nil || d != (Destination{Host: "db.example.com", Port: 5432}) {
		t.Fatalf("%v %v", d, err)
	}
	for _, bad := range []string{"db.example.com", "db.example.com:0", "db.example.com:http", "127.0.0.1:5432", "[::1]:5432", "-bad.example.com:1", "db.example.com:65536"} {
		if _, err := ParseDestination(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
