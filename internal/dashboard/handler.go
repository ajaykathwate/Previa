package dashboard

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
)

// UserSession supplies only the identity information the placeholder page needs.
type UserSession interface {
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

type handler struct{ users UserSession }

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	username, loggedIn := h.users.Username(r.Context())
	templ.Handler(page(username, loggedIn, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}

func (h *handler) protected(w http.ResponseWriter, r *http.Request) {
	username, _ := h.users.Username(r.Context())
	templ.Handler(protectedPage(username)).ServeHTTP(w, r)
}
