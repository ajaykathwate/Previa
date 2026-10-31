package support

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	pages "github.com/ajaykathwatee/previa/web/pages/support"
	"github.com/go-chi/chi/v5"
)

type Users interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}
type Handler struct {
	s     *Service
	users Users
	log   *slog.Logger
}

func NewHandler(s *Service, u Users, l *slog.Logger) *Handler { return &Handler{s, u, l} }
func (h *Handler) Mount(r chi.Router, require func(http.Handler) http.Handler) {
	r.Get("/help", h.help)
	r.Get("/status", h.status)
	r.Get("/policies/{kind}", h.policy)
	r.With(require).Post("/help/requests", h.submit)
	r.With(require).Get("/help/requests/{id}", h.help)
	r.With(require).Get("/operator/support", h.console)
	r.With(require).Get("/operator/support/{id}", h.console)
	r.With(require).Post("/operator/support/{id}/resolve", h.resolve)
	r.With(require).Post("/operator/incidents", h.incident)
}
func (h *Handler) model(r *http.Request) pages.Page {
	n, _ := h.users.Username(r.Context())
	_, gh, _ := h.users.UserIDs(r.Context())
	s := h.s.settings
	return pages.Page{Name: n, CSRF: h.users.CSRFToken(r.Context()), Email: s.Email, OperatorName: s.Operator, Operator: h.s.Operator(gh), MaxBytes: strconv.Itoa(s.MaxBytes), PerDay: strconv.Itoa(s.PerDay), RetentionDays: strconv.Itoa(s.RetentionDays), BackupDays: strconv.Itoa(s.BackupDays), JournalDays: strconv.Itoa(s.JournalDays)}
}
func display(v Request) pages.Request {
	return pages.Request{ID: v.ID, Category: v.Category, Message: v.Message, Diagnostic: v.Diagnostic, Repository: v.Repository, PR: v.PR, State: v.State, Created: v.Created.UTC().Format(time.RFC3339), Revision: strconv.FormatInt(v.Revision, 10)}
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, ErrInput) || errors.Is(err, ErrLimit) {
		p := h.model(r)
		p.Error = err.Error()
		publicid.Handler(r, pages.Help(p), nil, templ.WithStatus(http.StatusBadRequest)).ServeHTTP(w, r)
		return
	}
	h.log.ErrorContext(r.Context(), "support request failed", "error", err)
	http.Error(w, "Support temporarily unavailable. Please try again or use the configured support email.", http.StatusServiceUnavailable)
}
func (h *Handler) help(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	u, g, _ := h.users.UserIDs(r.Context())
	if u > 0 {
		list, err := h.s.Requests(r.Context(), u, g, false)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, v := range list {
			p.Requests = append(p.Requests, display(v))
		}
		if id := chi.URLParam(r, "id"); id != "" {
			v, err := h.s.Read(r.Context(), u, g, id, false)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			d := display(v)
			p.Detail = &d
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Help(p), nil).ServeHTTP(w, r)
}
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	id, err := h.s.Create(r.Context(), u, scope.Current(r.Context()).WorkspaceID, r.PostFormValue("category"), r.PostFormValue("message"), r.PostFormValue("repository"), r.PostFormValue("pr"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/help/requests/"+id, http.StatusSeeOther)
}
func (h *Handler) loadStatus(r *http.Request, p *pages.Page) error {
	a, i, err := h.s.Status(r.Context())
	if err != nil {
		return err
	}
	for _, v := range a {
		checked := ""
		if v.Checked != nil {
			checked = v.Checked.UTC().Format(time.RFC3339)
		}
		p.Availability = append(p.Availability, pages.Availability{Component: v.Component, State: v.State, Checked: checked})
	}
	for _, v := range i {
		p.Incidents = append(p.Incidents, pages.Incident{ID: v.ID, Component: v.Component, Summary: v.Summary, State: v.State, Revision: strconv.FormatInt(v.Revision, 10), Updated: v.Updated.UTC().Format(time.RFC3339)})
	}
	return nil
}
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	if err := h.loadStatus(r, &p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Status(p), nil).ServeHTTP(w, r)
}
func (h *Handler) policy(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	if !oneOf(kind, "privacy", "terms", "acceptable-use") {
		http.NotFound(w, r)
		return
	}
	publicid.Handler(r, pages.Policy(h.model(r), kind), nil).ServeHTTP(w, r)
}
func (h *Handler) console(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	if !p.Operator {
		http.NotFound(w, r)
		return
	}
	p.Console = true
	u, g, _ := h.users.UserIDs(r.Context())
	pending, pendingErr := h.s.PendingCleanup(r.Context(), g)
	if pendingErr != nil {
		h.fail(w, r, pendingErr)
		return
	}
	for _, v := range pending {
		p.Cleanup = append(p.Cleanup, pages.Cleanup(v))
	}
	list, err := h.s.Requests(r.Context(), u, g, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, v := range list {
		p.Requests = append(p.Requests, display(v))
	}
	if id := chi.URLParam(r, "id"); id != "" {
		v, err := h.s.Read(r.Context(), u, g, id, true)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		d := display(v)
		p.Detail = &d
	}
	if err := h.loadStatus(r, &p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Console(p), nil).ServeHTTP(w, r)
}
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	_, g, _ := h.users.UserIDs(r.Context())
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err := h.s.Resolve(r.Context(), g, chi.URLParam(r, "id"), revision); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/operator/support/"+chi.URLParam(r, "id"), http.StatusSeeOther)
}
func (h *Handler) incident(w http.ResponseWriter, r *http.Request) {
	_, g, _ := h.users.UserIDs(r.Context())
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err := h.s.Incident(r.Context(), g, r.PostFormValue("id"), r.PostFormValue("component"), r.PostFormValue("summary"), r.PostFormValue("state"), revision); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/operator/support", http.StatusSeeOther)
}
