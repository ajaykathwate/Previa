package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestHomeUsesLayout(t *testing.T) {
	r := chi.NewRouter()
	Mount(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{"<!doctype html>", "Hello, preview platform", "/static/htmx.min.js", "Main navigation"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
}
