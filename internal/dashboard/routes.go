package dashboard

import "github.com/go-chi/chi/v5"

// Mount registers the dashboard's routes.
func Mount(r chi.Router) {
	r.Get("/", home)
}
