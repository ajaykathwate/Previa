package source

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v91/github"
)

type clientsFunc func(int64) (*github.Client, error)

func (f clientsFunc) InstallationClient(id int64) (*github.Client, error) { return f(id) }

// fakeGitHub serves the API (plain HTTP, token required) and redirects archive
// requests to location().
type fakeGitHub struct {
	api      *httptest.Server
	paths    []string
	status   int
	location func() string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{status: http.StatusFound}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("API request without installation authentication")
		}
		switch r.URL.Path {
		case "/repositories/100":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":100,"name":"repo","owner":{"login":"owner"}}`))
		case "/repos/owner/repo/tarball/" + testSHA:
			if f.status != http.StatusFound {
				w.WriteHeader(f.status)
				_, _ = w.Write([]byte(`{"message":"fixture"}`))
				return
			}
			w.Header().Set("Location", f.location())
			w.WriteHeader(http.StatusFound)
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.api.Close)
	return f
}

func (f *fakeGitHub) clients(t *testing.T) InstallationClients {
	base := f.api.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(f.api.Client()), github.WithURLs(&base, &base), github.WithAuthToken("fixture-token"))
	if err != nil {
		t.Fatal(err)
	}
	return clientsFunc(func(id int64) (*github.Client, error) {
		if id != 99 {
			t.Errorf("wrong installation %d", id)
		}
		return client, nil
	})
}

func host(server *httptest.Server) string {
	u, _ := url.Parse(server.URL)
	return u.Host
}

func TestArchivesFetchExactCommitWithoutForwardingCredentials(t *testing.T) {
	archive := tarball(t, testSHA, validConfig, validDockerfile)
	var codeloadPath string
	codeload := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeloadPath = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("installation credentials forwarded to the archive host")
		}
		if r.URL.Query().Get("token") != "short-lived-secret" {
			t.Error("archive host lost its own signed URL")
		}
		_, _ = w.Write(archive)
	}))
	defer codeload.Close()
	gh := newFakeGitHub(t)
	gh.location = func() string {
		return codeload.URL + "/owner/repo/legacy.tar.gz/" + testSHA + "?token=short-lived-secret"
	}
	archives, err := NewGitHubArchives(gh.clients(t), WithDownloadClient(codeload.Client()), WithArchiveHosts(host(codeload)))
	if err != nil {
		t.Fatal(err)
	}
	preparer, workspaces := newTestPreparer(t, archives, Limits{})
	prepared, err := preparer.Prepare(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if codeloadPath != "/owner/repo/legacy.tar.gz/"+testSHA {
		t.Fatalf("archive fetched for %q, not the exact commit", codeloadPath)
	}
	// Fork or not, only the base repository (by numeric ID) is ever asked.
	if strings.Join(gh.paths, ",") != "/repositories/100,/repos/owner/repo/tarball/"+testSHA {
		t.Fatalf("unexpected API requests: %v", gh.paths)
	}
	if err := preparer.Release(prepared); err != nil {
		t.Fatal(err)
	}
	assertNoWorkspaces(t, workspaces)
}

func TestArchivesRejectUnsafeLocationsAndRedirects(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials reached a disallowed host")
		}
	}))
	defer evil.Close()
	var loops atomic.Int32
	codeload := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-evil":
			http.Redirect(w, r, evil.URL+"/steal?token=short-lived-secret", http.StatusFound)
		case "/loop":
			loops.Add(1)
			http.Redirect(w, r, "/loop?token=short-lived-secret", http.StatusFound)
		case "/gone":
			http.NotFound(w, r)
		case "/broken":
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("cannot hijack")
				return
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer codeload.Close()
	cases := []struct {
		name     string
		location string
		code     string
	}{
		{"API points at a disallowed host", evil.URL + "/archive?token=short-lived-secret", CodeRedirectRejected},
		{"API points at plain HTTP", strings.Replace(codeload.URL, "https://", "http://", 1) + "/archive", CodeRedirectRejected},
		{"API points at credentials in URL", strings.Replace(codeload.URL, "https://", "https://user:pass@", 1) + "/archive", CodeRedirectRejected},
		{"archive host redirects to a disallowed host", codeload.URL + "/to-evil", CodeRedirectRejected},
		{"redirect loop", codeload.URL + "/loop", CodeRedirectRejected},
		{"archive missing", codeload.URL + "/gone", CodeUnavailable},
		{"connection dropped", codeload.URL + "/broken", CodeAPIUnavailable},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.location = func() string { return tt.location }
			archives, err := NewGitHubArchives(gh.clients(t), WithDownloadClient(codeload.Client()), WithArchiveHosts(host(codeload)), WithMaxRedirects(2))
			if err != nil {
				t.Fatal(err)
			}
			preparer, workspaces := newTestPreparer(t, archives, Limits{})
			_, err = preparer.Prepare(t.Context(), testRequest())
			if CodeOf(err) != tt.code {
				t.Fatalf("code = %q (%v); want %q", CodeOf(err), err, tt.code)
			}
			if strings.Contains(err.Error(), "short-lived-secret") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error leaks the archive URL: %v", err)
			}
			if evilHits.Load() != 0 {
				t.Fatal("a disallowed host was contacted")
			}
			assertNoWorkspaces(t, workspaces)
		})
	}
	if loops.Load() > 3 {
		t.Fatalf("redirects were not bounded: %d", loops.Load())
	}
}

func TestArchivesClassifyAPIFailures(t *testing.T) {
	for status, code := range map[int]string{
		http.StatusNotFound:            CodeUnavailable,
		http.StatusUnprocessableEntity: CodeUnavailable,
		http.StatusForbidden:           CodeAccessDenied,
		http.StatusUnauthorized:        CodeAccessDenied,
		http.StatusInternalServerError: CodeAPIError,
	} {
		gh := newFakeGitHub(t)
		gh.status = status
		archives, err := NewGitHubArchives(gh.clients(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = archives.Open(t.Context(), testRequest())
		if CodeOf(err) != code {
			t.Fatalf("HTTP %d: code %q (%v); want %q", status, CodeOf(err), err, code)
		}
		var e *Error
		if errors.As(err, &e) && e.Retryable != (status >= 500) {
			t.Fatalf("HTTP %d: retryable = %v", status, e.Retryable)
		}
	}
	if _, err := NewGitHubArchives(nil); err == nil {
		t.Fatal("archives require installation clients")
	}
	unauthenticated, err := NewGitHubArchives(clientsFunc(func(int64) (*github.Client, error) { return nil, errors.New("no key") }))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unauthenticated.Open(t.Context(), testRequest()); CodeOf(err) != CodeAuthUnavailable {
		t.Fatalf("missing installation auth: %v", err)
	}
}
