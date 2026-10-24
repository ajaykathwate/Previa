package secrets

import (
	"context"
	"errors"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
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
		r.Post("/repositories/{repoID}/secrets", h.save)
		r.Post("/repositories/{repoID}/secrets/build", h.setBuild)
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
	repoID, err := publicid.Param(r, "repoID", "repository")
	user, _, ok := h.users.UserIDs(r.Context())
	return user, repoID, err == nil && repoID > 0 && ok
}

// page shows the .env file with its values hidden, or with them in the
// editor after Show values and edit (?edit=1).
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	view := pages.Page{Notice: notice(query), Editing: query.Get("edit") == "1"}
	h.render(w, r, user, repoID, http.StatusOK, view, nil)
}

// notice confirms the last save or build-access change from its redirect
// query.
func notice(query url.Values) string {
	switch {
	case query.Get("saved") == "1":
		var parts []string
		for _, change := range []string{"added", "changed", "removed"} {
			if n, _ := strconv.Atoi(query.Get(change)); n > 0 && n <= 10000 {
				parts = append(parts, fmt.Sprintf("%d %s", n, change))
			}
		}
		if len(parts) == 0 {
			return "Saved the .env file. No values changed."
		}
		return "Saved the .env file: " + strings.Join(parts, ", ") + ". Previews get the new values when they are next built."
	case namePattern.MatchString(query.Get("build_on")):
		return "Builds of this repository's own branches can now read " + query.Get("build_on") + ". It takes effect at the next build."
	case namePattern.MatchString(query.Get("build_off")):
		return "Builds no longer receive " + query.Get("build_off") + "."
	}
	return ""
}

// submitted is an edited file shown again after a refused save, so nothing
// typed is lost. Revision -1 takes the current file's revision.
type submitted struct {
	text     []byte
	revision int64
}

// save stores the edited .env file and makes the repository's secrets the
// names it sets.
func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	// Browsers submit textarea line breaks as CRLF; store \n line breaks.
	text := []byte(strings.ReplaceAll(r.PostFormValue("env"), "\r\n", "\n"))
	defer clear(text)
	revision, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil || revision < 0 {
		revision = 0
	}
	saved, err := h.service.SaveFile(r.Context(), user, repoID, text, revision, r.PostFormValue("confirm_removals") == "1")
	if err != nil {
		view, status := pages.Page{Editing: true}, http.StatusUnprocessableEntity
		again := &submitted{text: text, revision: revision}
		var invalid *InvalidError
		var removals *RemovalsError
		switch {
		case errors.As(err, &removals):
			view.Removals, status = removals.Names, http.StatusOK
		case errors.As(err, &invalid):
			view.Error = invalid.Message + " Nothing was saved."
		case errors.Is(err, ErrLimit):
			view.Error = fmt.Sprintf("A .env file can set at most %d names. Nothing was saved.", MaxSecretsPerRepository)
		case errors.Is(err, ErrChanged):
			view.Error, status = "Someone saved this file after you opened it. Your version is below: saving it replaces theirs, or open the file again to see their changes.", http.StatusConflict
			again.revision = -1
		case errors.Is(err, ErrNotConfigured):
			view.Error, status = "Secrets are not configured on this server.", http.StatusServiceUnavailable
		case errors.Is(err, ErrUnavailable):
			// Saving needs the repository; new or changed values also need
			// current GitHub access.
			view.Error, status = "GitHub access to this repository was removed, so values cannot be added or changed. You can still remove variables or edit comments. Nothing was saved.", http.StatusConflict
		default:
			h.logger.ErrorContext(r.Context(), "save repository .env file failed", "repo_id", repoID)
			view.Error, status = "The .env file could not be saved. Please retry.", http.StatusServiceUnavailable
		}
		h.render(w, r, user, repoID, status, view, again)
		return
	}
	h.logger.InfoContext(r.Context(), "repository .env file saved", "repo_id", repoID,
		"added", len(saved.Added), "changed", len(saved.Changed), "removed", len(saved.Removed))
	query := url.Values{"saved": {"1"}}
	for name, names := range map[string][]string{"added": saved.Added, "changed": saved.Changed, "removed": saved.Removed} {
		if len(names) > 0 {
			query.Set(name, strconv.Itoa(len(names)))
		}
	}
	publicid.Redirect(w, r, secretsURL(repoID, query), http.StatusSeeOther)
}

// setBuild lets Dockerfile steps of trusted builds read one secret, or stops
// it.
func (h *Handler) setBuild(w http.ResponseWriter, r *http.Request) {
	user, repoID, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, build := r.PostFormValue("name"), r.PostFormValue("build") == "true"
	if _, err := h.service.SetBuild(r.Context(), user, repoID, name, build); err != nil {
		var invalid *InvalidError
		switch {
		case errors.As(err, &invalid):
			http.Error(w, invalid.Message, http.StatusBadRequest)
		case errors.Is(err, ErrUnavailable):
			h.render(w, r, user, repoID, http.StatusConflict, pages.Page{Error: "GitHub access to this repository was removed, so builds cannot be given new secrets."}, nil)
		default:
			h.logger.ErrorContext(r.Context(), "set build use of a secret failed", "repo_id", repoID)
			http.Error(w, "Secrets are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
		}
		return
	}
	h.logger.InfoContext(r.Context(), "secret build use changed", "repo_id", repoID, "build", build)
	key := "build_off"
	if build {
		key = "build_on"
	}
	publicid.Redirect(w, r, secretsURL(repoID, url.Values{key: {name}}), http.StatusSeeOther)
}

// render loads the owned repository, its names and its .env file. Values
// reach the page only in the editor.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, user, repoID int64, status int, view pages.Page, again *submitted) {
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
	view.MaxKiB = MaxFileBytes / 1024
	for _, s := range list {
		view.Secrets = append(view.Secrets, pages.Secret{Name: s.Name, UpdatedAt: s.UpdatedAt, Build: s.Build})
	}
	view.Editing = view.Editing && view.Configured
	if view.Configured {
		if err := h.loadFile(r.Context(), user, repoID, &view, again); err != nil {
			h.logger.ErrorContext(r.Context(), "open repository .env file failed", "repo_id", repoID)
			http.Error(w, "Secrets are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
			return
		}
		if view.Editing {
			w.Header().Set("Cache-Control", "no-store")
		}
	}
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Index(username, h.users.CSRFToken(r.Context()), view), view, templ.WithStatus(status)).ServeHTTP(w, r)
}

// loadFile puts the .env file on the page: its lines with values hidden, its
// size and last save, and in the editor the file itself (or the text of a
// refused save). An empty file has nothing to hide, so it opens the editor.
func (h *Handler) loadFile(ctx context.Context, user, repoID int64, view *pages.Page, again *submitted) error {
	file, err := h.service.File(ctx, user, repoID)
	if err != nil {
		return err
	}
	defer clear(file.Text)
	lines, err := MaskDotenv(string(file.Text))
	if err != nil {
		return err
	}
	for _, line := range lines {
		view.Lines = append(view.Lines, pages.Line{Comment: line.Comment, Name: line.Name, HasValue: line.HasValue})
	}
	view.UpdatedAt, view.UpdatedBy, view.Size = file.UpdatedAt, file.UpdatedBy, len(file.Text)
	view.Editing = view.Editing || len(view.Lines) == 0
	if !view.Editing {
		return nil
	}
	view.Text, view.Revision = string(file.Text), file.Revision
	if again != nil {
		view.Text, view.Size = string(again.text), len(again.text)
		if again.revision >= 0 {
			view.Revision = again.revision
		}
	}
	return nil
}
