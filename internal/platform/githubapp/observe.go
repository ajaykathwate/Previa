package githubapp

import (
	"net/http"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/platform/metrics"
)

// observed records each GitHub API response's status and rate-limit headroom
// (X-RateLimit-Remaining per X-RateLimit-Resource). It reads only headers.
type observed struct{ next http.RoundTripper }

// baseTransport is the base for every App and installation request.
var baseTransport http.RoundTripper = observed{next: http.DefaultTransport}

func (o observed) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(r)
	if err != nil {
		metrics.GitHubError()
		return resp, err
	}
	remaining, convErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	resource := resp.Header.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = "core"
	}
	metrics.GitHubResponse(resp.StatusCode, resource, remaining, convErr == nil)
	return resp, nil
}
