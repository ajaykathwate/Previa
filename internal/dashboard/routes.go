package dashboard

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers the dashboard's routes.
func Mount(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler) {
	MountWithInstallations(r, users, requireUser, nil, nil)
}

// MountWithInstallations connects an optional feature-owned installation reader.
func MountWithInstallations(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler, reader InstallationList, identity func(context.Context) (int64, int64, bool)) {
	h := &handler{users: users, installations: reader, userID: identity}
	r.Get("/", h.home)
	r.With(requireUser).Get("/dashboard", h.dashboard)
	r.With(requireUser).Get("/protected", h.protected)
}
