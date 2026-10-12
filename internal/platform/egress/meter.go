package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Build networks are metered. The worker registers a build network's egress
// address when the build starts and reads the totals when it ends; the
// registration may also limit the build to a list of hosts (package
// registries only). Registrations live in the proxy's memory, so a restarted
// proxy forgets them: the worker sees that at the end (Usage.Known is false)
// and cannot rely on a limit having held.
const (
	meterPath = "/_previa/meter"
	// staleMeter forgets a registration a crashed worker never ended:
	// longer than any build may run.
	staleMeter = 26 * time.Hour
	// maxDeniedHosts bounds the refused hosts a meter remembers.
	maxDeniedHosts = 5
)

// Usage is a build network's traffic through the proxy since it registered:
// bytes sent (up) and received (down), and requests refused for a host
// outside its list.
type Usage struct {
	Up     int64 `json:"up"`
	Down   int64 `json:"down"`
	Denied int64 `json:"denied"`
	// DeniedHosts are the first few distinct hosts refused.
	DeniedHosts []string `json:"denied_hosts,omitempty"`
	// Known is false when the proxy had no registration for the network,
	// for example because it restarted during the build.
	Known bool `json:"known"`
}

var errNotAllowedHost = errors.New("destination is not one of the package registries builds may reach on this server")

// hostPattern is a lowercase DNS name, optionally with a leading dot that
// makes it match subdomains.
var hostPattern = regexp.MustCompile(`^\.?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidHost reports whether entry can be in a build's host list: a DNS name
// such as registry.npmjs.org, or .example.com for every subdomain.
func ValidHost(entry string) bool { return len(entry) <= 254 && hostPattern.MatchString(entry) }

// meter counts one build network's traffic.
type meter struct {
	up, down, denied atomic.Int64
	// hosts limits the destinations; empty allows any public host.
	hosts   []string
	started time.Time

	mu          sync.Mutex
	deniedHosts []string
}

// refuse counts a request for host that the list does not allow.
func (m *meter) refuse(host string) {
	m.denied.Add(1)
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deniedHosts) < maxDeniedHosts && !slices.Contains(m.deniedHosts, host) {
		m.deniedHosts = append(m.deniedHosts, host)
	}
}

// allows reports whether the network may reach host.
func (m *meter) allows(host string) bool {
	if m == nil || len(m.hosts) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	for _, entry := range m.hosts {
		if host == entry || (strings.HasPrefix(entry, ".") && strings.HasSuffix(host, entry)) {
			return true
		}
	}
	return false
}

// reader counts what passes through r as sent (up) or received; a nil
// meter counts nothing.
func (m *meter) reader(r io.Reader, up bool) io.Reader {
	if m == nil {
		return r
	}
	if up {
		return &countingReader{r: r, total: &m.up}
	}
	return &countingReader{r: r, total: &m.down}
}

type countingReader struct {
	r     io.Reader
	total *atomic.Int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.total.Add(int64(n))
	return n, err
}

// meters holds the registered build networks by the proxy's address on them.
type meters struct {
	mu      sync.Mutex
	byAddr  map[netip.Addr]*meter
	nowFunc func() time.Time
}

func newMeters() *meters {
	return &meters{byAddr: map[netip.Addr]*meter{}, nowFunc: time.Now}
}

func (ms *meters) get(network netip.Addr) *meter {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.byAddr[network]
}

// start registers network afresh, replacing an earlier registration of the
// same address, and forgets registrations no worker ended.
func (ms *meters) start(network netip.Addr, hosts []string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	now := ms.nowFunc()
	for addr, m := range ms.byAddr {
		if now.Sub(m.started) > staleMeter {
			delete(ms.byAddr, addr)
		}
	}
	ms.byAddr[network] = &meter{hosts: hosts, started: now}
}

// end returns network's totals and forgets it.
func (ms *meters) end(network netip.Addr) Usage {
	ms.mu.Lock()
	m, ok := ms.byAddr[network]
	delete(ms.byAddr, network)
	ms.mu.Unlock()
	if !ok {
		return Usage{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Usage{Up: m.up.Load(), Down: m.down.Load(), Denied: m.denied.Load(), DeniedHosts: slices.Clone(m.deniedHosts), Known: true}
}

// localNetwork is the proxy's own address on the network a request came in
// on, which identifies a build or preview network.
func localNetwork(r *http.Request) (netip.Addr, bool) {
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return netip.Addr{}, false
	}
	ap, ok := addrPort(a)
	// A dual-stack listener reports IPv4 connections as IPv4-mapped.
	return ap.Addr().Unmap(), ok
}

// control serves meter registrations to the worker. Only a command run
// inside the egress container reaches it (over loopback); build and preview
// networks never do.
func (p *Proxy) control(w http.ResponseWriter, r *http.Request) {
	local, okLocal := localNetwork(r)
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if !okLocal || err != nil || !local.IsLoopback() || !remote.Addr().Unmap().IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	network, err := netip.ParseAddr(r.URL.Query().Get("network"))
	if err != nil {
		http.Error(w, "network must be an IP address", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var hosts []string
		for entry := range strings.SplitSeq(r.URL.Query().Get("hosts"), ",") {
			if entry = strings.TrimSpace(entry); entry == "" {
				continue
			}
			if !ValidHost(entry) {
				http.Error(w, fmt.Sprintf("invalid host %q", entry), http.StatusBadRequest)
				return
			}
			hosts = append(hosts, entry)
		}
		p.meters.start(network.Unmap(), hosts)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.meters.end(network.Unmap()))
	default:
		http.Error(w, "use POST or DELETE", http.StatusMethodNotAllowed)
	}
}

// controlClient talks to the proxy over loopback, never through a proxy.
var controlClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}

// localProxy is the proxy as seen from a command inside its container.
const localProxy = "http://127.0.0.1:" + Port

func controlURL(proxy string, network netip.Addr, hosts []string) string {
	q := url.Values{"network": {network.String()}}
	if len(hosts) > 0 {
		q.Set("hosts", strings.Join(hosts, ","))
	}
	return proxy + meterPath + "?" + q.Encode()
}

// StartMeter asks the proxy in this container to meter network, limited to
// hosts when given.
func StartMeter(ctx context.Context, network netip.Addr, hosts []string) error {
	return startMeter(ctx, localProxy, network, hosts)
}

// EndMeter asks the proxy in this container for network's totals and ends
// its metering.
func EndMeter(ctx context.Context, network netip.Addr) (Usage, error) {
	return endMeter(ctx, localProxy, network)
}

func startMeter(ctx context.Context, proxy string, network netip.Addr, hosts []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL(proxy, network, hosts), nil)
	if err != nil {
		return err
	}
	res, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("start metering: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("start metering: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func endMeter(ctx context.Context, proxy string, network netip.Addr) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, controlURL(proxy, network, nil), nil)
	if err != nil {
		return Usage{}, err
	}
	res, err := controlClient.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("end metering: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("end metering: %s", res.Status)
	}
	var u Usage
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&u); err != nil {
		return Usage{}, fmt.Errorf("end metering: %w", err)
	}
	return u, nil
}
