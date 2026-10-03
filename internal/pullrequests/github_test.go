package pullrequests

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v91/github"
)

type clientsFunc func(int64) (*github.Client, error)

func (f clientsFunc) InstallationClient(id int64) (*github.Client, error) { return f(id) }
func TestGitHubDiscoveryUsesNumericIdentityAndBoundedAuthenticatedPages(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.Header.Get("Authorization"), "fixture-token") {
			t.Error("missing installation authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repositories/100":
			_, err := w.Write([]byte(`{"id":100,"name":"renamed","owner":{"login":"transferred"}}`))
			if err != nil {
				t.Error(err)
			}
		case "/repos/transferred/renamed/pulls":
			q := r.URL.Query()
			if q.Get("page") != "2" || q.Get("per_page") != "100" || q.Get("state") != "all" || q.Get("sort") != "created" || q.Get("direction") != "asc" {
				t.Error("unbounded or incorrect query")
			}
			w.Header().Set("Link", `<http://example.test/pulls?page=3>; rel="next"`)
			_, err := w.Write([]byte(`[{"number":42,"title":"Real metadata","state":"closed","merged_at":"2026-10-03T10:00:00Z","updated_at":"2026-10-03T10:00:00Z","head":{"sha":"abcdef","ref":"topic","repo":{"id":101}},"user":{"login":"author"}}]`))
			if err != nil {
				t.Error(err)
			}
		default:
			t.Error("mutable repository name used as identity", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&base, &base), github.WithAuthToken("fixture-token"))
	if err != nil {
		t.Fatal(err)
	}
	source := NewGitHubSource(clientsFunc(func(id int64) (*github.Client, error) {
		if id != 99 {
			t.Error("wrong installation")
		}
		return client, nil
	}))
	rows, next, err := source.Page(t.Context(), 99, 100, 2)
	if err != nil || len(rows) != 1 || next != 3 || calls != 2 {
		t.Fatalf("API pagination: %+v next=%d calls=%d %v", rows, next, calls, err)
	}
	if rows[0].State != "merged" || !rows[0].Fork || rows[0].HeadRepoID != 101 || rows[0].Author != "author" {
		t.Fatal("metadata/fork/merge mapping", rows[0])
	}
}
func TestDiscoveryRateLimitsAndSafeErrors(t *testing.T) {
	reset := time.Now().Add(30 * time.Second)
	delay := 45 * time.Second
	for _, e := range []error{&github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: reset}}}, &github.AbuseRateLimitError{RetryAfter: &delay}} {
		var retry *RetryAfterError
		if err := sourceError(e); !errors.As(err, &retry) || retry.Delay <= 0 || retry.Delay > time.Minute {
			t.Fatal("rate limit not honored", err)
		}
	}
	if err := sourceError(errors.New("private URL/token/provider body")); strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe provider error surfaced")
	}
	if _, err := FromGitHub(nil, 100); err == nil {
		t.Fatal("invalid PR accepted")
	}
}

var _ Source = (*GitHubSource)(nil)
