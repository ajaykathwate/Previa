package pullrequests

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
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
			if q.Get("page") != "2" || q.Get("per_page") != "100" || q.Get("state") != "open" || q.Get("sort") != "created" || q.Get("direction") != "asc" {
				t.Error("unbounded or incorrect query")
			}
			w.Header().Set("Link", `<http://example.test/pulls?page=3>; rel="next"`)
			_, err := w.Write([]byte(`[{"number":42,"title":"Real metadata","state":"open","mergeable":true,"mergeable_state":"clean","updated_at":"2026-10-03T10:00:00Z","head":{"sha":"abcdef","ref":"topic","repo":{"id":101}},"user":{"login":"author"}}]`))
			if err != nil {
				t.Error(err)
			}
		case "/repos/transferred/renamed/pulls/42":
			_, err := w.Write([]byte(`{"number":42,"state":"open","mergeable":false,"mergeable_state":"dirty","updated_at":"2026-10-03T10:00:00Z","head":{"sha":"abcdef","repo":{"id":101}}}`))
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
	resolved, err := source.Get(t.Context(), 99, 100, 42)
	if err != nil || resolved.BuildStatus != "conflicts" || resolved.Mergeable == nil || *resolved.Mergeable || calls != 4 {
		t.Fatal("targeted mergeability GET lost signal", resolved, err)
	}
	if rows[0].State != "open" || rows[0].BuildStatus != "open" || !rows[0].Fork || rows[0].HeadRepoID != 101 || rows[0].Author != "author" {
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

func TestMergeabilityMappingNeverAssumesNullIsClean(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name        string
		mergeable   *bool
		state, want string
	}{
		{"null", nil, "", "checking"}, {"unknown", nil, "unknown", "checking"},
		{"blocked pending", nil, "blocked", "checking"}, {"unstable pending", nil, "unstable", "checking"},
		{"dirty", nil, "dirty", "conflicts"}, {"false", &no, "blocked", "conflicts"},
		{"true", &yes, "blocked", "open"}, {"clean", nil, "clean", "open"},
		{"contradictory dirty", &yes, "dirty", "conflicts"}, {"contradictory false", &no, "clean", "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeabilityStatus(tc.mergeable, tc.state); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestInspectClassifiesUpstreamFailures(t *testing.T) {
	status := http.StatusOK
	rateLimited := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if rateLimited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		switch r.URL.Path {
		case "/repositories/100":
			_, _ = w.Write([]byte(`{"id":100,"name":"renamed","owner":{"login":"transferred"}}`))
		case "/repos/transferred/renamed/pulls/7":
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"fixture failure with secret-ish detail"}`))
				return
			}
			_, _ = w.Write([]byte(`{"number":7,"state":"open","mergeable":true,"mergeable_state":"clean","updated_at":"2026-10-03T10:00:00Z","head":{"sha":"abc123","repo":{"id":101}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	source := NewGitHubSource(clientsFunc(func(int64) (*github.Client, error) { return client, nil }))

	up, err := source.Inspect(t.Context(), 99, 100, 7)
	if err != nil {
		t.Fatal(err)
	}
	if up.Owner != "transferred" || up.Repository != "renamed" || up.SHA != "abc123" || !up.Fork || up.HeadRepoID != 101 || up.BuildStatus != "open" {
		t.Fatalf("unexpected upstream: %+v", up)
	}
	for code, want := range map[int]error{
		http.StatusNotFound:            environments.ErrUpstreamInaccessible,
		http.StatusForbidden:           environments.ErrUpstreamInaccessible,
		http.StatusInternalServerError: environments.ErrUpstreamUnavailable,
		http.StatusBadGateway:          environments.ErrUpstreamUnavailable,
	} {
		status = code
		_, err := source.Inspect(t.Context(), 99, 100, 7)
		if !errors.Is(err, want) || strings.Contains(err.Error(), "secret-ish") {
			t.Fatalf("HTTP %d: error %v; want %v without response detail", code, err, want)
		}
	}
	status, rateLimited = http.StatusOK, true
	_, err = source.Inspect(t.Context(), 99, 100, 7)
	var retry *environments.UpstreamRetryAfter
	if !errors.As(err, &retry) || retry.Delay <= 0 || !errors.Is(err, environments.ErrUpstreamUnavailable) {
		t.Fatalf("rate limit: %v", err)
	}
	if _, err := source.Inspect(t.Context(), 0, 100, 7); !errors.Is(err, environments.ErrUpstreamInaccessible) {
		t.Fatal("invalid identity must fail closed")
	}
}
