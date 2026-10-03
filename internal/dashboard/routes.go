package dashboard

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers the dashboard's routes.
func Mount(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler) {
	MountWithSummary(r, users, requireUser, nil, nil)
}

// MountWithSummary connects an feature-owned workspace summary reader.
func MountWithSummary(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler, reader SummaryReader, identity func(context.Context) (int64, int64, bool)) {
	h := &handler{users: users, summary: reader, userID: identity}
	r.Get("/", h.home)
	r.With(requireUser).Get("/dashboard", h.dashboard)
	r.With(requireUser).Get("/protected", h.protected)
	r.With(requireUser).Get("/developer-tools", h.tools)
}
