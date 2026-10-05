package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type anonymousSession struct{}

func (anonymousSession) Username(context.Context) (string, bool) { return "", false }
func (anonymousSession) CSRFToken(context.Context) string        { return "" }

func TestHomeUsesLayout(t *testing.T) {
	r := chi.NewRouter()
	Mount(r, anonymousSession{}, func(next http.Handler) http.Handler { return next })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{"<!doctype html>", "Review the branch.", "/static/htmx.min.js", "/static/css/app.css", "/static/js/ui.js", "Main navigation", "Get started with GitHub", "NOT A LIVE PREVIEW"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(w.Body.String(), "csrf_token") || strings.Contains(w.Body.String(), "data-queue-form") {
		t.Fatal("public home exposes authenticated workspace actions")
	}
}

type signedIn struct{}

func (signedIn) Username(context.Context) (string, bool) { return "owner", true }
func (signedIn) CSRFToken(context.Context) string        { return "token" }

func TestDeveloperToolsOnlyOutsideProduction(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	for _, enabled := range []bool{true, false} {
		r := chi.NewRouter()
		MountWithSummary(r, signedIn{}, pass, Options{DeveloperTools: enabled})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/developer-tools", nil))
		hasForm := strings.Contains(w.Body.String(), "data-queue-form")
		if w.Code != http.StatusOK || hasForm != enabled || strings.Contains(w.Body.String(), "off in production") == enabled {
			t.Fatalf("developer tools enabled=%v: %d form=%v", enabled, w.Code, hasForm)
		}
	}
}
