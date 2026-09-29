package config

import (
	"os"
	"strings"
	"testing"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASE_URL":                "postgres://previa:password@localhost:5432/previa?sslmode=disable",
		"GITHUB_APP_ID":               "123456",
		"GITHUB_APP_PRIVATE_KEY_PATH": "/path/to/app.pem",
		"GITHUB_WEBHOOK_SECRET":       "webhook-secret",
		"GITHUB_OAUTH_CLIENT_ID":      "client-id",
		"GITHUB_OAUTH_CLIENT_SECRET":  "client-secret",
		"PREVIEW_BASE_DOMAIN":         "preview.example.com",
		"SESSION_SECRET":              strings.Repeat("s", 32),
		"SECRETS_ENCRYPTION_KEY":      "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"MAX_CONCURRENT_BUILDS":       "2",
		"ENV_TTL_HOURS":               "24",
		"DNS_PROVIDER_API_TOKEN":      "dns-token",
	} {
		t.Setenv(key, value)
	}
}

func TestLoad(t *testing.T) {
	validEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubAppID != 123456 || cfg.MaxConcurrentBuilds != 2 || cfg.EnvTTLHours != 24 {
		t.Fatal("numeric configuration was not loaded correctly")
	}
	if cfg.DatabaseURL != os.Getenv("DATABASE_URL") || cfg.GitHubAppPrivateKeyPath != "/path/to/app.pem" || cfg.PreviewBaseDomain != "preview.example.com" || cfg.GitHubWebhookSecret != "webhook-secret" || cfg.GitHubOAuthClientID != "client-id" || cfg.GitHubOAuthClientSecret != "client-secret" || cfg.SessionSecret != strings.Repeat("s", 32) || cfg.SecretsEncryptionKey != os.Getenv("SECRETS_ENCRYPTION_KEY") || cfg.DNSProviderAPIToken != "dns-token" {
		t.Fatal("string configuration was not loaded correctly")
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"DATABASE_URL", "https://localhost/previa"},
		{"DATABASE_URL", "postgres://localhost"},
		{"DATABASE_URL", "postgres:///previa"},
		{"DATABASE_URL", "postgres://localhost:70000/previa"},
		{"DATABASE_URL", "postgres://localhost:0/previa"},
		{"DATABASE_URL", "postgres://localhost/previa#fragment"},
		{"GITHUB_APP_ID", "0"},
		{"GITHUB_APP_ID", "-1"},
		{"GITHUB_APP_ID", "not-a-number"},
		{"GITHUB_APP_PRIVATE_KEY_PATH", " "},
		{"GITHUB_WEBHOOK_SECRET", ""},
		{"GITHUB_OAUTH_CLIENT_ID", ""},
		{"GITHUB_OAUTH_CLIENT_SECRET", ""},
		{"PREVIEW_BASE_DOMAIN", "https://preview.example.com"},
		{"PREVIEW_BASE_DOMAIN", "*.example.com"},
		{"PREVIEW_BASE_DOMAIN", "example.com:443"},
		{"PREVIEW_BASE_DOMAIN", "-preview.example.com"},
		{"PREVIEW_BASE_DOMAIN", "preview..com"},
		{"PREVIEW_BASE_DOMAIN", "localhost"},
		{"PREVIEW_BASE_DOMAIN", strings.Repeat("a", 64) + ".com"},
		{"SESSION_SECRET", "too-short"},
		{"SECRETS_ENCRYPTION_KEY", "not-base64!"},
		{"SECRETS_ENCRYPTION_KEY", "c2hvcnQ="},
		{"MAX_CONCURRENT_BUILDS", "0"},
		{"MAX_CONCURRENT_BUILDS", "-2"},
		{"MAX_CONCURRENT_BUILDS", "1.5"},
		{"ENV_TTL_HOURS", "0"},
		{"ENV_TTL_HOURS", "-1"},
		{"ENV_TTL_HOURS", "99999999999999999999999999"},
		{"DNS_PROVIDER_API_TOKEN", " "},
	} {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.key, tt.value)
			cfg, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("expected error mentioning %s, got %v", tt.key, err)
			}
			if cfg != (Config{}) {
				t.Fatal("failed load returned partial configuration")
			}
		})
	}
}

func TestLoadRequiresEveryVariable(t *testing.T) {
	for _, key := range []string{
		"DATABASE_URL", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_PATH",
		"GITHUB_WEBHOOK_SECRET", "GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET",
		"PREVIEW_BASE_DOMAIN", "SESSION_SECRET", "SECRETS_ENCRYPTION_KEY",
		"MAX_CONCURRENT_BUILDS", "ENV_TTL_HOURS", "DNS_PROVIDER_API_TOKEN",
	} {
		t.Run(key, func(t *testing.T) {
			validEnvironment(t)
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected missing %s error, got %v", key, err)
			}
		})
	}
}

func TestLoadDoesNotExposeDatabaseCredentials(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:private-password@localhost:invalid/previa")
	_, err := Load()
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("invalid database configuration must fail without exposing credentials")
	}
}
