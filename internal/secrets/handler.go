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
	view.Notice = notice(query)
	if name := query.Get("replace"); namePattern.MatchString(name) {
		view.Replacing = name
		view.Rows = []pages.Row{{Name: name}}
	}
	h.render(w, r, user, repoID, http.StatusOK, view)
}

// notice confirms the last save or delete from its redirect query.
func notice(query url.Values) string {
	count, _ := strconv.Atoi(query.Get("saved_count"))
	created, _ := strconv.Atoi(query.Get("created"))
	deleted, _ := strconv.Atoi(query.Get("deleted_count"))
	switch {
	case namePattern.MatchString(query.Get("saved")):
		return "Saved " + query.Get("saved") + ". Its value is encrypted and will not be shown again."
	case count > 1 && created >= 0 && created <= count:
		return fmt.Sprintf("Saved %d secrets (%d new, %d replaced). Their values are encrypted and will not be shown again.", count, created, count-created)
	case namePattern.MatchString(query.Get("deleted")):
		return "Deleted " + query.Get("deleted") + "."
	case deleted > 1:
		return fmt.Sprintf("Deleted %d secrets.", deleted)
	}
	return ""
}

// put saves every filled editor line and every NAME=value of pasted .env
// text in one transaction. A rejected save shows the names again with the
// reason, never the values.
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
	names, values := r.PostForm["name"], r.PostForm["value"]
	view := pages.Page{}
	var entries []Entry
	var rowOf []int // the editor line of each entry, or -1 for pasted text
	for i, raw := range names {
		name := strings.TrimSpace(raw)
		value := ""
		if i < len(values) {
			// Browsers submit textarea line breaks as CRLF; store \n line breaks.
			value = strings.ReplaceAll(values[i], "\r\n", "\n")
		}
		if name == "" && value == "" {
			continue // an unused line
		}
		row := pages.Row{Name: name}
		switch {
		case name == "":
			row.Error = "Enter a name for this value."
		case value == "":
			row.Error = "Enter a value for " + name + "."
		default:
			entries = append(entries, Entry{Name: name, Value: []byte(value)})
			rowOf = append(rowOf, len(view.Rows))
		}
		view.Rows = append(view.Rows, row)
	}
	pasted, err := ParseDotenv(r.PostForm.Get("dotenv"))
	if invalid := (*InvalidError)(nil); errors.As(err, &invalid) {
		view.DotenvError = invalid.Message
	}
	for _, e := range pasted {
		entries = append(entries, e)
		rowOf = append(rowOf, -1)
	}
	defer func() {
		for _, e := range entries {
			clear(e.Value)
		}
	}()
	for _, row := range view.Rows {
		if row.Error != "" {
			view.Error = "Some lines need a name or a value. Nothing was saved; enter the values again."
		}
	}
	if view.DotenvError != "" && view.Error == "" {
		view.Error = "The pasted text could not be read. Nothing was saved; paste it again after fixing it."
	}
	if view.Error != "" {
		h.render(w, r, user, repoID, http.StatusUnprocessableEntity, view)
		return
	}
	created, err := h.service.PutMany(r.Context(), user, repoID, entries)
	if err == nil {
		h.logger.InfoContext(r.Context(), "repository secrets saved", "repo_id", repoID, "count", len(entries), "created", created)
		query := url.Values{}
		if len(entries) == 1 {
			query.Set("saved", entries[0].Name)
		} else {
			query.Set("saved_count", strconv.Itoa(len(entries)))
			query.Set("created", strconv.Itoa(created))
		}
		http.Redirect(w, r, secretsURL(repoID, query), http.StatusSeeOther)
		return
	}
	status := http.StatusUnprocessableEntity
	var invalid *InvalidError
	switch {
	case errors.As(err, &invalid):
		view.Error = invalid.Message + " Nothing was saved; enter the values again."
		if invalid.Entry > 0 {
			if line := rowOf[invalid.Entry-1]; line >= 0 {
				view.Rows[line].Error = invalid.Message
			} else {
				view.DotenvError = "In the pasted text: " + invalid.Message
			}
		}
	case errors.Is(err, ErrLimit):
		view.Error = fmt.Sprintf("A repository holds at most %d secrets. Delete some, or save fewer new names. Nothing was saved.", MaxSecretsPerRepository)
	case errors.Is(err, ErrNotConfigured):
		status, view.Error = http.StatusServiceUnavailable, "Secrets are not configured on this server."
	case errors.Is(err, ErrUnavailable):
		status, view.Error = http.StatusConflict, "GitHub access to this repository was removed. You can delete secrets, but not add or replace them."
	default:
		h.logger.ErrorContext(r.Context(), "save repository secrets failed", "repo_id", repoID)
		status, view.Error = http.StatusServiceUnavailable, "The secrets could not be saved. Please retry."
	}
	h.render(w, r, user, repoID, status, view)
}

// delete removes one secret (a row's Delete) or every selected one.
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
	names := r.PostForm["name"]
	deleted, err := h.service.DeleteMany(r.Context(), user, repoID, names)
	var invalid *InvalidError
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.As(err, &invalid):
		http.Redirect(w, r, secretsURL(repoID, nil), http.StatusSeeOther)
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "delete repository secrets failed", "repo_id", repoID)
		http.Error(w, "The secrets could not be deleted. Please retry.", http.StatusServiceUnavailable)
		return
	}
	query := url.Values{}
	if deleted > 0 {
		h.logger.InfoContext(r.Context(), "repository secrets deleted", "repo_id", repoID, "count", deleted)
		if len(names) == 1 {
			query.Set("deleted", names[0])
		} else {
			query.Set("deleted_count", strconv.Itoa(deleted))
		}
	}
	http.Redirect(w, r, secretsURL(repoID, query), http.StatusSeeOther)
}

// render loads the owned repository and its secret names. It never receives or
// renders a value.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, user, repoID int64, status int, view pages.Page) {
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
	}
	if view.Replacing != "" && !view.Exists(view.Replacing) {
		view.Replacing, view.Rows = "", nil
	}
	username, _ := h.users.Username(r.Context())
	templ.Handler(pages.Index(username, h.users.CSRFToken(r.Context()), view), templ.WithStatus(status)).ServeHTTP(w, r)
}
