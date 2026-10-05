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
	// ApplicationName labels this process's connections (set by the caller).
	ApplicationName string `ignored:"true"`
}

// SecretsConfig is the repository-secret keyring: versioned AES-256 keys and the
// version used for new values. Key material is a secret; never log it. The
// secrets feature parses and validates the keys themselves.
type SecretsConfig struct {
	SecretsEncryptionKeys   string `envconfig:"SECRETS_ENCRYPTION_KEYS"`
	SecretsActiveKeyVersion string `envconfig:"SECRETS_ACTIVE_KEY_VERSION"`
}

// SecretsConfigured reports whether a keyring was provided.
func (c SecretsConfig) SecretsConfigured() bool {
	return strings.TrimSpace(c.SecretsEncryptionKeys) != "" || strings.TrimSpace(c.SecretsActiveKeyVersion) != ""
}

func (c SecretsConfig) validate() error {
	if (strings.TrimSpace(c.SecretsEncryptionKeys) == "") != (strings.TrimSpace(c.SecretsActiveKeyVersion) == "") {
		return errors.New("SECRETS_ENCRYPTION_KEYS and SECRETS_ACTIVE_KEY_VERSION must be configured together")
	}
	return nil
}

// SecretsMaintenanceConfig is used by the re-encryption command.
type SecretsMaintenanceConfig struct {
	SharedConfig
	SecretsConfig
}

// LoadSecretsMaintenance requires the database and a complete keyring.
func LoadSecretsMaintenance() (SecretsMaintenanceConfig, error) {
	var cfg SecretsMaintenanceConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return SecretsMaintenanceConfig{}, fmt.Errorf("load secrets maintenance configuration: %w", err)
	}
	if err := cfg.SharedConfig.validate(); err != nil {
		return SecretsMaintenanceConfig{}, fmt.Errorf("validate secrets maintenance configuration: %w", err)
	}
	if !cfg.SecretsConfigured() {
		return SecretsMaintenanceConfig{}, errors.New("SECRETS_ENCRYPTION_KEYS and SECRETS_ACTIVE_KEY_VERSION are required for re-encryption")
	}
	if err := cfg.SecretsConfig.validate(); err != nil {
		return SecretsMaintenanceConfig{}, err
	}
	return cfg, nil
}

// ServerConfig contains only settings used by the HTTP server.
type ServerConfig struct {
	SharedConfig
	SecretsConfig
	EnvironmentConfig
	// InternalAddr serves /livez, /readyz and /metrics; never publish it.
	InternalAddr string `envconfig:"INTERNAL_ADDR" default:"127.0.0.1:9090"`
	// TrustedProxies may set X-Forwarded-For/-Host (the router, in production).
	TrustedProxies string `envconfig:"TRUSTED_PROXIES"`
	// BackupStatusFile is written by the backup script after each successful
	// backup; its modification time becomes a metric.
	BackupStatusFile        string `envconfig:"BACKUP_STATUS_FILE"`
	AppBaseURL              string `envconfig:"APP_BASE_URL" default:"http://localhost:8080"`
	HTTPAddr                string `envconfig:"HTTP_ADDR" default:"127.0.0.1:8080"`
	GitHubOAuthClientID     string `envconfig:"GITHUB_OAUTH_CLIENT_ID" required:"true"`
	GitHubOAuthClientSecret string `envconfig:"GITHUB_OAUTH_CLIENT_SECRET" required:"true"`
	GitHubWebhookSecret     string `envconfig:"GITHUB_WEBHOOK_SECRET"`
	SessionSecret           string `envconfig:"SESSION_SECRET" required:"true"`
	GitHubAppID             int64  `envconfig:"GITHUB_APP_ID"`
	GitHubAppSlug           string `envconfig:"GITHUB_APP_SLUG"`
	GitHubAppPrivateKeyPath string `envconfig:"GITHUB_APP_PRIVATE_KEY_PATH"`
	GitHubAppClientID       string `envconfig:"GITHUB_APP_CLIENT_ID"`
	GitHubAppClientSecret   string `envconfig:"GITHUB_APP_CLIENT_SECRET"`
	// PreviewBaseDomain, when set, must keep previews off the dashboard's host.
	PreviewBaseDomain string `envconfig:"PREVIEW_BASE_DOMAIN"`
}

// WorkerConfig contains only settings used to execute queued jobs.
type WorkerConfig struct {
	SharedConfig
	SecretsConfig
	EnvironmentConfig
	ExecutionConfig
	RoutingConfig
	HousekeepingConfig
	GitHubAppID             int64         `envconfig:"GITHUB_APP_ID"`
	GitHubAppPrivateKeyPath string        `envconfig:"GITHUB_APP_PRIVATE_KEY_PATH"`
	MaxConcurrentBuilds     int           `envconfig:"MAX_CONCURRENT_BUILDS" required:"true"`
	BuildTimeout            time.Duration `envconfig:"BUILD_TIMEOUT" default:"12m"`
	// InternalAddr serves /livez, /readyz and /metrics; never publish it.
	InternalAddr string `envconfig:"WORKER_INTERNAL_ADDR" default:"127.0.0.1:9091"`
	// BuildsPaused is the execution kill switch: no new build starts, while
	// teardown, cleanup, routing, reports and housekeeping keep running.
	BuildsPaused bool `envconfig:"BUILDS_PAUSED"`
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
	if err := cfg.SharedConfig.validate(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate worker configuration: %w", err)
	}
	if err := cfg.EnvironmentConfig.check(); err != nil {
		return WorkerConfig{}, err
	}
	if err := checkListenAddress("WORKER_INTERNAL_ADDR", cfg.InternalAddr); err != nil {
		return WorkerConfig{}, err
	}
	if cfg.GitHubAppID != 0 || cfg.GitHubAppPrivateKeyPath != "" {
		if cfg.GitHubAppID <= 0 || strings.TrimSpace(cfg.GitHubAppPrivateKeyPath) == "" {
			return WorkerConfig{}, errors.New("GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH must be configured together for PR discovery")
		}
		if cfg.Production() {
			if err := CheckPrivateFile("GITHUB_APP_PRIVATE_KEY_PATH", cfg.GitHubAppPrivateKeyPath); err != nil {
				return WorkerConfig{}, err
			}
		}
	}
	if err := cfg.SecretsConfig.validate(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate worker configuration: %w", err)
	}
	if cfg.MaxConcurrentBuilds <= 0 {
		return WorkerConfig{}, errors.New("MAX_CONCURRENT_BUILDS must be positive")
	}
	if cfg.BuildTimeout <= 0 || cfg.BuildTimeout > 24*time.Hour {
		return WorkerConfig{}, errors.New("BUILD_TIMEOUT must be greater than zero and at most 24h")
	}
	if err := cfg.ExecutionConfig.check(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate execution configuration: %w", err)
	}
	if err := cfg.HousekeepingConfig.check(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate housekeeping configuration: %w", err)
	}
	if err := cfg.RoutingConfig.check(); err != nil {
		return WorkerConfig{}, fmt.Errorf("validate routing configuration: %w", err)
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
	if err := c.check(); err != nil {
		return err
	}
	if err := checkListenAddress("INTERNAL_ADDR", c.InternalAddr); err != nil {
		return err
	}
	if _, err := ParseTrustedProxies(c.TrustedProxies); err != nil {
		return err
	}
	base, err := url.Parse(c.AppBaseURL)
	if err != nil || base == nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || (base.Path != "" && base.Path != "/") || (base.Scheme != "http" && base.Scheme != "https") {
		return errors.New("APP_BASE_URL must be an HTTP(S) origin without credentials, query, or path")
	}
	if base.Scheme == "http" && base.Hostname() != "localhost" && base.Hostname() != "127.0.0.1" && base.Hostname() != "::1" {
		return errors.New("APP_BASE_URL must use HTTPS except on localhost")
	}
	if c.Production() {
		if base.Scheme != "https" {
			return errors.New("APP_BASE_URL must use HTTPS when APP_ENV=production")
		}
		if strings.TrimSpace(c.GitHubWebhookSecret) == "" {
			return errors.New("GITHUB_WEBHOOK_SECRET is required when APP_ENV=production")
		}
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
	if err := c.SecretsConfig.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.PreviewBaseDomain) != "" {
		if err := CheckPreviewDomain(c.PreviewBaseDomain, c.AppBaseURL); err != nil {
			return err
		}
	}
	if c.GitHubAppID != 0 || c.GitHubAppSlug != "" || c.GitHubAppPrivateKeyPath != "" || c.GitHubAppClientID != "" || c.GitHubAppClientSecret != "" {
		if c.GitHubAppID <= 0 {
			return errors.New("GITHUB_APP_ID must be positive when installation linking is configured")
		}
		for _, field := range []struct{ name, value string }{
			{"GITHUB_APP_SLUG", c.GitHubAppSlug}, {"GITHUB_APP_PRIVATE_KEY_PATH", c.GitHubAppPrivateKeyPath},
			{"GITHUB_APP_CLIENT_ID", c.GitHubAppClientID}, {"GITHUB_APP_CLIENT_SECRET", c.GitHubAppClientSecret},
		} {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("%s is required when installation linking is configured", field.name)
			}
		}
		for _, char := range c.GitHubAppSlug {
			valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-'
			if !valid {
				return errors.New("GITHUB_APP_SLUG must contain lowercase letters, digits or hyphens")
			}
		}
		if c.Production() {
			if err := CheckPrivateFile("GITHUB_APP_PRIVATE_KEY_PATH", c.GitHubAppPrivateKeyPath); err != nil {
				return err
			}
		}
	}
	return nil
}
