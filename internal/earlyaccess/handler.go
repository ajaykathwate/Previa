package earlyaccess

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	pages "github.com/ajaykathwatee/previa/web/pages/earlyaccess"
	"github.com/go-chi/chi/v5"
)

type Users interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}
type Handler struct {
	service *Service
	users   Users
	log     *slog.Logger
}

func NewHandler(service *Service, users Users, log *slog.Logger) *Handler {
	return &Handler{service, users, log}
}
func (h *Handler) Mount(r chi.Router, require func(http.Handler) http.Handler) {
	r.With(require).Get("/early-access", h.page)
	r.With(require).Get("/operator/early-access", h.console)
	r.With(require).Post("/operator/early-access/{workspace}/approve", h.approve)
}
func (h *Handler) model(r *http.Request) pages.Page {
	name, _ := h.users.Username(r.Context())
	_, github, _ := h.users.UserIDs(r.Context())
	return pages.Page{Name: name, CSRF: h.users.CSRFToken(r.Context()), Operator: h.service.Operator(github), Enabled: h.service.enabled}
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.log.ErrorContext(r.Context(), "early-access action failed", "error", err)
	http.Error(w, "Early-access approval could not be checked. Please try again.", http.StatusServiceUnavailable)
}
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	user, _, _ := h.users.UserIDs(r.Context())
	if id := scope.WorkspaceID(r.Context()); id > 0 {
		v, err := h.service.Workspace(r.Context(), user, id)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.Workspace = pages.Workspace(v)
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Status(p), nil).ServeHTTP(w, r)
}
func (h *Handler) console(w http.ResponseWriter, r *http.Request) {
	p := h.model(r)
	_, operator, _ := h.users.UserIDs(r.Context())
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 || offset > 10000 {
		offset = 0
	}
	values, more, err := h.service.Pending(r.Context(), operator, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, v := range values {
		p.Pending = append(p.Pending, pages.Workspace(v))
	}
	if more {
		p.Next = strconv.Itoa(offset + 25)
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.Console(p), nil).ServeHTTP(w, r)
}
func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	_, operator, _ := h.users.UserIDs(r.Context())
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err := h.service.Approve(r.Context(), operator, chi.URLParam(r, "workspace"), revision); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/operator/early-access", http.StatusSeeOther)
}

// Guard is presentation feedback; repository transactions and worker usage
// admission independently enforce the gate, including baseline execution.
func (h *Handler) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.service.enabled {
			next.ServeHTTP(w, r)
			return
		}
		if _, _, ok := h.users.UserIDs(r.Context()); !ok {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		// OAuth callbacks use the workspace saved when connection started, which
		// can differ from the workspace currently selected in another tab.
		// Repository.Link checks that original workspace in its transaction.
		connect := path == "/installations/start"
		build := false
		if r.Method == http.MethodPost {
			for _, suffix := range []string{"/build", "/build-now", "/retry", "/rebuild", "/refresh", "/reset"} {
				if strings.HasPrefix(path, "/repositories/") && strings.HasSuffix(path, suffix) {
					build = true
				}
			}
		}
		setup := strings.HasPrefix(path, "/workspaces/") && strings.HasSuffix(path, "/setup")
		if !connect && !build && !setup {
			next.ServeHTTP(w, r)
			return
		}
		allowed, err := h.service.Allowed(r.Context(), scope.WorkspaceID(r.Context()))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if allowed {
			next.ServeHTTP(w, r)
			return
		}
		// Pages and plain forms (such as Install GitHub) go to the status page,
		// which explains what is pending; in-page requests show this text.
		if r.Method == http.MethodGet || (r.Header.Get("HX-Request") != "true" && !httpx.WantsJSON(r)) {
			http.Redirect(w, r, "/early-access", http.StatusSeeOther)
			return
		}
		http.Error(w, "This workspace needs early-access approval. Visit /early-access to see its status.", http.StatusForbidden)
	})
}
