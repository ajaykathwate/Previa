package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedBrowserAssets(t *testing.T) {
	h := StaticHandler()
	for _, path := range []string{"/static/css/app.css", "/static/htmx.min.js"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Fatalf("asset response: status=%d bytes=%d", w.Code, w.Body.Len())
			}
			if strings.HasSuffix(path, ".css") {
				if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
					t.Fatalf("CSS content type: %q", w.Header().Get("Content-Type"))
				}
				// Verify component-only variants were detected by the asset compiler.
				for _, selector := range []string{".bg-primary", ".bg-card", ".border-input", ".text-destructive"} {
					if !strings.Contains(w.Body.String(), selector) {
						t.Errorf("compiled stylesheet missing %s", selector)
					}
				}
			}
		})
	}
	for _, path := range []string{"/pages/auth/error.templ", "/styles/app.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("source exposed at %s: status=%d", path, w.Code)
		}
	}
}
