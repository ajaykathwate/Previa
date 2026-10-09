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
	form := readSecretsForm(r.PostForm)
	defer form.wipe()
	if form.view.Error != "" {
		h.render(w, r, user, repoID, http.StatusUnprocessableEntity, form.view)
		return
	}
	created, err := h.service.PutMany(r.Context(), user, repoID, form.entries)
	if err != nil {
		status, unexpected := form.explainSaveError(err)
		if unexpected {
			h.logger.ErrorContext(r.Context(), "save repository secrets failed", "repo_id", repoID)
		}
		h.render(w, r, user, repoID, status, form.view)
		return
	}
	h.logger.InfoContext(r.Context(), "repository secrets saved", "repo_id", repoID, "count", len(form.entries), "created", created)
	query := url.Values{}
	if len(form.entries) == 1 {
		query.Set("saved", form.entries[0].Name)
	} else {
		query.Set("saved_count", strconv.Itoa(len(form.entries)))
		query.Set("created", strconv.Itoa(created))
	}
	http.Redirect(w, r, secretsURL(repoID, query), http.StatusSeeOther)
}

// secretsForm is a submitted secrets form: the page to show again (names and
// reasons, never values) and the entries to save.
type secretsForm struct {
	view    pages.Page
	entries []Entry
	rowOf   []int // the editor line of each entry, or -1 for pasted text
}

// readSecretsForm reads every filled editor line and every NAME=value of the
// pasted .env text. A line missing its name or value, or unreadable pasted
// text, sets the page's error: nothing is saved then.
func readSecretsForm(form url.Values) *secretsForm {
	f := &secretsForm{}
	names, values := form["name"], form["value"]
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
			f.entries = append(f.entries, Entry{Name: name, Value: []byte(value)})
			f.rowOf = append(f.rowOf, len(f.view.Rows))
		}
		if row.Error != "" {
			f.view.Error = "Some lines need a name or a value. Nothing was saved; enter the values again."
		}
		f.view.Rows = append(f.view.Rows, row)
	}
	pasted, err := ParseDotenv(form.Get("dotenv"))
	if invalid := (*InvalidError)(nil); errors.As(err, &invalid) {
		f.view.DotenvError = invalid.Message
		if f.view.Error == "" {
			f.view.Error = "The pasted text could not be read. Nothing was saved; paste it again after fixing it."
		}
	}
	for _, e := range pasted {
		f.entries = append(f.entries, e)
		f.rowOf = append(f.rowOf, -1)
	}
	return f
}

// wipe clears the submitted values from memory.
func (f *secretsForm) wipe() {
	for _, e := range f.entries {
		clear(e.Value)
	}
}

// explainSaveError puts why saving failed on the page, at the offending line
// when it is one, and returns the response status and whether the failure
// was unexpected (and so worth logging).
func (f *secretsForm) explainSaveError(err error) (status int, unexpected bool) {
	var invalid *InvalidError
	switch {
	case errors.As(err, &invalid):
		f.view.Error = invalid.Message + " Nothing was saved; enter the values again."
		if invalid.Entry > 0 {
			if line := f.rowOf[invalid.Entry-1]; line >= 0 {
				f.view.Rows[line].Error = invalid.Message
			} else {
				f.view.DotenvError = "In the pasted text: " + invalid.Message
			}
		}
		return http.StatusUnprocessableEntity, false
	case errors.Is(err, ErrLimit):
		f.view.Error = fmt.Sprintf("A repository holds at most %d secrets. Delete some, or save fewer new names. Nothing was saved.", MaxSecretsPerRepository)
		return http.StatusUnprocessableEntity, false
	case errors.Is(err, ErrNotConfigured):
		f.view.Error = "Secrets are not configured on this server."
		return http.StatusServiceUnavailable, false
	case errors.Is(err, ErrUnavailable):
		f.view.Error = "GitHub access to this repository was removed. You can delete secrets, but not add or replace them."
		return http.StatusConflict, false
	default:
		f.view.Error = "The secrets could not be saved. Please retry."
		return http.StatusServiceUnavailable, true
	}
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
