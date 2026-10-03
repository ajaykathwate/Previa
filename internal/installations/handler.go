package installations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	authpages "github.com/ajaykathwatee/previa/web/pages/auth"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
)

type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// The PR feature supplies the selected panel; installations owns the directory.
type repositoryWorkspace interface {
	RepositoryPanel(context.Context, int64, int64, url.Values, string, string) (templ.Component, error)
}

var ErrWorkspaceInput = errors.New("invalid repository workspace filters")

type Handler struct {
	service   *Service
	users     UserSession
	sessions  *scs.SessionManager
	slug      string
	logger    *slog.Logger
	workspace repositoryWorkspace
}

func (h *Handler) WithWorkspace(workspace repositoryWorkspace) *Handler {
	h.workspace = workspace
	return h
}

func NewHandler(service *Service, users UserSession, sessions *scs.SessionManager, slug string, logger *slog.Logger) *Handler {
	return &Handler{service: service, users: users, sessions: sessions, slug: slug, logger: logger}
}
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.With(requireUser).Get("/repositories", h.repositories)
	if h.slug == "" {
		return
	}
	r.With(requireUser).Post("/installations/start", h.start)
	r.With(requireUser).Get("/installations/callback", h.setup)
	r.With(requireUser).Get("/installations/authorize/callback", h.authorize)
}
func newState() string {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("generate installation state")
	}
	return hex.EncodeToString(bytes[:])
}
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.users.UserIDs(r.Context())
	if !ok {
		h.fail(w, r, http.StatusUnauthorized, "Sign in before connecting a repository.")
		return
	}
	state := newState()
	h.sessions.Put(r.Context(), "installation_state", state)
	h.sessions.Put(r.Context(), "installation_user", userID)
	h.sessions.Put(r.Context(), "installation_expires", time.Now().Add(10*time.Minute).Unix())
	h.sessions.Remove(r.Context(), "pending_installation")
	h.sessions.Remove(r.Context(), "installation_authorize_state")
	http.Redirect(w, r, "https://github.com/apps/"+h.slug+"/installations/new?state="+state, http.StatusSeeOther)
}
func (h *Handler) validSession(r *http.Request, stateKey string) bool {
	userID, _, ok := h.users.UserIDs(r.Context())
	expected := h.sessions.PopString(r.Context(), stateKey)
	provided := r.URL.Query().Get("state")
	return ok && expected != "" && provided != "" && hmac.Equal([]byte(expected), []byte(provided)) &&
		h.sessions.GetInt64(r.Context(), "installation_user") == userID && time.Now().Unix() < h.sessions.GetInt64(r.Context(), "installation_expires")
}
func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	if !h.validSession(r, "installation_state") {
		h.fail(w, r, http.StatusBadRequest, "Installation could not be verified. Start again from your dashboard.")
		return
	}
	if r.URL.Query().Get("setup_action") == "request" {
		h.fail(w, r, http.StatusAccepted, "An organization owner must approve your installation request. Once approved, restart the connection from your dashboard.")
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		h.fail(w, r, http.StatusBadRequest, "GitHub did not return a valid installation. Start again.")
		return
	}
	h.sessions.Put(r.Context(), "pending_installation", id)
	state := newState()
	h.sessions.Put(r.Context(), "installation_authorize_state", state)
	http.Redirect(w, r, h.service.authorization.AuthCodeURL(state), http.StatusSeeOther)
}
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	if !h.validSession(r, "installation_authorize_state") {
		h.fail(w, r, http.StatusBadRequest, "Installation authorization expired or could not be verified. Start again.")
		return
	}
	id := h.sessions.GetInt64(r.Context(), "pending_installation")
	h.sessions.Remove(r.Context(), "pending_installation")
	h.sessions.Remove(r.Context(), "installation_user")
	h.sessions.Remove(r.Context(), "installation_expires")
	code := r.URL.Query().Get("code")
	if id <= 0 || strings.TrimSpace(code) == "" || r.URL.Query().Get("error") != "" {
		h.fail(w, r, http.StatusBadRequest, "GitHub App authorization was declined or interrupted. You can try again.")
		return
	}
	userID, githubID, _ := h.users.UserIDs(r.Context())
	if err := h.service.Link(r.Context(), code, userID, githubID, id); err != nil {
		// OAuth/provider errors can contain sensitive response bodies. Never log them.
		h.logger.WarnContext(r.Context(), "installation linking failed", "installation_id", id)
		status := http.StatusBadGateway
		if errors.Is(err, ErrForbidden) {
			status = http.StatusForbidden
		}
		if errors.Is(err, ErrSuperseded) {
			status = http.StatusConflict
		}
		h.fail(w, r, status, "This installation could not be linked. Check that the same GitHub account has access, then start again. An installation already linked to another user cannot be reassigned.")
		return
	}
	h.logger.InfoContext(r.Context(), "installation linked; authenticated repository verification completed", "installation_id", id)
	h.sessions.Put(r.Context(), "installation_notice", "linked")
	http.Redirect(w, r, "/repositories", http.StatusSeeOther)
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	templ.Handler(authpages.Error(message), templ.WithStatus(status)).ServeHTTP(w, r)
}
