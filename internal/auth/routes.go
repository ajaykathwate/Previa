package auth

import "github.com/go-chi/chi/v5"

// Mount registers OAuth and session routes. Apply LoadAndSave, LoadUser, and CSRF first.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/auth/github/login", h.login)
	r.Get("/auth/github/callback", h.callback)
	r.With(RequireUser).Get("/auth/logout", h.logoutPage)
	r.With(RequireUser).Post("/auth/logout", h.logout)
}
