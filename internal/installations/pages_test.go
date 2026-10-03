package installations

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type pageStore struct {
	fakeStore
	lastUser   int64
	lastFilter RepositoryFilter
	err        error
}

func (s *pageStore) Page(_ context.Context, user int64, filter RepositoryFilter) (RepositoryPage, error) {
	s.lastUser = user
	s.lastFilter = filter
	return RepositoryPage{Page: min(filter.Page, 2), Pages: 2, Total: 26, Rows: []RepositoryRow{{FullName: "owner/repo", Account: "owner", Status: "active", Accessible: true, InstallationID: 99}}}, s.err
}
func (*pageStore) Summary(context.Context, int64) (WorkspaceSummary, error) {
	return WorkspaceSummary{Installations: 1}, nil
}
func TestRepositoryHTTPValidationScopeAndOptionalConfiguration(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	memory := memstore.NewWithCleanupInterval(0)
	sessions := auth.NewSessionManager(memory, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: "http://localhost", SessionSecret: strings.Repeat("s", 32)}, sessions, fixtureUsers{}, fixtureOAuth{}, logger)
	store := &pageStore{}
	handler := NewHandler(NewService(store, nil, nil, nil), users, sessions, "", logger)
	router := chi.NewRouter()
	router.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF)
	router.Get("/fixture-login", func(w http.ResponseWriter, r *http.Request) {
		sessions.Put(r.Context(), "user_id", int64(1))
		w.WriteHeader(200)
	})
	handler.Mount(router, auth.RequireUser)
	server := httptest.NewServer(router)
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(path string) (int, string, string) {
		t.Helper()
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Location"), string(body)
	}
	if code, _, _ := request("/repositories"); code != 303 {
		t.Fatal("anonymous repository access allowed")
	}
	request("/fixture-login")
	if code, _, body := request("/repositories?q=owner&access=available"); code != 200 || !strings.Contains(body, "owner/repo") || !strings.Contains(body, "Not configured") {
		t.Fatal("stored repositories unavailable without App configuration")
	}
	if store.lastUser != 1 || store.lastFilter.Search != "owner" || store.lastFilter.Access != "available" {
		t.Fatal("handler did not pass authenticated identity and filters")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/repositories?q=owner&access=available", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "repository-results")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fragment, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if response.StatusCode != 200 || !strings.Contains(string(fragment), "owner/repo") || strings.Contains(string(fragment), "<!doctype") || strings.Contains(string(fragment), `id="repository-filters"`) {
		t.Fatal("HTMX request did not return only repository results")
	}
	if response.Header.Get("Vary") == "" {
		t.Fatal("partial/full representations missing Vary header")
	}
	if code, _, _ := request("/repositories"); code != 200 || store.lastFilter.Access != "available" {
		t.Fatal("repository page did not default to available")
	}
	if code, _, _ := request("/repositories?access=all"); code != 200 || store.lastFilter.Access != "all" {
		t.Fatal("explicit all filter was not preserved")
	}
	for _, path := range []string{"/repositories?page=0", "/repositories?page=-1", "/repositories?page=abc", "/repositories?page=9223372036854775808", "/repositories?access=invalid", "/repositories?q=" + strings.Repeat("x", 101)} {
		if code, _, _ := request(path); code != 400 {
			t.Fatalf("invalid input accepted: %s", path)
		}
	}
	if code, location, _ := request("/repositories?page=100&q=owner&access=available"); code != 303 || location != "/repositories?access=available&page=2&q=owner" {
		t.Fatal("out-of-range page did not preserve filters in redirect")
	}
	store.err = errors.New("private database error")
	if code, _, body := request("/repositories"); code != 503 || strings.Contains(body, "private database error") {
		t.Fatal("database failure was not handled safely")
	}
}
