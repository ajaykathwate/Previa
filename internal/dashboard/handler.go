package dashboard

import (
	"net/http"

	"github.com/a-h/templ"
)

func home(w http.ResponseWriter, r *http.Request) {
	templ.Handler(page()).ServeHTTP(w, r)
}
