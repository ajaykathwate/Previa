package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/web/components"

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
	return u.DisplayName(), ok
}

// GitHubUsername is the provider login for source-permission checks. Personal
// display names must never be sent to GitHub as account identities.
func (h *Handler) GitHubUsername(ctx context.Context) (string, bool) {
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
			if err == nil && u.SessionEpoch != h.sessions.GetInt64(r.Context(), "session_epoch") {
				err = pgx.ErrNoRows
			}
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// The account is gone: end the session and continue signed out.
				if err := h.sessions.Destroy(r.Context()); err != nil {
					h.fail(w, r, http.StatusServiceUnavailable, "Your session could not be cleared. Please try again.")
					return
				}
			case err != nil:
				h.logger.ErrorContext(r.Context(), "load session user", "error", err)
				h.fail(w, r, http.StatusServiceUnavailable, "Your account is unavailable. Please try again.")
				return
			default:
				token := h.sessions.Token(r.Context())
				r = r.WithContext(components.WithViewer(context.WithValue(r.Context(), userKey, u), components.Viewer{Name: u.DisplayName(), Login: u.Username, Avatar: u.AvatarSrc(), PublicID: u.PublicID}))
				r = r.WithContext(realtime.WithSessionCheck(r.Context(), func(ctx context.Context) error {
					if err := h.sessionExists(ctx, token); err != nil {
						return err
					}
					fresh, err := h.users.GetUser(ctx, id)
					if err != nil {
						return err
					}
					if fresh.SessionEpoch != u.SessionEpoch {
						return errors.New("session revoked")
					}
					return nil
				}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Long-lived responses must also stop on ordinary logout or idle expiry, even
// when the account-wide epoch has not changed. The token is captured once so a
// stream never races against mutable request session data.
func (h *Handler) sessionExists(ctx context.Context, token string) error {
	if h.sessions.HashTokenInStore {
		hash := sha256.Sum256([]byte(token))
		token = base64.RawURLEncoding.EncodeToString(hash[:])
	}
	var found bool
	var err error
	if store, ok := h.sessions.Store.(interface {
		FindCtx(context.Context, string) ([]byte, bool, error)
	}); ok {
		_, found, err = store.FindCtx(ctx, token)
	} else {
		_, found, err = h.sessions.Store.Find(token)
	}
	if err != nil {
		return fmt.Errorf("check live session: %w", err)
	}
	if !found {
		return errors.New("session ended or expired")
	}
	return nil
}

// RequireUser redirects anonymous requests to the login flow.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := CurrentUser(r.Context()); !ok {
			login := loginDestination(r)
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", login)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			publicid.Redirect(w, r, login, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loginDestination preserves safe navigation, never a mutation. HTMX's
// current document is preferable to returning to a fragment endpoint.
func loginDestination(r *http.Request) string {
	const login = "/auth/github/login"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return login
	}
	next := r.URL.RequestURI()
	if r.Header.Get("HX-Request") == "true" {
		current, err := url.Parse(r.Header.Get("HX-Current-URL"))
		if err == nil && current.Host == r.Host && (current.Scheme == "http" || current.Scheme == "https") && LocalPath(current.RequestURI()) {
			next = current.RequestURI()
		}
	}
	if !LocalPath(next) {
		return login
	}
	return login + "?" + url.Values{"next": {next}}.Encode()
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
				if err := parseCSRFForm(r); err != nil {
					h.fail(w, r, http.StatusBadRequest, "The form could not be read.")
					return
				}
				if r.MultipartForm != nil {
					defer func() {
						if err := r.MultipartForm.RemoveAll(); err != nil {
							h.logger.WarnContext(r.Context(), "remove multipart form files", "error", err)
						}
					}()
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

func parseCSRFForm(r *http.Request) error {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err == nil && kind == "multipart/form-data" {
		return r.ParseMultipartForm(1 << 20)
	}
	return r.ParseForm()
}

// UserIDs supplies scalar identity values to feature-owned authorization boundaries.
func (h *Handler) UserIDs(ctx context.Context) (int64, int64, bool) {
	u, ok := CurrentUser(ctx)
	return u.ID, u.GitHubID, ok
}

// SelectedWorkspaceID remembers navigation in this session. Resource access is
// always checked independently by the workspace feature.
func (h *Handler) SelectedWorkspaceID(ctx context.Context) int64 {
	return h.sessions.GetInt64(ctx, "active_workspace")
}
func (h *Handler) RememberWorkspace(ctx context.Context, id int64) {
	if id <= 0 {
		h.sessions.Remove(ctx, "active_workspace")
		return
	}
	h.sessions.Put(ctx, "active_workspace", id)
}
