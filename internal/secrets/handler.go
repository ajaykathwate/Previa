package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	pages "github.com/ajaykathwatee/previa/web/pages/secrets"
	"github.com/go-chi/chi/v5"
)

// UserSession is the signed-in user surface the secrets pages need.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// Handler serves the owner-only Secrets tab of a repository.
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
		r.Get("/repositories/{repoID}/secrets", h.page)
		r.Post("/repositories/{repoID}/secrets", h.put)
		r.Post("/repositories/{repoID}/secrets/delete", h.delete)
	})
}

func secretsURL(repoID int64, query url.Values) string {
	u := fmt.Sprintf("/repositories/%d/secrets", repoID)
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
	view := pages.Page{}
	query := r.URL.Query()
	if name := query.Get("saved"); namePattern.MatchString(name) {
		view.Notice = "Saved " + name + ". Its value is encrypted and will not be shown again."
	}
	if name := query.Get("deleted"); namePattern.MatchString(name) {
		view.Notice = "Deleted " + name + "."
	}
	h.render(w, r, user, repoID, http.StatusOK, view, query.Get("replace"))
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	// Browsers submit textarea line breaks as CRLF; store \n line breaks.
	value := []byte(strings.ReplaceAll(r.PostForm.Get("value"), "\r\n", "\n"))
	created, err := h.service.Put(r.Context(), user, repoID, name, value)
	clear(value)
	if err == nil {
		h.logger.InfoContext(r.Context(), "repository secret saved", "repo_id", repoID, "name", name, "created", created)
		http.Redirect(w, r, secretsURL(repoID, url.Values{"saved": {name}}), http.StatusSeeOther)
		return
	}
	view := pages.Page{FormName: name}
	status := http.StatusUnprocessableEntity
	var invalid *InvalidError
	switch {
	case errors.As(err, &invalid):
		view.ErrorField, view.Error = invalid.Field, invalid.Message
		if invalid.Field == "name" && !namePattern.MatchString(name) {
			view.FormName = ""
		}
	case errors.Is(err, ErrLimit):
		view.Error = fmt.Sprintf("This repository already has %d secrets. Delete one before adding another.", MaxSecretsPerRepository)
	case errors.Is(err, ErrNotConfigured):
		status, view.Error = http.StatusServiceUnavailable, "Secrets are not configured on this server."
	case errors.Is(err, ErrUnavailable):
		status, view.Error = http.StatusConflict, "GitHub access to this repository was removed. You can delete secrets, but not add or replace them."
	default:
		h.logger.ErrorContext(r.Context(), "save repository secret failed", "repo_id", repoID)
		status, view.Error = http.StatusServiceUnavailable, "The secret could not be saved. Please retry."
	}
	h.render(w, r, user, repoID, status, view, "")
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	name := r.PostForm.Get("name")
	deleted, err := h.service.Delete(r.Context(), user, repoID, name)
	var invalid *InvalidError
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.As(err, &invalid):
		http.Redirect(w, r, secretsURL(repoID, nil), http.StatusSeeOther)
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "delete repository secret failed", "repo_id", repoID)
		http.Error(w, "The secret could not be deleted. Please retry.", http.StatusServiceUnavailable)
		return
	}
	query := url.Values{}
	if deleted {
		h.logger.InfoContext(r.Context(), "repository secret deleted", "repo_id", repoID, "name", name)
		query.Set("deleted", name)
	}
	http.Redirect(w, r, secretsURL(repoID, query), http.StatusSeeOther)
}

// render loads the owned repository and its secret names. It never receives or
// renders a value.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, user, repoID int64, status int, view pages.Page, replace string) {
	repo, err := h.service.Repository(r.Context(), user, repoID)
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "load repository secrets failed", "repo_id", repoID)
		http.Error(w, "Secrets are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
		return
	}
	list, err := h.service.List(r.Context(), user, repoID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list repository secrets failed", "repo_id", repoID)
		http.Error(w, "Secrets are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
		return
	}
	view.RepoID, view.FullName, view.Available, view.Configured = repo.ID, repo.FullName, repo.Available, h.service.Configured()
	view.Limit = MaxSecretsPerRepository
	for _, s := range list {
		view.Secrets = append(view.Secrets, pages.Secret{Name: s.Name, UpdatedAt: s.UpdatedAt})
		if s.Name == replace {
			view.FormName, view.Replacing = s.Name, true
		}
	}
	username, _ := h.users.Username(r.Context())
	templ.Handler(pages.Index(username, h.users.CSRFToken(r.Context()), view), templ.WithStatus(status)).ServeHTTP(w, r)
}
