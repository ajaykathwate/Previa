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

// InstallationList supplies display data without coupling dashboard to another feature.
type InstallationList func(context.Context, int64) ([]dashboardpages.Installation, error)

type handler struct {
	users         UserSession
	installations InstallationList
	userID        func(context.Context) (int64, int64, bool)
}

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	_, loggedIn := h.users.Username(r.Context())
	templ.Handler(homepages.Index(loggedIn)).ServeHTTP(w, r)
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	var linked []dashboardpages.Installation
	if h.installations != nil {
		id, _, _ := h.userID(r.Context())
		var err error
		linked, err = h.installations(r.Context(), id)
		if err != nil {
			http.Error(w, "Repository connections are temporarily unavailable.", http.StatusServiceUnavailable)
			return
		}
	}
	templ.Handler(dashboardpages.Index(username, h.users.CSRFToken(r.Context()), h.installations != nil, linked)).ServeHTTP(w, r)
}

func (h *handler) protected(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(dashboardpages.Protected(username, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}
