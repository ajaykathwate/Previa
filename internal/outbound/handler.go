package outbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
	pages "github.com/ajaykathwatee/previa/web/pages/outbound"
	"github.com/go-chi/chi/v5"
)

// UserSession is the signed-in user surface the Outbound tab needs.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// Handler serves the owner-only Outbound tab of a repository. Its forms are
// CSRF-protected by the session middleware; removal is confirmed in a modal.
type Handler struct {
	service *Service
	users   UserSession
	logger  *slog.Logger
}

func NewHandler(service *Service, users UserSession, logger *slog.Logger) *Handler {
	return &Handler{service: service, users: users, logger: logger}
}

func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}/outbound", h.page)
		r.Post("/repositories/{repoID}/outbound", h.add)
		r.Post("/repositories/{repoID}/outbound/{id}/remove", h.remove)
	})
}

func outboundURL(repoID int64, query url.Values) string {
	u := fmt.Sprintf("/repositories/%d/outbound", repoID)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (h *Handler) identity(r *http.Request) (int64, int64, bool) {
	repoID, err := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
	user, _, ok := h.users.UserIDs(r.Context())
	return user, repoID, err == nil && repoID > 0 && ok
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.render(w, r, user, repoID, http.StatusOK, pages.Page{Notice: notice(r.URL.Query())})
}

// notice confirms the last change from its redirect query; only a valid
// destination is ever echoed.
func notice(query url.Values) string {
	if d, err := egress.ParseDestination(query.Get("approved")); err == nil {
		return "Approved " + d.String() + ". Previews started from now on can connect to it."
	}
	if d, err := egress.ParseDestination(query.Get("removed")); err == nil {
		return "Removed " + d.String() + ". Running previews lose it within a minute."
	}
	return ""
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	host, port := r.PostForm.Get("host"), r.PostForm.Get("port")
	d, err := h.service.Add(r.Context(), user, repoID, host, port)
	if err == nil {
		h.logger.InfoContext(r.Context(), "outbound destination approved", "repo_id", repoID, "port", d.Port)
		http.Redirect(w, r, outboundURL(repoID, url.Values{"approved": {d.String()}}), http.StatusSeeOther)
		return
	}
	view := pages.Page{Host: host, Port: port}
	status := http.StatusUnprocessableEntity
	var invalid *InvalidError
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.As(err, &invalid):
		if invalid.Field == "port" {
			view.PortError = invalid.Message
		} else {
			view.HostError = invalid.Message
		}
		view.Error = "The destination was not approved."
	case errors.Is(err, ErrDisabled):
		status, view.Error = http.StatusConflict, "Outbound connections are turned off on this server."
	case errors.Is(err, ErrAccessRemoved):
		status, view.Error = http.StatusConflict, "GitHub access to this repository was removed. You can remove destinations, but not add them."
	case errors.Is(err, ErrLimit):
		view.Error = fmt.Sprintf("A repository may approve at most %d destinations. Remove one first.", MaxDestinations)
	case errors.Is(err, ErrPortTaken):
		view.PortError = "Another destination already uses this port; remove it first."
		view.Error = "The destination was not approved."
	default:
		h.logger.ErrorContext(r.Context(), "approve outbound destination failed", "repo_id", repoID)
		status, view.Error = http.StatusServiceUnavailable, "The destination could not be approved. Please retry."
	}
	h.render(w, r, user, repoID, status, view)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !ok || err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	d, err := h.service.Remove(r.Context(), user, repoID, id)
	switch {
	case errors.Is(err, ErrUnavailable):
		// Gone already, or not this user's: nothing to remove.
		http.Redirect(w, r, outboundURL(repoID, nil), http.StatusSeeOther)
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "remove outbound destination failed", "repo_id", repoID)
		http.Error(w, "The destination could not be removed. Please retry.", http.StatusServiceUnavailable)
		return
	}
	h.logger.InfoContext(r.Context(), "outbound destination removed", "repo_id", repoID, "port", d.Port)
	http.Redirect(w, r, outboundURL(repoID, url.Values{"removed": {d.String()}}), http.StatusSeeOther)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, user, repoID int64, status int, view pages.Page) {
	repo, err := h.service.Repository(r.Context(), user, repoID)
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "load repository for outbound connections failed", "repo_id", repoID)
		http.Error(w, "Outbound connections are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
		return
	}
	list, err := h.service.List(r.Context(), user, repoID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list outbound destinations failed", "repo_id", repoID)
		http.Error(w, "Outbound connections are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
		return
	}
	view.RepoID, view.FullName, view.Available = repo.ID, repo.FullName, repo.Available
	view.Enabled, view.Ports, view.Limit = h.service.Enabled(), h.service.Ports(), MaxDestinations
	for _, d := range list {
		view.Destinations = append(view.Destinations, pages.Destination{ID: d.ID, Host: d.Host, Port: d.Port, CreatedAt: d.CreatedAt})
	}
	username, _ := h.users.Username(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.Index(username, h.users.CSRFToken(r.Context()), view).Render(r.Context(), w); err != nil {
		h.logger.ErrorContext(r.Context(), "render outbound page failed", "repo_id", repoID)
	}
}
