package repoconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/google/go-github/v91/github"
)

var _ InstallationClients = (*githubapp.Client)(nil)

type installationClientsFunc func(int64) (*github.Client, error)

func (f installationClientsFunc) InstallationClient(id int64) (*github.Client, error) {
	return f(id)
}

func testRequest() Request {
	return Request{InstallationID: 42, Owner: "owner", Repository: "repo", CommitSHA: strings.Repeat("a", 40)}
}

func apiFetcher(t *testing.T, handler http.HandlerFunc) *GitHubFetcher {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL), github.WithAuthToken("fixture-installation-token"))
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := NewGitHubFetcher(installationClientsFunc(func(id int64) (*github.Client, error) {
		if id != testRequest().InstallationID {
			t.Errorf("incorrect installation ID: %d", id)
		}
		return client, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func writeFile(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "size": len(data), "content": base64.StdEncoding.EncodeToString(data)}); err != nil {
		t.Error(err)
	}
}

func TestGitHubFetchExactCommitAndAuthentication(t *testing.T) {
	data := fixture(t, "valid.yaml")
	var calls atomic.Int32
	fetcher := apiFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/repos/owner/repo/contents/.previewrc.yaml" || r.URL.Query().Get("ref") != testRequest().CommitSHA || !strings.Contains(r.Header.Get("Authorization"), "fixture-installation-token") {
			t.Error("incorrect path/ref or installation authentication missing")
		}
		writeFile(t, w, data)
	})
	actual, err := fetcher.Fetch(context.Background(), testRequest())
	if err != nil || string(actual) != string(data) || calls.Load() != 1 {
		t.Fatalf("fetch failed: %v calls=%d", err, calls.Load())
	}
	service, err := NewService(fetcher, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Load(context.Background(), testRequest())
	if err != nil || result.Config.Port != 8080 || len(result.Hash) != 64 {
		t.Fatalf("fetch/parse service failed: %v", err)
	}
}

func TestGitHubFetchErrors(t *testing.T) {
	missing := fixture(t, "missing_file.json")
	for _, tc := range []struct {
		name, code       string
		status, fallback int
		rate, retryable  bool
	}{
		{"missing file", "missing_file", 404, 200, false, false},
		{"masked repository", "resource_unavailable", 404, 404, false, false},
		{"masked access", "access_denied", 404, 403, false, false},
		{"unauthorized", "access_denied", 401, 0, false, false},
		{"forbidden", "access_denied", 403, 0, false, false},
		{"primary rate limit", "rate_limited", 403, 0, true, true},
		{"too many requests", "rate_limited", 429, 0, false, true},
		{"GitHub unavailable", "api_error", 503, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			fetcher := apiFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				status := tc.status
				if strings.Contains(r.URL.Path, "/commits/") {
					if r.URL.Path != "/repos/owner/repo/commits/"+testRequest().CommitSHA {
						t.Error("fallback did not verify exact SHA")
					}
					status = tc.fallback
					if status == 200 {
						if _, err := io.WriteString(w, testRequest().CommitSHA); err != nil {
							t.Error(err)
						}
						return
					}
				}
				if tc.rate {
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", "2000000000")
				}
				w.WriteHeader(status)
				body := []byte(`{"message":"fixture-provider-secret"}`)
				if status == 404 {
					body = missing
				}
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			})
			_, err := fetcher.Fetch(context.Background(), testRequest())
			var fetchErr *FetchError
			if !errors.As(err, &fetchErr) || fetchErr.Code != tc.code || fetchErr.Retryable != tc.retryable || strings.Contains(err.Error(), "fixture-provider-secret") {
				t.Fatalf("unexpected/unsafe fetch error: %v", err)
			}
			if (tc.fallback != 0 && calls.Load() != 2) || (tc.fallback == 0 && calls.Load() != 1) {
				t.Fatal("unexpected API call count")
			}
		})
	}
}

func TestMissingFileVerifiesEquivalentCommitSHA(t *testing.T) {
	request := testRequest()
	request.CommitSHA = strings.ToUpper(request.CommitSHA)
	fetcher := apiFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/") {
			if _, err := io.WriteString(w, strings.ToLower(request.CommitSHA)+"\n"); err != nil {
				t.Error(err)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := fetcher.Fetch(context.Background(), request)
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Code != "missing_file" {
		t.Fatalf("equivalent commit SHA not recognized: %v", err)
	}
}

func TestGitHubResponseBoundsAndFileShape(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		body       string
	}{
		{"oversized response", "too_large", strings.Repeat("x", maxResponseBytes+1)},
		{"oversized declared size", "too_large", `{"type":"file","size":65537,"encoding":"base64","content":""}`},
		{"oversized actual file", "too_large", `{"type":"file","size":1,"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", MaxConfigBytes+1))) + `"}`},
		{"directory", "invalid_file", `[]`},
		{"symlink", "invalid_file", `{"type":"symlink"}`},
		{"bad base64", "invalid_response", `{"type":"file","encoding":"base64","content":"!"}`},
		{"no content", "invalid_response", `{"type":"file","encoding":"none"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := apiFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Error(err)
				}
			})
			_, err := fetcher.Fetch(context.Background(), testRequest())
			var fetchErr *FetchError
			if !errors.As(err, &fetchErr) || fetchErr.Code != tc.code {
				t.Fatalf("expected %s: %v", tc.code, err)
			}
		})
	}
}

func TestCancellationAndRequestValidation(t *testing.T) {
	fetcher := apiFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := fetcher.Fetch(ctx, testRequest())
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Code != "request_cancelled" || !fetchErr.Retryable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not preserved: %v", err)
	}
	ctx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = fetcher.Fetch(ctx, testRequest())
	if !errors.Is(err, context.Canceled) || !errors.As(err, &fetchErr) || fetchErr.Retryable {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.CommitSHA = "main" },
		func(r *Request) { r.CommitSHA = "aabbccdd" },
		func(r *Request) { r.Owner = "../owner" },
		func(r *Request) { r.Repository = "repo?token=value" },
		func(r *Request) { r.InstallationID = 0 },
	} {
		r := testRequest()
		change(&r)
		if _, err := fetcher.Fetch(context.Background(), r); err == nil {
			t.Fatal("accepted invalid request")
		}
	}
}

type fileFetcherFunc func(context.Context, Request) ([]byte, error)

func (f fileFetcherFunc) Fetch(ctx context.Context, r Request) ([]byte, error) { return f(ctx, r) }

func TestServiceValidationAndFailurePropagation(t *testing.T) {
	if _, err := NewService(nil, DefaultLimits()); err == nil {
		t.Fatal("accepted nil fetcher")
	}
	if _, err := NewGitHubFetcher(nil); err == nil {
		t.Fatal("accepted nil authentication")
	}
	service, err := NewService(fileFetcherFunc(func(context.Context, Request) ([]byte, error) {
		return fixture(t, "invalid_port.yaml"), nil
	}), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Load(context.Background(), testRequest())
	assertIssue(t, err, "invalid_port", "port")
	fetchErr := &FetchError{Code: "missing_file", Message: "Add the config file."}
	service, err = NewService(fileFetcherFunc(func(context.Context, Request) ([]byte, error) {
		return nil, fetchErr
	}), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Load(context.Background(), testRequest())
	if !errors.Is(err, fetchErr) {
		t.Fatal("service lost structured fetch error")
	}
	clients := installationClientsFunc(func(int64) (*github.Client, error) {
		return nil, errors.New("fixture-credential")
	})
	fetcher, err := NewGitHubFetcher(clients)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(context.Background(), testRequest())
	if err == nil || strings.Contains(err.Error(), "fixture-credential") {
		t.Fatal("unsafe authentication error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransientNetworkAndSecondaryRateLimit(t *testing.T) {
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("fixture-network-private-detail")
	})}))
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := NewGitHubFetcher(installationClientsFunc(func(int64) (*github.Client, error) { return client, nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(context.Background(), testRequest())
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Code != "api_unavailable" || !fetchErr.Retryable || strings.Contains(err.Error(), "fixture-network-private-detail") {
		t.Fatalf("unsafe/wrong network classification: %v", err)
	}
	fetcher = apiFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, `{"message":"fixture-private-detail","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#secondary-rate-limits"}`); err != nil {
			t.Error(err)
		}
	})
	_, err = fetcher.Fetch(context.Background(), testRequest())
	if !errors.As(err, &fetchErr) || fetchErr.Code != "rate_limited" || !fetchErr.Retryable {
		t.Fatalf("secondary rate limit not retryable: %v", err)
	}
}

type trackedBody struct {
	*strings.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestResponseBoundStopsAndClosesBody(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", maxResponseBytes*4))}
	transport := responseBound{transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body}, nil
	})}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if !errors.Is(err, errResponseTooLarge) || len(data) != maxResponseBytes+1 || body.Len() != maxResponseBytes*4-(maxResponseBytes+1) {
		t.Fatal("response read was not bounded")
	}
	if err := response.Body.Close(); err != nil || !body.closed {
		t.Fatal("original body not closed")
	}
}
