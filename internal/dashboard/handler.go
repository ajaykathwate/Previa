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

type handler struct{ users UserSession }

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	_, loggedIn := h.users.Username(r.Context())
	templ.Handler(homepages.Index(loggedIn)).ServeHTTP(w, r)
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(dashboardpages.Index(username, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}

func (h *handler) protected(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(dashboardpages.Protected(username, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}
