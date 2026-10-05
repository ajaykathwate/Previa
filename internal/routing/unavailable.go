package routing

import (
	"net/http"

	routingpages "github.com/ajaykathwatee/previa/web/pages/routing"
)

// UnavailablePage serves the dashboard page the router redirects unknown and
// removed preview hosts to. It is public, reads no session, sets no cookie and
// is never cached; its 404 status tells crawlers and monitors the truth.
func UnavailablePage(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Robots-Tag", "noindex")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = routingpages.Unavailable().Render(r.Context(), w)
}
