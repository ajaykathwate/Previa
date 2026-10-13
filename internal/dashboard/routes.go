package dashboard

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Options connect the dashboard to other features.
type Options struct {
	Summary  SummaryReader                              // the Overview's counts
	Identity func(context.Context) (int64, int64, bool) // the signed-in user's IDs
	// DeveloperTools offers the queue ping; production leaves it off.
	DeveloperTools bool
}

// Mount registers the dashboard's routes with developer tools on.
func Mount(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler) {
	MountWithSummary(r, users, requireUser, Options{DeveloperTools: true})
}

// MountWithSummary registers the dashboard's routes with feature-owned data.
func MountWithSummary(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler, o Options) {
	h := &handler{users: users, summary: o.Summary, userID: o.Identity, devTools: o.DeveloperTools}
	r.Get("/", h.home)
	r.Get("/open-source-tests", h.openSourceTests)
	r.With(requireUser).Get("/dashboard", h.dashboard)
	r.With(requireUser).Get("/protected", h.protected)
	r.With(requireUser).Get("/developer-tools", h.tools)
	// Public: signed-out visitors are sent to the same answers on the home page.
	r.Get("/faq", h.faq)
}
