package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// SharedConfig contains the connection used by both application state and River.
// Configuration contains secrets and must not be logged.
type SharedConfig struct {
	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`
}

// ServerConfig contains only settings used by the HTTP server.
type ServerConfig struct {
	SharedConfig
	AppBaseURL              string `envconfig:"APP_BASE_URL" default:"http://localhost:8080"`
	HTTPAddr                string `envconfig:"HTTP_ADDR" default:"127.0.0.1:8080"`
	GitHubOAuthClientID     string `envconfig:"GITHUB_OAUTH_CLIENT_ID" required:"true"`
	GitHubOAuthClientSecret string `envconfig:"GITHUB_OAUTH_CLIENT_SECRET" required:"true"`
	SessionSecret           string `envconfig:"SESSION_SECRET" required:"true"`
}

// WorkerConfig contains only settings used to execute queued jobs.
type WorkerConfig struct {
	SharedConfig
	MaxConcurrentBuilds int           `envconfig:"MAX_CONCURRENT_BUILDS" required:"true"`
	BuildTimeout        time.Duration `envconfig:"BUILD_TIMEOUT" default:"12m"`
}

// LoadShared reads database/queue connection settings, including for migrations.
func LoadShared() (SharedConfig, error) {
	var cfg SharedConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return SharedConfig{}, fmt.Errorf("load shared configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return SharedConfig{}, fmt.Errorf("validate shared configuration: %w", err)
	}
	return cfg, nil
}

// LoadServer reads and validates the server environment; it does not read .env files.
func LoadServer() (ServerConfig, error) {
	var cfg ServerConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return ServerConfig{}, fmt.Errorf("load server configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return ServerConfig{}, fmt.Errorf("validate server configuration: %w", err)
	}
	return cfg, nil
}

// LoadWorker reads only the worker environment; no HTTP or OAuth settings are needed.
func LoadWorker() (WorkerConfig, error) {
	var cfg WorkerConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return WorkerConfig{}, fmt.Errorf("load worker configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate worker configuration: %w", err)
	}
	if cfg.MaxConcurrentBuilds <= 0 {
		return WorkerConfig{}, errors.New("MAX_CONCURRENT_BUILDS must be positive")
	}
	if cfg.BuildTimeout <= 0 || cfg.BuildTimeout > 24*time.Hour {
		return WorkerConfig{}, errors.New("BUILD_TIMEOUT must be greater than zero and at most 24h")
	}
	return cfg, nil
}

func (c SharedConfig) validate() error {
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
	return nil
}

func (c ServerConfig) validate() error {
	if err := c.SharedConfig.validate(); err != nil {
		return err
	}
	base, err := url.Parse(c.AppBaseURL)
	if err != nil || base == nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || (base.Path != "" && base.Path != "/") || (base.Scheme != "http" && base.Scheme != "https") {
		return errors.New("APP_BASE_URL must be an HTTP(S) origin without credentials, query, or path")
	}
	if base.Scheme == "http" && base.Hostname() != "localhost" && base.Hostname() != "127.0.0.1" && base.Hostname() != "::1" {
		return errors.New("APP_BASE_URL must use HTTPS except on localhost")
	}
	if base.Port() != "" {
		port, err := strconv.Atoi(base.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("APP_BASE_URL port must be between 1 and 65535")
		}
	}
	for _, field := range []struct{ name, value string }{
		{"GITHUB_OAUTH_CLIENT_ID", c.GitHubOAuthClientID},
		{"GITHUB_OAUTH_CLIENT_SECRET", c.GitHubOAuthClientSecret},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	if len(strings.TrimSpace(c.SessionSecret)) < 32 {
		return errors.New("SESSION_SECRET must contain at least 32 characters")
	}
	return nil
}
