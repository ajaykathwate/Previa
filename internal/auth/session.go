package auth

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
)

// NewSessionManager configures opaque server-side sessions using the supplied store.
func NewSessionManager(store scs.Store, secure bool, logger *slog.Logger) *scs.SessionManager {
	s := scs.New()
	s.Store = store
	s.HashTokenInStore = true
	s.Lifetime = 24 * time.Hour
	s.IdleTimeout = 30 * time.Minute
	s.Cookie.Name = "previa_session"
	if secure {
		// Browsers refuse a __Host- cookie from any other host, so a preview on a
		// sibling subdomain cannot plant a session for the dashboard.
		s.Cookie.Name = "__Host-previa_session"
	}
	s.Cookie.Path = "/"
	s.Cookie.HttpOnly = true
	s.Cookie.Secure = secure
	s.Cookie.SameSite = http.SameSiteLaxMode // The OAuth callback is a cross-site GET.
	s.Cookie.Persist = true
	s.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.ErrorContext(r.Context(), "session storage failed", "error", err)
		w.Header().Del("Location")
		http.Error(w, "Session unavailable. Please try again.", http.StatusServiceUnavailable)
	}
	return s
}
