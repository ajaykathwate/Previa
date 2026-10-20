package auth

import (
	"context"
	"crypto/sha256"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	authpages "github.com/ajaykathwatee/previa/web/pages/auth"
	"github.com/alexedwards/scs/v2"
)

type userRepository interface {
	UpsertUser(context.Context, Identity) (User, error)
	GetUser(context.Context, int64) (User, error)
}

// Handler owns the human login/session flows, independently of GitHub App access.
type Handler struct {
	sessions *scs.SessionManager
	users    userRepository
	audience AvatarAudience
	activity ActivityFeed
	provider identityProvider
	google   googleProvider
	logger   *slog.Logger
	stateKey []byte
	origin   string
	secure   bool
}

func NewHandler(cfg config.ServerConfig, sessions *scs.SessionManager, users userRepository, provider identityProvider, logger *slog.Logger) *Handler {
	key := sha256.Sum256([]byte("previa/oauth-state/" + cfg.SessionSecret))
	return &Handler{sessions: sessions, users: users, provider: provider, logger: logger,
		stateKey: key[:], origin: strings.TrimSuffix(cfg.AppBaseURL, "/"), secure: strings.HasPrefix(strings.ToLower(cfg.AppBaseURL), "https://")}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	h.beginOAuth(w, r, "github", 0)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if !h.consumeState(w, r, "github") {
		h.fail(w, r, http.StatusBadRequest, "Sign-in could not be verified. Please start again.")
		return
	}
	if r.URL.Query().Get("error") != "" {
		h.fail(w, r, http.StatusBadRequest, "GitHub sign-in was declined or interrupted. You can try again.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.fail(w, r, http.StatusBadRequest, "GitHub did not return a sign-in code. Please start again.")
		return
	}
	identity, err := h.provider.Authenticate(r.Context(), code)
	if err != nil {
		// Provider errors may contain response bodies; do not log OAuth secrets.
		h.logger.WarnContext(r.Context(), "GitHub identity authentication failed")
		h.fail(w, r, http.StatusBadGateway, "GitHub sign-in is unavailable. Please try again.")
		return
	}
	h.saveOAuthIdentity(w, r, "github", identity)
}

// LocalPath reports whether next is a path on this server to return to
// after signing in: never another host, so sign-in is no open redirect.
func LocalPath(next string) bool {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") || len(next) > 2048 {
		return false
	}
	for _, ch := range next {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	u, err := url.ParseRequestURI(next)
	return err == nil && u.Host == "" && u.Scheme == ""
}

// logoutConfirmation preserves old links without rendering a separate screen.
func (h *Handler) logoutConfirmation(w http.ResponseWriter, r *http.Request) {
	publicid.Redirect(w, r, "/dashboard?confirm=logout", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.sessions.Destroy(r.Context()); err != nil {
		h.logger.ErrorContext(r.Context(), "destroy session", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Sign-out failed. Please try again.")
		return
	}
	http.SetCookie(w, h.stateCookie("", -1))
	publicid.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	if r.Header.Get("HX-Request") == "true" {
		// An in-page request shows the refusal's sentence next to its control;
		// an error page would only be replaced by a generic message.
		http.Error(w, message, status)
		return
	}
	templ.Handler(authpages.Error(message), templ.WithStatus(status)).ServeHTTP(w, r)
}
