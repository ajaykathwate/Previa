package dashboard

import (
	"context"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"net/http"

	"github.com/a-h/templ"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	faqpages "github.com/ajaykathwatee/previa/web/pages/faq"
	homepages "github.com/ajaykathwatee/previa/web/pages/home"
	opensourcepages "github.com/ajaykathwatee/previa/web/pages/opensource"
)

// UserSession supplies identity and CSRF data needed by the web pages.
type UserSession interface {
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// SummaryReader supplies counts without coupling dashboard to another feature.
type SummaryReader func(context.Context, int64) (dashboardpages.Summary, error)

type handler struct {
	users    UserSession
	summary  SummaryReader
	userID   func(context.Context) (int64, int64, bool)
	devTools bool
}

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	_, loggedIn := h.users.Username(r.Context())
	templ.Handler(homepages.Index(loggedIn)).ServeHTTP(w, r)
}

// openSourceTests shows the public Open Source Tested page to everyone, signed in or not.
func (h *handler) openSourceTests(w http.ResponseWriter, r *http.Request) {
	_, loggedIn := h.users.Username(r.Context())
	templ.Handler(opensourcepages.Page(loggedIn)).ServeHTTP(w, r)
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
	summary.DeveloperTools = h.devTools
	summary.Joined = r.URL.Query().Get("saved") == "joined"
	publicid.Handler(r, dashboardpages.Index(username, h.users.CSRFToken(r.Context()), summary), summary).ServeHTTP(w, r)
}

func (h *handler) protected(w http.ResponseWriter, r *http.Request) {
	if profile, ok := h.users.(interface {
		ServeProfile(http.ResponseWriter, *http.Request)
	}); ok {
		profile.ServeProfile(w, r)
		return
	}
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, dashboardpages.Protected(username, h.users.CSRFToken(r.Context())), nil).ServeHTTP(w, r)
}

func (h *handler) tools(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, dashboardpages.Tools(username, h.users.CSRFToken(r.Context()), h.devTools), nil).ServeHTTP(w, r)
}

// faq shows the workspace FAQ. The answers are public, so a signed-out visitor
// is sent to the same conversation on the home page instead of to sign-in.
func (h *handler) faq(w http.ResponseWriter, r *http.Request) {
	username, ok := h.users.Username(r.Context())
	if !ok {
		publicid.Redirect(w, r, "/#faq", http.StatusSeeOther)
		return
	}
	publicid.Handler(r, faqpages.Page(username, h.users.CSRFToken(r.Context())), nil).ServeHTTP(w, r)
}
