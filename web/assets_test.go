package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedBrowserAssets(t *testing.T) {
	h := StaticHandler()
	for _, path := range []string{"/static/css/app.css", "/static/htmx.min.js", "/static/js/ui.js"} {
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

func TestBrandAssetsAndFavicon(t *testing.T) {
	h := StaticHandler()
	for path, kind := range map[string]string{
		"/static/brand/favicon.svg":             "image/svg+xml",
		"/static/brand/favicon-32.png":          "image/png",
		"/static/brand/apple-touch-icon.png":    "image/png",
		"/static/brand/previa-mark.png":         "image/png",
		"/static/brand/previa-mark-on-dark.png": "image/png",
		"/static/brand/previa-logo.png":         "image/png",
		"/static/brand/previa-logo-on-dark.png": "image/png",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), kind) {
			t.Errorf("%s: status=%d type=%q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	w := httptest.NewRecorder()
	FaviconHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Body.String(), "\x89PNG") {
		t.Fatalf("/favicon.ico: status=%d type=%q", w.Code, w.Header().Get("Content-Type"))
	}
}
