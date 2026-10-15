package egress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Port is where the egress proxy listens on build and preview networks.
const Port = "3128"

const (
	dialTimeout   = 10 * time.Second
	idleTimeout   = 2 * time.Minute
	maxDuration   = time.Hour
	maxConns      = 512
	headerTimeout = time.Minute
)

var (
	errDeniedPort    = errors.New("destination port is not allowed; only ports 80 and 443 are reachable")
	errDeniedAddress = errors.New("destination resolves to a private, loopback, link-local or reserved address")
	errUnresolvable  = errors.New("destination could not be resolved")
	errBadTarget     = errors.New("destination host is invalid")
)

// nonPublic lists special-purpose ranges that Go's netip predicates do not
// cover: shared address space, benchmarking, documentation, IETF and
// translation prefixes whose embedded IPv4 address could be internal.
var nonPublic = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
		"2002::/16", "3fff::/20", "5f00::/16",
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	return prefixes
}()

var globalUnicastV6 = netip.MustParsePrefix("2000::/3")

// PublicAddress reports whether a build or preview may connect to a: a global unicast
// address outside private, loopback, link-local, shared, documentation,
// translation and other special-purpose ranges. Cloud metadata endpoints and
// every container network fall outside it.
func PublicAddress(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	if a.Is6() && !globalUnicastV6.Contains(a) {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Proxy is the only route out of build and preview networks: an HTTP forward
// proxy for CONNECT tunnels and plain http:// requests. It resolves names
// itself, refuses a destination if any of its addresses is not public, and
// connects only to the address it checked, so DNS rebinding cannot redirect a
// build or preview to an internal service. It never sees Previa credentials.
type Proxy struct {
	resolver  resolver
	dialer    dialer
	logger    *slog.Logger
	transport *http.Transport
	slots     chan struct{}
	meters    *meters
}

// NewProxy returns a proxy that uses the system resolver.
func NewProxy(logger *slog.Logger) *Proxy {
	return newProxy(net.DefaultResolver, &net.Dialer{Timeout: dialTimeout}, logger)
}

func newProxy(r resolver, d dialer, logger *slog.Logger) *Proxy {
	p := &Proxy{resolver: r, dialer: d, logger: logger, slots: make(chan struct{}, maxConns), meters: newMeters()}
	p.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			target, _ := ctx.Value(vettedTarget{}).(string)
			if target == "" {
				return nil, errBadTarget
			}
			return p.dialer.DialContext(ctx, network, target)
		},
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          64,
		DisableCompression:    true,
	}
	return p
}

type vettedTarget struct{}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect && r.URL.Host == "" && r.URL.Path == meterPath {
		p.control(w, r)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "egress proxy is busy", http.StatusServiceUnavailable)
		return
	}
	var m *meter
	if network, ok := localNetwork(r); ok {
		m = p.meters.get(network)
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, m)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "this proxy forwards only CONNECT tunnels and absolute http:// requests", http.StatusBadRequest)
		return
	}
	p.forward(w, r, m)
}

// hostOf is the host part of hostport.
func hostOf(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

// vet resolves hostport and returns the single address to dial.
func (p *Proxy) vet(ctx context.Context, hostport, defaultPort string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort
	}
	if port != "80" && port != "443" {
		return "", errDeniedPort
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if host == "" || len(host) > 253 || strings.ContainsAny(host, "/\\@ ") {
			return "", errBadTarget
		}
		lookupCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		addrs, err = p.resolver.LookupNetIP(lookupCtx, "ip", host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return "", errUnresolvable
		}
	}
	for _, a := range addrs {
		if !PublicAddress(a) {
			return "", errDeniedAddress
		}
	}
	return net.JoinHostPort(addrs[0].Unmap().String(), port), nil
}

func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host string, err error) {
	p.logger.WarnContext(r.Context(), "egress denied", "host", host, "reason", err.Error())
	http.Error(w, "Previa egress denied: "+err.Error(), http.StatusForbidden)
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, m *meter) {
	if !m.allows(hostOf(r.Host)) {
		m.refuse(hostOf(r.Host))
		p.deny(w, r, r.Host, errNotAllowedHost)
		return
	}
	target, err := p.vet(r.Context(), r.Host, "443")
	if err != nil {
		p.deny(w, r, r.Host, err)
		return
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.dialer.DialContext(dialCtx, "tcp", target)
	cancel()
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "tunnels are not supported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	splice(client, buffered.Reader, upstream, m)
}

// splice copies both directions until either side ends, an idle timeout
// passes or the tunnel reaches its maximum duration, counting the bytes for
// a metered network.
func splice(client net.Conn, clientBuffer *bufio.Reader, upstream net.Conn, m *meter) {
	end := time.Now().Add(maxDuration)
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	defer closeBoth()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, m.reader(&idleReader{conn: client, r: clientBuffer, end: end}, true))
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, m.reader(&idleReader{conn: upstream, r: upstream, end: end}, false))
		done <- struct{}{}
	}()
	<-done
	closeBoth()
	<-done
}

// idleReader extends the read deadline before each read, up to end.
type idleReader struct {
	conn net.Conn
	r    io.Reader
	end  time.Time
}

func (i *idleReader) Read(b []byte) (int, error) {
	deadline := time.Now().Add(idleTimeout)
	if deadline.After(i.end) {
		deadline = i.end
	}
	if err := i.conn.SetReadDeadline(deadline); err != nil {
		return 0, err
	}
	return i.r.Read(b)
}

// hopHeaders apply to one connection and are never forwarded.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func stripHopHeaders(h http.Header) {
	for _, field := range h.Values("Connection") {
		for _, name := range strings.Split(field, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, m *meter) {
	if !m.allows(r.URL.Hostname()) {
		m.refuse(r.URL.Hostname())
		p.deny(w, r, r.URL.Host, errNotAllowedHost)
		return
	}
	target, err := p.vet(r.Context(), r.URL.Host, "80")
	if err != nil {
		p.deny(w, r, r.URL.Host, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), vettedTarget{}, target), maxDuration)
	defer cancel()
	out := r.Clone(ctx)
	out.RequestURI = ""
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = io.NopCloser(m.reader(out.Body, true))
	}
	stripHopHeaders(out.Header)
	res, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = res.Body.Close() }()
	stripHopHeaders(res.Header)
	for name, values := range res.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, m.reader(res.Body, false))
}
