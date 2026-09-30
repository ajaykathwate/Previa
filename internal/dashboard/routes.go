package dashboard

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers the dashboard's routes.
func Mount(r chi.Router, users UserSession, requireUser func(http.Handler) http.Handler) {
	h := &handler{users: users}
	r.Get("/", h.home)
	r.With(requireUser).Get("/protected", h.protected)
}
