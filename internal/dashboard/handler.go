package dashboard

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	homepages "github.com/ajaykathwatee/previa/web/pages/home"
)

// UserSession supplies identity and CSRF data needed by the web pages.
type UserSession interface {
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// SummaryReader supplies counts without coupling dashboard to another feature.
type SummaryReader func(context.Context, int64) (dashboardpages.Summary, error)

type handler struct {
	users   UserSession
	summary SummaryReader
	userID  func(context.Context) (int64, int64, bool)
}

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	_, loggedIn := h.users.Username(r.Context())
	templ.Handler(homepages.Index(loggedIn)).ServeHTTP(w, r)
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	var summary dashboardpages.Summary
	if h.summary != nil {
		id, _, _ := h.userID(r.Context())
		var err error
		summary, err = h.summary(r.Context(), id)
		if err != nil {
			http.Error(w, "Repository connections are temporarily unavailable.", http.StatusServiceUnavailable)
			return
		}
	}
	templ.Handler(dashboardpages.Index(username, h.users.CSRFToken(r.Context()), summary)).ServeHTTP(w, r)
}

func (h *handler) protected(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(dashboardpages.Protected(username, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}

func (h *handler) tools(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(dashboardpages.Tools(username, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}
