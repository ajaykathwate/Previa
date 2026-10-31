package onboarding

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v91/github"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

type clientsFunc func(int64) (*github.Client, error)

func (f clientsFunc) InstallationClient(id int64) (*github.Client, error) { return f(id) }

// fakeAPI answers the GitHub API calls a setup check makes.
func fakeAPI(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) bool) Files {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("GitHub request without installation authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repositories/100" {
			_, _ = w.Write([]byte(`{"id":100,"name":"api","owner":{"login":"acme"}}`))
			return
		}
		if !handle(w, r) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(server.Close)
	base := server.URL + "/"
	files, err := NewGitHubFiles(clientsFunc(func(id int64) (*github.Client, error) {
		if id != 7 {
			t.Errorf("installation %d", id)
		}
		return github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&base, &base), github.WithAuthToken("fixture-token"))
	}))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func fileJSON(name, content string) string {
	return `{"type":"file","name":"` + name + `","path":"` + name + `","size":` + strconv.Itoa(len(content)) + `,"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"}`
}

func TestGitHubFilesReadsOneCommitThroughTheAPI(t *testing.T) {
	files := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("ref") != testSHA && strings.Contains(r.URL.Path, "/contents/") {
			t.Errorf("read without the exact commit: %s", r.URL)
		}
		switch r.URL.Path {
		case "/repos/acme/api/contents/.previewrc.yaml":
			_, _ = w.Write([]byte(fileJSON(".previewrc.yaml", "port: 8080\n")))
		case "/repos/acme/api/contents/Dockerfile":
			_, _ = w.Write([]byte(fileJSON("Dockerfile", "FROM scratch")))
		case "/repos/acme/api/contents/web":
			_, _ = w.Write([]byte(`[{"type":"file","name":"x","path":"web/x"}]`))
		case "/repos/acme/api/contents/linked":
			_, _ = w.Write([]byte(`{"type":"symlink","name":"linked","path":"linked","target":"Dockerfile"}`))
		default:
			return false
		}
		return true
	})
	tree, err := files.Commit(t.Context(), 7, 100, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := tree.Config(t.Context()); err != nil || string(data) != "port: 8080\n" {
		t.Fatalf("config: %q %v", data, err)
	}
	for name, want := range map[string]string{"Dockerfile": EntryFile, "web": EntryDir, "linked": EntryLink, "missing": EntryMissing} {
		if kind, err := tree.Stat(t.Context(), name); err != nil || kind != want {
			t.Errorf("stat %s: %s %v", name, kind, err)
		}
	}
	if _, err := tree.Read(t.Context(), "web", 1024); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("a directory is not readable as a file: %v", err)
	}
	if _, err := tree.Read(t.Context(), "Dockerfile", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
}

func TestGitHubFilesCallsAConfigurationMissingOnlyForAReadableCommit(t *testing.T) {
	readable := true
	files := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/acme/api/commits/"+testSHA && readable {
			_, _ = w.Write([]byte(testSHA))
			return true
		}
		return false
	})
	tree, err := files.Commit(t.Context(), 7, 100, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Config(t.Context()); !errors.Is(err, ErrMissing) {
		t.Fatalf("readable commit without the file: %v", err)
	}
	readable = false
	if _, err := tree.Config(t.Context()); failureCode(err) != FailAccess {
		t.Fatalf("an unreadable commit is lost access, not a missing file: %v", err)
	}
}

func TestGitHubFilesClassifiesFailures(t *testing.T) {
	status := http.StatusTooManyRequests
	files := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/repos/acme/api/contents/big":
			_, _ = w.Write([]byte(`{"type":"file","name":"big","path":"big","content":"` + strings.Repeat("A", maxResponseBytes) + `"}`))
		default:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"fixture"}`))
		}
		return true
	})
	tree, err := files.Commit(t.Context(), 7, 100, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]string{http.StatusTooManyRequests: FailRateLimited, http.StatusBadGateway: FailGitHubDown, http.StatusForbidden: FailAccess, http.StatusTeapot: FailGitHubError} {
		status = code
		if _, err := tree.Stat(t.Context(), "Dockerfile"); failureCode(err) != want {
			t.Errorf("status %d: %v", code, err)
		}
	}
	if kind, err := tree.Stat(t.Context(), "big"); err != nil || kind != EntryLarge {
		t.Fatalf("an answer above the bound still proves the file exists: %s %v", kind, err)
	}
	if _, err := tree.Read(t.Context(), "big", 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversized file is not read: %v", err)
	}
}

func TestGitHubFilesNeedsTheApp(t *testing.T) {
	if _, err := NewGitHubFiles(nil); err == nil {
		t.Fatal("no App accepted")
	}
	files, _ := NewGitHubFiles(clientsFunc(func(int64) (*github.Client, error) { return nil, errors.New("no key") }))
	if _, err := files.Commit(t.Context(), 7, 100, testSHA); failureCode(err) != FailApp {
		t.Fatalf("missing credentials: %v", err)
	}
}
