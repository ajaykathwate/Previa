package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Destination is one owner-approved outbound connection of a repository's
// previews: a DNS name and a port. Previews keep using the name, so TLS
// verification works end to end; the egress proxy resolves it itself.
type Destination struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (d Destination) String() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

// StateDir holds the egress container's runtime state on a tmpfs: one allow
// file per preview network and the forwarding counters.
const StateDir = "/run/previa-egress"

const (
	allowDirName  = "allow"
	statsFileName = "stats.json"
	portsFileName = "ports.json"
)

const (
	statsInterval = 10 * time.Second
	portsInterval = 2 * time.Second
)

// forwardLimits bound forwarded connections.
type forwardLimits struct {
	idle, maxDuration, recheck time.Duration
	maxBytes                   int64 // both directions together, per connection
	perNetwork, total          int   // concurrent connections
}

// defaultForwardLimits: a connection lives at most an hour, idles at most ten
// minutes and moves at most 1 GiB; a preview network holds at most 32 at once
// and the proxy 256. A withdrawn approval closes connections within 30 s.
var defaultForwardLimits = forwardLimits{
	idle: 10 * time.Minute, maxDuration: time.Hour, recheck: 30 * time.Second,
	maxBytes: 1 << 30, perNetwork: 32, total: 256,
}

// Forwarding outcomes: the counters' bounded keys.
const (
	OutcomeForwarded   = "forwarded"       // relayed to an approved destination
	OutcomeUnapproved  = "unapproved"      // no approval for this network and port
	OutcomeDenied      = "refused_address" // the name resolves to a non-public address
	OutcomeUnresolved  = "unresolvable"    // the name did not resolve
	OutcomeDialFailed  = "dial_failed"     // the destination did not accept
	OutcomeLimit       = "limit"           // too many connections at once
	OutcomeRevoked     = "revoked"         // closed because the approval was removed
	OutcomeByteLimited = "byte_limit"      // closed at the per-connection byte cap
)

// Outcomes lists every forwarding outcome.
var Outcomes = []string{OutcomeForwarded, OutcomeUnapproved, OutcomeDenied, OutcomeUnresolved, OutcomeDialFailed, OutcomeLimit, OutcomeRevoked, OutcomeByteLimited}

var hostName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ValidDestination reports whether d can be approved: a lowercase DNS name
// with at least two labels (never an IP address) and a port from 1 to 65535.
func ValidDestination(d Destination) bool {
	return len(d.Host) <= 253 && hostName.MatchString(d.Host) && d.Port >= 1 && d.Port <= 65535
}

// Stats are the forwarding counters since the egress container started.
type Stats struct {
	Connections map[string]int64 `json:"connections"`
	BytesUp     int64            `json:"bytes_up"`   // preview to destination
	BytesDown   int64            `json:"bytes_down"` // destination to preview
}

// Forwarder relays raw TCP from previews to the destinations their
// repository's owner approved. It listens on the operator's ports on every
// network the egress container joins. The network a connection arrived on,
// known from the egress container's own address on it, identifies the
// preview's repository: the worker writes one allow file per preview network,
// named by that address. Nothing the preview sends is trusted. Names are
// resolved here, every address must be public, and the vetted address is the
// one dialled, so DNS tricks cannot reach internal services.
type Forwarder struct {
	dir       string // allow files
	statsPath string
	portsPath string
	resolver  resolver
	dialer    dialer
	logger    *slog.Logger
	limits    forwardLimits

	mu     sync.Mutex
	active map[netip.Addr]int
	total  int

	outcomes  sync.Map // outcome -> *atomic.Int64
	bytesUp   atomic.Int64
	bytesDown atomic.Int64
}

// NewForwarder returns a forwarder that keeps its state (allow files and
// counters) under stateDir.
func NewForwarder(stateDir string, logger *slog.Logger) *Forwarder {
	return newForwarder(stateDir, net.DefaultResolver, &net.Dialer{Timeout: dialTimeout}, logger)
}

func newForwarder(stateDir string, r resolver, d dialer, logger *slog.Logger) *Forwarder {
	f := &Forwarder{dir: AllowDir(stateDir), statsPath: StatsPath(stateDir), portsPath: PortsPath(stateDir), resolver: r, dialer: d, logger: logger, limits: defaultForwardLimits, active: map[netip.Addr]int{}}
	for _, outcome := range Outcomes {
		f.outcomes.Store(outcome, new(atomic.Int64))
	}
	return f
}

// Serve forwards until ctx ends. It listens on the ports the worker last
// recorded in the state directory (WritePortsFile), opening and closing
// listeners as that list changes, and writes the counters periodically.
func (f *Forwarder) Serve(ctx context.Context) error {
	var wg sync.WaitGroup
	listeners := map[int]net.Listener{}
	closeAll := func() {
		for port, l := range listeners {
			_ = l.Close()
			delete(listeners, port)
		}
	}
	reconcile := func() {
		want, err := ReadPortsFile(f.portsPath)
		if err != nil {
			f.logger.Warn("read forwarding ports", "error", err)
			return
		}
		for port, l := range listeners {
			if !slices.Contains(want, port) {
				_ = l.Close()
				delete(listeners, port)
				f.logger.Info("stopped forwarding a port", "port", port)
			}
		}
		for _, port := range want {
			if listeners[port] != nil {
				continue
			}
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", ":"+strconv.Itoa(port))
			if err != nil {
				f.logger.Warn("listen for outbound connections", "port", port, "error", err)
				continue
			}
			listeners[port] = l
			wg.Go(func() { f.accept(ctx, l) })
			f.logger.Info("forwarding approved outbound connections", "port", port)
		}
	}
	reconcile()
	ticker := time.NewTicker(portsInterval)
	defer ticker.Stop()
	wg.Go(func() {
		ticker := time.NewTicker(statsInterval)
		defer ticker.Stop()
		for {
			f.writeStats()
			select {
			case <-ctx.Done():
				// The counters as they stand when it stops.
				defer f.writeStats()
				return
			case <-ticker.C:
			}
		}
	})
	for {
		select {
		case <-ctx.Done():
			closeAll()
			wg.Wait()
			return nil
		case <-ticker.C:
			reconcile()
		}
	}
}

func (f *Forwarder) accept(ctx context.Context, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go f.handle(ctx, conn)
	}
}

// handle relays one connection or refuses it.
func (f *Forwarder) handle(ctx context.Context, conn net.Conn) {
	local, ok := addrPort(conn.LocalAddr())
	if !ok {
		_ = conn.Close()
		return
	}
	network, port := local.Addr().Unmap(), int(local.Port())
	dest, ok := f.lookup(network, port)
	if !ok {
		f.refuse(conn, OutcomeUnapproved, "", port, network)
		return
	}
	if !f.acquire(network) {
		f.refuse(conn, OutcomeLimit, dest.Host, port, network)
		return
	}
	defer f.release(network)
	targets, outcome := f.vet(ctx, dest)
	if outcome != "" {
		f.refuse(conn, outcome, dest.Host, port, network)
		return
	}
	upstream, err := dialFirst(ctx, f.dialer, targets, addressTimeout)
	if err != nil {
		f.refuse(conn, OutcomeDialFailed, dest.Host, port, network)
		return
	}
	started := time.Now()
	outcome = f.relay(ctx, conn, upstream, network, dest)
	f.count(outcome)
	f.logger.Info("outbound connection", "outcome", outcome, "host", dest.Host, "port", port, "duration", time.Since(started).Round(time.Millisecond).String())
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	return tcp.AddrPort(), true
}

func (f *Forwarder) refuse(conn net.Conn, outcome, host string, port int, network netip.Addr) {
	_ = conn.Close()
	f.count(outcome)
	f.logger.Warn("outbound connection refused", "outcome", outcome, "host", host, "port", port, "network_address", network.String())
}

func (f *Forwarder) count(outcome string) {
	if counter, ok := f.outcomes.Load(outcome); ok {
		counter.(*atomic.Int64).Add(1)
	}
}

func (f *Forwarder) acquire(network netip.Addr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.total >= f.limits.total || f.active[network] >= f.limits.perNetwork {
		return false
	}
	f.total++
	f.active[network]++
	return true
}

func (f *Forwarder) release(network netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total--
	if f.active[network]--; f.active[network] <= 0 {
		delete(f.active, network)
	}
}

// lookup returns the destination approved for port on the network whose
// egress address is network.
func (f *Forwarder) lookup(network netip.Addr, port int) (Destination, bool) {
	destinations, err := ReadAllowFile(f.dir, network)
	if err != nil {
		return Destination{}, false
	}
	for _, d := range destinations {
		if d.Port == port {
			return d, true
		}
	}
	return Destination{}, false
}

// vet resolves the destination and returns its public addresses to dial, or
// the outcome that refuses it.
func (f *Forwarder) vet(ctx context.Context, d Destination) ([]string, string) {
	lookupCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	addrs, err := f.resolver.LookupNetIP(lookupCtx, "ip", d.Host)
	cancel()
	if err != nil || len(addrs) == 0 {
		return nil, OutcomeUnresolved
	}
	targets, err := vetted(addrs, strconv.Itoa(d.Port))
	if err != nil {
		return nil, OutcomeDenied
	}
	return targets, ""
}

// relay copies both directions until either side ends, an idle timeout or
// the maximum duration passes, the byte cap is reached, or the approval is
// withdrawn (checked every 30 seconds).
func (f *Forwarder) relay(ctx context.Context, client, upstream net.Conn, network netip.Addr, dest Destination) string {
	end := time.Now().Add(f.limits.maxDuration)
	var budget atomic.Int64
	budget.Store(f.limits.maxBytes)
	var once sync.Once
	outcome := OutcomeForwarded
	var outcomeMu sync.Mutex
	stop := func(why string) {
		once.Do(func() {
			outcomeMu.Lock()
			outcome = why
			outcomeMu.Unlock()
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	done := make(chan struct{}, 2)
	copyBounded := func(dst, src net.Conn, counter *atomic.Int64) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			deadline := time.Now().Add(f.limits.idle)
			if deadline.After(end) {
				deadline = end
			}
			_ = src.SetReadDeadline(deadline)
			n, err := src.Read(buf)
			if n > 0 {
				if budget.Add(-int64(n)) < 0 {
					stop(OutcomeByteLimited)
					return
				}
				counter.Add(int64(n))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					stop(OutcomeForwarded)
					return
				}
			}
			if err != nil {
				stop(OutcomeForwarded)
				return
			}
		}
	}
	go copyBounded(upstream, client, &f.bytesUp)
	go copyBounded(client, upstream, &f.bytesDown)
	ticker := time.NewTicker(f.limits.recheck)
	defer ticker.Stop()
	finished := 0
	for finished < 2 {
		select {
		case <-done:
			finished++
		case <-ctx.Done():
			stop(OutcomeForwarded)
		case <-ticker.C:
			if current, ok := f.lookup(network, dest.Port); !ok || current.Host != dest.Host {
				stop(OutcomeRevoked)
			}
		}
	}
	stop(OutcomeForwarded)
	outcomeMu.Lock()
	defer outcomeMu.Unlock()
	return outcome
}

// Snapshot returns the counters.
func (f *Forwarder) Snapshot() Stats {
	s := Stats{Connections: map[string]int64{}, BytesUp: f.bytesUp.Load(), BytesDown: f.bytesDown.Load()}
	for _, outcome := range Outcomes {
		if counter, ok := f.outcomes.Load(outcome); ok {
			s.Connections[outcome] = counter.(*atomic.Int64).Load()
		}
	}
	return s
}

func (f *Forwarder) writeStats() {
	data, err := json.Marshal(f.Snapshot())
	if err != nil {
		return
	}
	if err := writeAtomic(f.statsPath, data); err != nil {
		f.logger.Warn("write forwarding counters", "error", err)
	}
}

// AllowDir is the directory of allow files under a state directory.
func AllowDir(stateDir string) string { return filepath.Join(stateDir, allowDirName) }

// StatsPath is the counters file under a state directory.
func StatsPath(stateDir string) string { return filepath.Join(stateDir, statsFileName) }

// PortsPath is the forwarded ports file under a state directory.
func PortsPath(stateDir string) string { return filepath.Join(stateDir, portsFileName) }

// ReadPortsFile returns the ports to forward; none when there is no file.
func ReadPortsFile(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ports []int
	if err := json.Unmarshal(data, &ports); err != nil {
		return nil, fmt.Errorf("read forwarding ports: %w", err)
	}
	return ports, nil
}

// WritePortsFile records the ports to forward; none stops forwarding.
func WritePortsFile(path string, ports []int) error {
	for _, port := range ports {
		if port < 1 || port > 65535 || strconv.Itoa(port) == Port {
			return fmt.Errorf("port %d cannot be forwarded", port)
		}
	}
	data, err := json.Marshal(append([]int{}, ports...))
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func allowPath(dir string, network netip.Addr) string {
	return filepath.Join(dir, network.Unmap().String()+".json")
}

// ReadAllowFile returns the destinations approved for the network whose
// egress address is network; none when there is no file.
func ReadAllowFile(dir string, network netip.Addr) ([]Destination, error) {
	data, err := os.ReadFile(allowPath(dir, network))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var destinations []Destination
	if err := json.Unmarshal(data, &destinations); err != nil {
		return nil, fmt.Errorf("read allow file: %w", err)
	}
	return destinations, nil
}

// WriteAllowFile records the destinations approved for one preview network;
// none removes the file. Every destination is validated again, and each port
// may name one destination.
func WriteAllowFile(dir string, network netip.Addr, destinations []Destination) error {
	if !network.IsValid() || !network.Unmap().Is4() {
		return errors.New("the network address must be an IPv4 address")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create allow directory: %w", err)
	}
	if len(destinations) == 0 {
		if err := os.Remove(allowPath(dir, network)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove allow file: %w", err)
		}
		return nil
	}
	var ports []int
	for _, d := range destinations {
		if !ValidDestination(d) {
			return fmt.Errorf("destination %q is not a DNS name and port", d.String())
		}
		if slices.Contains(ports, d.Port) {
			return fmt.Errorf("port %d is approved twice", d.Port)
		}
		ports = append(ports, d.Port)
	}
	data, err := json.Marshal(destinations)
	if err != nil {
		return err
	}
	return writeAtomic(allowPath(dir, network), data)
}

// SyncAllowFiles makes the allow directory hold exactly desired: one file per
// preview network address that has approvals, and nothing else, so an
// approval never outlives the network it was written for.
func SyncAllowFiles(dir string, desired map[netip.Addr][]Destination) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create allow directory: %w", err)
	}
	keep := map[string]bool{}
	for network, destinations := range desired {
		if len(destinations) == 0 {
			continue
		}
		if err := WriteAllowFile(dir, network, destinations); err != nil {
			return err
		}
		keep[filepath.Base(allowPath(dir, network))] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read allow directory: %w", err)
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove stale allow file: %w", err)
			}
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ParseDestination reads host:port.
func ParseDestination(value string) (Destination, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return Destination{}, errors.New("use host:port")
	}
	n, err := strconv.Atoi(port)
	d := Destination{Host: strings.ToLower(host), Port: n}
	if err != nil || !ValidDestination(d) {
		return Destination{}, errors.New("use a DNS name and a port from 1 to 65535")
	}
	return d, nil
}

// ReadStats reads the counters file; zero counters when it does not exist yet.
func ReadStats(path string) (Stats, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Stats{Connections: map[string]int64{}}, nil
	}
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	if err := json.Unmarshal(data, &s); err != nil {
		return Stats{}, fmt.Errorf("read forwarding counters: %w", err)
	}
	if s.Connections == nil {
		s.Connections = map[string]int64{}
	}
	return s, nil
}
