package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Probe reports whether the service at target is ready: an HTTP status from
// 200 to 399 for path, or an accepted TCP connection when path is empty.
// target must be a literal IP address and port, so a probe never resolves a
// name, uses a proxy or follows a redirect: it can only reach the address the
// worker chose, never an arbitrary URL.
func Probe(ctx context.Context, target, path string, timeout time.Duration) error {
	addr, err := netip.ParseAddrPort(target)
	if err != nil || !addr.IsValid() || addr.Port() == 0 {
		return errors.New("probe target must be an IP address and port")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if path == "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr.String())
		if err != nil {
			return fmt.Errorf("nothing is accepting connections on port %d yet", addr.Port())
		}
		_ = conn.Close()
		return nil
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\ \t\r\n") {
		return errors.New("health check path must be an absolute path")
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+path, nil)
	if err != nil {
		return errors.New("health check path must be an absolute path")
	}
	req.Header.Set("User-Agent", "previa-readiness-probe")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no HTTP response on port %d yet", addr.Port())
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 399 {
		return fmt.Errorf("health check %s returned HTTP %d", path, res.StatusCode)
	}
	return nil
}
