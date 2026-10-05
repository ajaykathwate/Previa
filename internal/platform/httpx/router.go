package httpx

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter constructs a router with request IDs, logging, metrics, panic
// recovery and security headers, without host or body restrictions.
func NewRouter(logger *slog.Logger) *chi.Mux {
	return New(logger, Options{})
}

// New constructs the public router: request IDs, the client address (from
// trusted proxies only), correlated request logs and metrics, panic
// recovery, security headers, and, when configured, host validation and a
// body cap.
func New(logger *slog.Logger, o Options) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(clientAddress(o.TrustedProxies))
	r.Use(requestLogging(logger))
	r.Use(panicRecovery(logger))
	r.Use(securityHeaders(o.Secure))
	if o.AppHost != "" {
		r.Use(hostCheck(o.AppHost, o.TrustedProxies, o.HostExempt))
	}
	if o.MaxBodyBytes > 0 {
		r.Use(limitBody(o.MaxBodyBytes, o.BodyExempt))
	}
	return r
}

// requestID returns the ID installed by chi's middleware.
func requestID(r *http.Request) string {
	return middleware.GetReqID(r.Context())
}
