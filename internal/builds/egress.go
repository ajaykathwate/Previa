package builds

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

// EgressPort is where the egress proxy listens inside the build networks.
const EgressPort = "3128"

const (
	egressDialTimeout   = 10 * time.Second
	egressIdleTimeout   = 2 * time.Minute
	egressMaxDuration   = time.Hour
	egressMaxConns      = 512
	egressHeaderTimeout = time.Minute
)

var (
	errDeniedPort    = errors.New("destination port is not allowed; builds may reach ports 80 and 443 only")
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

// PublicAddress reports whether a build may connect to a: a global unicast
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

// EgressProxy is the only route out of a build network: an HTTP forward proxy
// for CONNECT tunnels and plain http:// requests. It resolves names itself,
// refuses a destination if any of its addresses is not public, and connects only
// to the address it checked, so DNS rebinding cannot redirect a build to an
// internal service. It never sees or forwards Previa credentials.
type EgressProxy struct {
	resolver  resolver
	dialer    dialer
	logger    *slog.Logger
	transport *http.Transport
	slots     chan struct{}
}

// NewEgressProxy returns a proxy that uses the system resolver.
func NewEgressProxy(logger *slog.Logger) *EgressProxy {
	return newEgressProxy(net.DefaultResolver, &net.Dialer{Timeout: egressDialTimeout}, logger)
}

func newEgressProxy(r resolver, d dialer, logger *slog.Logger) *EgressProxy {
	p := &EgressProxy{resolver: r, dialer: d, logger: logger, slots: make(chan struct{}, egressMaxConns)}
	p.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			target, _ := ctx.Value(vettedTarget{}).(string)
			if target == "" {
				return nil, errBadTarget
			}
			return p.dialer.DialContext(ctx, network, target)
		},
		ResponseHeaderTimeout: egressHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          64,
		DisableCompression:    true,
	}
	return p
}

type vettedTarget struct{}

// ServeHTTP handles one proxy request.
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "egress proxy is busy", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "this proxy forwards only CONNECT tunnels and absolute http:// requests", http.StatusBadRequest)
		return
	}
	p.forward(w, r)
}

// vet resolves hostport and returns the single address to dial.
func (p *EgressProxy) vet(ctx context.Context, hostport, defaultPort string) (string, error) {
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
		lookupCtx, cancel := context.WithTimeout(ctx, egressDialTimeout)
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

func (p *EgressProxy) deny(w http.ResponseWriter, r *http.Request, host string, err error) {
	p.logger.WarnContext(r.Context(), "build egress denied", "host", host, "reason", err.Error())
	http.Error(w, "Previa build egress denied: "+err.Error(), http.StatusForbidden)
}

func (p *EgressProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	target, err := p.vet(r.Context(), r.Host, "443")
	if err != nil {
		p.deny(w, r, r.Host, err)
		return
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), egressDialTimeout)
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
	splice(client, buffered.Reader, upstream)
}

// splice copies both directions until either side ends, an idle timeout
// passes or the tunnel reaches its maximum duration.
func splice(client net.Conn, clientBuffer *bufio.Reader, upstream net.Conn) {
	end := time.Now().Add(egressMaxDuration)
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
		_, _ = io.Copy(upstream, &idleReader{conn: client, r: clientBuffer, end: end})
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, &idleReader{conn: upstream, r: upstream, end: end})
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
	deadline := time.Now().Add(egressIdleTimeout)
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

func (p *EgressProxy) forward(w http.ResponseWriter, r *http.Request) {
	target, err := p.vet(r.Context(), r.URL.Host, "80")
	if err != nil {
		p.deny(w, r, r.URL.Host, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), vettedTarget{}, target), egressMaxDuration)
	defer cancel()
	out := r.Clone(ctx)
	out.RequestURI = ""
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
	_, _ = io.Copy(w, res.Body)
}
