package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

// Config holds the application configuration loaded from the process environment.
// It contains secrets and must not be logged.
type Config struct {
	DatabaseURL             string `envconfig:"DATABASE_URL" required:"true"`
	GitHubAppID             int64  `envconfig:"GITHUB_APP_ID" required:"true"`
	GitHubAppPrivateKeyPath string `envconfig:"GITHUB_APP_PRIVATE_KEY_PATH" required:"true"`
	GitHubWebhookSecret     string `envconfig:"GITHUB_WEBHOOK_SECRET" required:"true"`
	GitHubOAuthClientID     string `envconfig:"GITHUB_OAUTH_CLIENT_ID" required:"true"`
	GitHubOAuthClientSecret string `envconfig:"GITHUB_OAUTH_CLIENT_SECRET" required:"true"`
	PreviewBaseDomain       string `envconfig:"PREVIEW_BASE_DOMAIN" required:"true"`
	SessionSecret           string `envconfig:"SESSION_SECRET" required:"true"`
	SecretsEncryptionKey    string `envconfig:"SECRETS_ENCRYPTION_KEY" required:"true"`
	MaxConcurrentBuilds     int    `envconfig:"MAX_CONCURRENT_BUILDS" required:"true"`
	EnvTTLHours             int    `envconfig:"ENV_TTL_HOURS" required:"true"`
	DNSProviderAPIToken     string `envconfig:"DNS_PROVIDER_API_TOKEN" required:"true"`
}

// Load reads and validates environment variables. It does not read .env files.
func Load() (Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return Config{}, fmt.Errorf("load configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("validate configuration: %w", err)
	}
	return cfg, nil
}

var domainLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func (c Config) validate() error {
	for _, field := range []struct{ name, value string }{
		{"GITHUB_APP_PRIVATE_KEY_PATH", c.GitHubAppPrivateKeyPath},
		{"GITHUB_WEBHOOK_SECRET", c.GitHubWebhookSecret},
		{"GITHUB_OAUTH_CLIENT_ID", c.GitHubOAuthClientID},
		{"GITHUB_OAUTH_CLIENT_SECRET", c.GitHubOAuthClientSecret},
		{"DNS_PROVIDER_API_TOKEN", c.DNSProviderAPIToken},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}

	// Report validation failures without echoing the URL or its credentials.
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || strings.Trim(u.Path, "/") == "" || u.Fragment != "" {
		return errors.New("DATABASE_URL must be a PostgreSQL URL with a host and database name")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("DATABASE_URL port must be between 1 and 65535")
		}
	}
	if c.GitHubAppID <= 0 {
		return errors.New("GITHUB_APP_ID must be positive")
	}
	labels := strings.Split(c.PreviewBaseDomain, ".")
	if len(c.PreviewBaseDomain) > 253 || len(labels) < 2 {
		return errors.New("PREVIEW_BASE_DOMAIN must be a DNS domain without a scheme, port, or wildcard")
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return errors.New("PREVIEW_BASE_DOMAIN contains an invalid DNS label")
		}
	}
	if len(strings.TrimSpace(c.SessionSecret)) < 32 {
		return errors.New("SESSION_SECRET must contain at least 32 characters")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(c.SecretsEncryptionKey)
	if err != nil || len(key) != 32 {
		return errors.New("SECRETS_ENCRYPTION_KEY must be base64-encoded 32-byte key material")
	}
	if c.MaxConcurrentBuilds <= 0 {
		return errors.New("MAX_CONCURRENT_BUILDS must be positive")
	}
	if c.EnvTTLHours <= 0 {
		return errors.New("ENV_TTL_HOURS must be positive")
	}
	return nil
}
