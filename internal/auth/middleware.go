package auth

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type contextKey uint8

const (
	userKey contextKey = iota
	csrfKey
)

// CurrentUser returns the authenticated identity, if one was loaded for the request.
func CurrentUser(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey).(User)
	return u, ok
}

// Username exposes the session identity to dashboard's small consumer interface.
func (h *Handler) Username(ctx context.Context) (string, bool) {
	u, ok := CurrentUser(ctx)
	return u.Username, ok
}

// CSRFToken exposes the token through dashboard's consumer-defined interface.
func (h *Handler) CSRFToken(ctx context.Context) string { return CSRFToken(ctx) }

// CSRFToken returns the session-bound token for forms and HTMX request headers.
func CSRFToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfKey).(string)
	return token
}

// LoadUser loads the session user into the request context.
func (h *Handler) LoadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if id := h.sessions.GetInt64(r.Context(), "user_id"); id != 0 {
			u, err := h.users.GetUser(r.Context(), id)
			if errors.Is(err, pgx.ErrNoRows) {
				if err := h.sessions.Destroy(r.Context()); err != nil {
					h.fail(w, r, http.StatusServiceUnavailable, "Your session could not be cleared. Please try again.")
					return
				}
			} else if err != nil {
				h.logger.ErrorContext(r.Context(), "load session user", "error", err)
				h.fail(w, r, http.StatusServiceUnavailable, "Your account is unavailable. Please try again.")
				return
			} else {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser redirects anonymous requests to the login flow.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := CurrentUser(r.Context()); !ok {
			http.Redirect(w, r, "/auth/github/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CSRF protects unsafe methods using a random token stored in the server session.
// Forms use csrf_token; HTMX can send the same value as X-CSRF-Token.
func (h *Handler) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := h.sessions.GetString(r.Context(), "csrf_token")
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if token == "" {
				token = randomToken()
				h.sessions.Put(r.Context(), "csrf_token", token)
			}
		default:
			if origin := r.Header.Get("Origin"); origin != "" && origin != h.origin {
				h.fail(w, r, http.StatusForbidden, "This action came from another site. Please try again here.")
				return
			}
			provided := r.Header.Get("X-CSRF-Token")
			if provided == "" {
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
				if err := r.ParseForm(); err != nil {
					h.fail(w, r, http.StatusBadRequest, "The form could not be read.")
					return
				}
				provided = r.PostForm.Get("csrf_token")
			}
			if token == "" || provided == "" || !hmac.Equal([]byte(token), []byte(provided)) {
				h.fail(w, r, http.StatusForbidden, "This action could not be verified. Refresh the page and try again.")
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), csrfKey, token))
		next.ServeHTTP(w, r)
	})
}

// UserIDs supplies scalar identity values to feature-owned authorization boundaries.
func (h *Handler) UserIDs(ctx context.Context) (int64, int64, bool) {
	u, ok := CurrentUser(ctx)
	return u.ID, u.GitHubID, ok
}
