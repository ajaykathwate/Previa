package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v91/github"
)

// Client owns App JWT signing and one concurrency-safe cached transport per
// installation. Installation tokens are never persisted or returned to the UI.
type Client struct {
	app           *ghinstallation.AppsTransport
	mu            sync.Mutex
	installations map[int64]*ghinstallation.Transport
	signal        func(context.Context, int64) error
}

func New(cfg config.ServerConfig) (*Client, error) {
	return NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
}

// NewApp requires only JWT credentials, including in non-HTTP workers.
func NewApp(appID int64, privateKeyPath string) (*Client, error) {
	if appID <= 0 {
		return nil, errors.New("GITHUB_APP_ID must be configured before authenticating as the App")
	}
	info, err := os.Stat(privateKeyPath)
	if err != nil {
		return nil, errors.New("GitHub App private key file is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("GitHub App private key must be a regular file with owner-only permissions (chmod 600)")
	}
	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, errors.New("read GitHub App private key failed")
	}
	defer clear(key)
	app, err := ghinstallation.NewAppsTransport(baseTransport, appID, key)
	if err != nil {
		return nil, errors.New("GitHub App private key must be a valid RSA PEM key")
	}
	return &Client{app: app, installations: make(map[int64]*ghinstallation.Transport)}, nil
}

// App returns the authenticated App itself (ID and slug), for example to
// recognise comments the App wrote.
func (c *Client) App(ctx context.Context) (*github.App, error) {
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: c.app, Timeout: 10 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("construct App client: %w", err)
	}
	app, _, err := client.Apps.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("get authenticated App: %w", err)
	}
	return app, nil
}

func (c *Client) Installation(ctx context.Context, id int64) (*github.Installation, error) {
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: c.app, Timeout: 10 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("construct App client: %w", err)
	}
	installation, _, err := client.Apps.GetInstallation(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get App installation: %w", err)
	}
	return installation, nil
}

// InstallationClient reuses ghinstallation's mutex-protected token cache, which
// refreshes before expiry. The token-exchange client is also time-bounded.
func (c *Client) InstallationClient(id int64) (*github.Client, error) {
	if id <= 0 {
		return nil, errors.New("installation ID must be positive")
	}
	c.mu.Lock()
	transport := c.installations[id]
	if transport == nil {
		// Token refresh mutates AppsTransport fields in ghinstallation. Give each
		// installation its own copy rather than sharing those mutable fields.
		app := *c.app
		transport = ghinstallation.NewFromAppsTransport(&app, id)
		transport.Client = &http.Client{Transport: baseTransport, Timeout: 10 * time.Second}
		c.installations[id] = transport
	}
	signal := c.signal
	c.mu.Unlock()
	return github.NewClient(github.WithHTTPClient(&http.Client{Transport: installationTransport{next: transport, signal: signal}, Timeout: 10 * time.Second}))
}

// Invalidate drops cached credentials after access or lifecycle changes. Existing
// in-flight calls still rely on GitHub's revocation; new calls use a fresh cache.
func (c *Client) Invalidate(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.installations, id)
}
