package auth

import "github.com/go-chi/chi/v5"

// Mount registers OAuth and session routes. Apply LoadAndSave, LoadUser, and CSRF first.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/auth/github/login", h.login)
	r.With(RequireUser).Post("/protected/profile", h.updatePersonalName)
	r.With(RequireUser).Post("/protected/profile/avatar", h.uploadAvatar)
	r.With(RequireUser).Post("/protected/profile/avatar/remove", h.removeAvatar)
	r.With(RequireUser).Get("/avatars/{userID}", h.serveAvatar)
	r.Get("/auth/github/callback", h.callback)
	r.With(RequireUser).Get("/auth/logout", h.logoutConfirmation)
	r.With(RequireUser).Post("/auth/logout", h.logout)
}
