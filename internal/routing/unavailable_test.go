package routing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnavailablePageIsAnUncachedCookieFree404(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, UnavailablePath, nil)
	r.AddCookie(&http.Cookie{Name: "previa_session", Value: "planted"})
	UnavailablePage(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
	h := w.Header()
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Robots-Tag") != "noindex" || h.Get("Referrer-Policy") != "no-referrer" || len(h.Values("Set-Cookie")) != 0 {
		t.Fatalf("headers %v", h)
	}
	if body := w.Body.String(); !strings.Contains(body, "Preview not found or expired") || strings.Contains(body, "planted") {
		t.Fatal("unexpected page content")
	}
}
