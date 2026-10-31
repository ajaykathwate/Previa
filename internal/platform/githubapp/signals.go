package githubapp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// WithInstallationSignals configures a durable suspicion callback before use.
// It observes token-mint errors only, never normal repository 403/404 responses.
func (c *Client) WithInstallationSignals(signal func(context.Context, int64) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signal = signal
}

type installationTransport struct {
	next   http.RoundTripper
	signal func(context.Context, int64) error
}

func (t installationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(r)
	var tokenError *ghinstallation.HTTPError
	if t.signal != nil && errors.As(err, &tokenError) && tokenError.Response != nil && (tokenError.Response.StatusCode == 403 || tokenError.Response.StatusCode == 404) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		if signalErr := t.signal(ctx, tokenError.InstallationID); signalErr != nil {
			err = errors.Join(err, signalErr)
		}
	}
	return response, err
}
