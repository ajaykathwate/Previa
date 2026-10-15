package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"golang.org/x/time/rate"
)

// Options configure the public router's ingress controls.
type Options struct {
	// AppHost is the dashboard's host[:port] from APP_BASE_URL. When set,
	// requests naming any other host (or, through a trusted proxy, any other
	// forwarded host) are refused with 421, except paths in HostExempt.
	AppHost    string
	HostExempt []string
	// Secure adds HSTS; set when APP_BASE_URL is HTTPS.
	Secure bool
	// TrustedProxies may set X-Forwarded-For/-Host; nobody else's are read.
	TrustedProxies []netip.Prefix
	// MaxBodyBytes caps request bodies (0: no cap), except BodyExempt paths
	// that enforce their own limit.
	MaxBodyBytes int64
	BodyExempt   []string
}

type clientIPKey struct{}

// ClientIP is the request's client address: the peer, or the nearest
// untrusted address a trusted proxy forwarded.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func trusted(addr netip.Addr, proxies []netip.Prefix) bool {
	for _, p := range proxies {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

func peer(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		addr, err := netip.ParseAddr(r.RemoteAddr)
		return addr, err == nil
	}
	return ap.Addr(), true
}

// fromTrustedProxy reports whether the peer may set forwarded headers.
func fromTrustedProxy(r *http.Request, proxies []netip.Prefix) bool {
	addr, ok := peer(r)
	return ok && trusted(addr, proxies)
}

// clientAddress resolves the client IP: X-Forwarded-For is read right to
// left only while each hop is a trusted proxy.
func clientAddress(proxies []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr, ok := peer(r)
			client := r.RemoteAddr
			if ok {
				client = addr.Unmap().String()
				if trusted(addr, proxies) {
					hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
					for i := len(hops) - 1; i >= 0; i-- {
						hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
						if err != nil {
							break
						}
						client = hop.Unmap().String()
						if !trusted(hop, proxies) {
							break
						}
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, client)))
		})
	}
}

func exempt(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// hostCheck refuses requests for another host. The server never builds
// URLs from the request, so this stops misrouted traffic and host-header
// games rather than fixing them up.
func hostCheck(appHost string, proxies []netip.Prefix, skip []string) func(http.Handler) http.Handler {
	appHost = strings.ToLower(appHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt(r.URL.Path, skip) {
				next.ServeHTTP(w, r)
				return
			}
			if !sameHost(r.Host, appHost) {
				metrics.IngressRejected("wrong_host")
				http.Error(w, "This server does not serve that host.", http.StatusMisdirectedRequest)
				return
			}
			if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" && fromTrustedProxy(r, proxies) && !sameHost(forwarded, appHost) {
				metrics.IngressRejected("forwarded_host")
				http.Error(w, "This server does not serve that host.", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sameHost compares host[:port] values, treating a missing default port
// as equal to the explicit one.
func sameHost(got, want string) bool {
	got = strings.ToLower(strings.TrimSpace(got))
	if got == want {
		return true
	}
	strip := func(h string) string {
		for _, p := range []string{":443", ":80"} {
			h = strings.TrimSuffix(h, p)
		}
		return h
	}
	return strip(got) == strip(want)
}

type nonceKey struct{}

// Nonce is the request's Content-Security-Policy nonce.
func Nonce(ctx context.Context) string {
	n, _ := ctx.Value(nonceKey{}).(string)
	return n
}

// securityHeaders sets a strict policy on every response: scripts and
// styles only from this origin or with the request's nonce, images from this
// origin plus GitHub's avatar host (members' profile pictures), no framing, no
// plugins, forms only to this origin and GitHub (installation and sign-in
// redirects), and HSTS when the dashboard is HTTPS.
func securityHeaders(secure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := make([]byte, 18)
			_, _ = rand.Read(raw)
			nonce := base64.RawStdEncoding.EncodeToString(raw)
			h := w.Header()
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-"+nonce+"' 'inline-speculation-rules'; "+
				"style-src 'self' 'nonce-"+nonce+"'; img-src 'self' data: "+GitHubAvatarOrigin+"; font-src 'self'; connect-src 'self'; "+
				"object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self' https://github.com")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
			if secure {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			ctx := context.WithValue(templ.WithNonce(r.Context(), nonce), nonceKey{}, nonce)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// limitBody caps request bodies. Handlers see a read error past the cap.
func limitBody(n int64, skip []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n && !exempt(r.URL.Path, skip) {
				metrics.IngressRejected("body_too_large")
				http.Error(w, "Request body is too large.", http.StatusRequestEntityTooLarge)
				return
			}
			if r.Body != nil && !exempt(r.URL.Path, skip) {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit allows each client IP `burst` requests at once and one more
// every `every` on the given paths; others pass untouched. At most maxKeys
// clients are tracked; idle ones are forgotten.
func RateLimit(paths []string, every time.Duration, burst int) func(http.Handler) http.Handler {
	l := &ipLimiter{every: rate.Every(every), burst: burst, max: 10000, idle: 10 * time.Minute, clients: map[string]*ipEntry{}, now: time.Now}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt(r.URL.Path, paths) && !l.allow(ClientIP(r)) {
				metrics.IngressRejected("rate_limited")
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(every/time.Second))))
				http.Error(w, "Too many requests. Try again shortly.", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type ipEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

type ipLimiter struct {
	mu      sync.Mutex
	every   rate.Limit
	burst   int
	max     int
	idle    time.Duration
	clients map[string]*ipEntry
	now     func() time.Time
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.clients[ip]
	if !ok {
		if len(l.clients) >= l.max {
			for k, v := range l.clients {
				if now.Sub(v.seen) > l.idle {
					delete(l.clients, k)
				}
			}
			if len(l.clients) >= l.max {
				// Under a flood of distinct addresses, start over rather than
				// grow without bound.
				clear(l.clients)
			}
		}
		e = &ipEntry{limiter: rate.NewLimiter(l.every, l.burst)}
		l.clients[ip] = e
	}
	e.seen = now
	return e.limiter.AllowN(now, 1)
}

// LimitConcurrency lets at most n requests run the handler at once; the
// rest get 503 with Retry-After and record nothing, so a sender's retry or
// redelivery is processed normally later.
func LimitConcurrency(n int, next http.Handler) http.Handler {
	slots := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			metrics.IngressRejected("overloaded")
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Busy. Retry shortly.", http.StatusServiceUnavailable)
		}
	})
}
