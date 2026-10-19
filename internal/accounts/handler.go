package accounts

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	pages "github.com/ajaykathwatee/previa/web/pages/accounts"
	"github.com/go-chi/chi/v5"
)

type Users interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}
type Handler struct {
	s       *Service
	users   Users
	log     *slog.Logger
	signOut func(context.Context) error
	secure  bool
}

func NewHandler(s *Service, u Users, l *slog.Logger, signOut func(context.Context) error, secure bool) *Handler {
	return &Handler{s, u, l, signOut, secure}
}
func (h *Handler) Mount(r chi.Router, require func(http.Handler) http.Handler) {
	r.With(require).Get("/account", h.page)
	r.With(require).Post("/account/delete", h.personal)
	r.With(require).Post("/account/repositories/{repoID}/disconnect", h.repository)
	r.With(require).Post("/account/installations/{installationID}/disconnect", h.installation)
	r.With(require).Post("/account/workspaces/{workspaceID}/delete", h.workspace)
	r.With(require).Get("/account/operations/{id}", h.progress)
	r.Get("/account/deletion", h.receipt)
}
func (h *Handler) model(r *http.Request) pages.Page {
	n, _ := h.users.Username(r.Context())
	return pages.Page{Name: n, CSRF: h.users.CSRFToken(r.Context())}
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusServiceUnavailable
	message := "Account actions are temporarily unavailable. Please try again."
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrConflict), errors.Is(err, ErrConfirmation), errors.Is(err, ErrOwnership):
		status = http.StatusConflict
		message = err.Error()
	default:
		h.log.ErrorContext(r.Context(), "account action failed", "error", err)
	}
	p := h.model(r)
	p.Error = message
	publicid.Handler(r, pages.Index(p), nil, templ.WithStatus(status)).ServeHTTP(w, r)
}
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	u, _, _ := h.users.UserIDs(r.Context())
	catalog, err := h.s.Catalog(r.Context(), u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, v := range catalog.Workspaces {
		p.Workspaces = append(p.Workspaces, pages.Workspace(v))
	}
	for _, v := range catalog.Installations {
		p.Installations = append(p.Installations, pages.Installation(v))
	}
	for _, v := range catalog.Repositories {
		p.Repositories = append(p.Repositories, pages.Installation(v))
	}
	for _, v := range catalog.Operations {
		p.Operations = append(p.Operations, pages.Operation(v))
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Index(p), nil).ServeHTTP(w, r)
}
func revision(r *http.Request) int64 {
	n, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil {
		return -1
	}
	return n
}
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, o Operation, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/operations/"+o.ID, http.StatusSeeOther)
}
func (h *Handler) repository(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	o, err := h.s.DisconnectRepository(r.Context(), u, publicid.PathID(r, "repoID", "repository"), revision(r), r.PostFormValue("confirm"))
	h.redirect(w, r, o, err)
}
func (h *Handler) installation(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	o, err := h.s.DisconnectInstallation(r.Context(), u, publicid.PathID(r, "installationID", "installation"), revision(r), r.PostFormValue("confirm"))
	h.redirect(w, r, o, err)
}
func (h *Handler) workspace(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	o, err := h.s.StartWorkspaceDeletion(r.Context(), u, publicid.PathID(r, "workspaceID", "workspace"), revision(r), r.PostFormValue("confirm"))
	h.redirect(w, r, o, err)
}
func (h *Handler) personal(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	_, receipt, err := h.s.StartAccountDeletion(r.Context(), u, r.PostFormValue("confirm"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.signOut(r.Context()); err != nil {
		h.log.ErrorContext(r.Context(), "end deleted account session", "error", err)
	}
	http.SetCookie(w, &http.Cookie{Name: "previa_deletion_receipt", Value: receipt, Path: "/account/deletion", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 86400})
	http.Redirect(w, r, "/account/deletion", http.StatusSeeOther)
}
func (h *Handler) showProgress(w http.ResponseWriter, r *http.Request, o Operation) {
	p := h.model(r)
	p.Progress = &pages.Progress{ID: o.ID, Kind: o.Kind, Stage: o.Stage, Error: o.Error, Created: o.Created.UTC().Format(time.RFC3339), Journal: o.JournalAcknowledged != nil}
	if o.Completed != nil {
		p.Progress.Completed = o.Completed.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.ProgressPage(p), nil).ServeHTTP(w, r)
}
func (h *Handler) progress(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	o, err := h.s.Read(r.Context(), u, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.showProgress(w, r, o)
}
func (h *Handler) receipt(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("previa_deletion_receipt")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	o, err := h.s.ReadReceipt(r.Context(), cookie.Value)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.showProgress(w, r, o)
}
