package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"APP_BASE_URL":               "http://localhost:8080",
		"HTTP_ADDR":                  "127.0.0.1:8080",
		"DATABASE_URL":               "postgres://previa:password@localhost:5432/previa?sslmode=disable",
		"GITHUB_OAUTH_CLIENT_ID":     "client-id",
		"GITHUB_OAUTH_CLIENT_SECRET": "client-secret",
		"SESSION_SECRET":             strings.Repeat("s", 32),
		"GITHUB_APP_ID":              "0", "GITHUB_APP_SLUG": "", "GITHUB_APP_PRIVATE_KEY_PATH": "", "GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_CLIENT_SECRET": "",
		"MAX_CONCURRENT_BUILDS": "2",
		"BUILD_TIMEOUT":         "12m",
	} {
		t.Setenv(key, value)
	}
	// Routing is opt-in; a developer's own settings must not leak into tests.
	unsetEnvironment(t, "PREVIEW_BASE_DOMAIN", "ROUTER_DYNAMIC_DIR")
}

func unsetEnvironment(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "") // Restore the original environment at cleanup, even if unset here.
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadServer(t *testing.T) {
	validEnvironment(t)
	unsetEnvironment(t, "MAX_CONCURRENT_BUILDS", "BUILD_TIMEOUT", "APP_BASE_URL", "HTTP_ADDR")
	cfg, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != os.Getenv("DATABASE_URL") || cfg.GitHubOAuthClientID != "client-id" || cfg.GitHubOAuthClientSecret != "client-secret" || cfg.SessionSecret != strings.Repeat("s", 32) || cfg.AppBaseURL != "http://localhost:8080" || cfg.HTTPAddr != "127.0.0.1:8080" {
		t.Fatal("server configuration/defaults were not loaded correctly")
	}
	// The server must also ignore invalid settings owned by the worker.
	t.Setenv("MAX_CONCURRENT_BUILDS", "not-a-number")
	t.Setenv("BUILD_TIMEOUT", "invalid")
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWorkerWithoutServerOrFutureSettings(t *testing.T) {
	validEnvironment(t)
	// APP_BASE_URL matters to the worker only once PREVIEW_BASE_DOMAIN configures routing.
	unrelated := []string{"APP_BASE_URL", "HTTP_ADDR", "GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET", "SESSION_SECRET", "GITHUB_WEBHOOK_SECRET", "SECRETS_ENCRYPTION_KEY", "DNS_PROVIDER_API_TOKEN"}
	unsetEnvironment(t, unrelated...)
	unsetEnvironment(t, "BUILD_TIMEOUT", "ENV_TTL_HOURS", "HOUSEKEEPING_DRY_RUN", "HOUSEKEEPING_GRACE", "BUILD_CACHE_BUDGET")
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != os.Getenv("DATABASE_URL") || cfg.MaxConcurrentBuilds != 2 || cfg.BuildTimeout != 12*time.Minute {
		t.Fatal("worker configuration not loaded")
	}
	if cfg.PreviewLifetime() != 48*time.Hour || cfg.HousekeepingDryRun || cfg.HousekeepingGrace != 15*time.Minute || cfg.BuildCacheBudget != 40<<30 {
		t.Fatalf("housekeeping defaults: %+v", cfg.HousekeepingConfig)
	}
	for _, key := range unrelated {
		t.Setenv(key, "invalid")
	}
	if _, err := LoadWorker(); err != nil {
		t.Fatalf("worker read unrelated setting: %v", err)
	}
}

func TestLoadServerIgnoresFutureSettings(t *testing.T) {
	validEnvironment(t)
	for _, key := range []string{"GITHUB_WEBHOOK_SECRET", "SECRETS_ENCRYPTION_KEY", "ENV_TTL_HOURS", "DNS_PROVIDER_API_TOKEN", "ROUTER_DYNAMIC_DIR", "ACME_EMAIL"} {
		t.Setenv(key, "invalid")
	}
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadServerRejectsInvalidEnvironment(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"APP_BASE_URL", "http://example.com"},
		{"APP_BASE_URL", "https://example.com/path"},
		{"APP_BASE_URL", "https://user:pass@example.com"},
		{"APP_BASE_URL", "http://localhost:0"},
		{"APP_BASE_URL", "https://example.com?query=value"},
		{"APP_BASE_URL", "https://example.com?"},
		{"DATABASE_URL", "https://localhost/previa"},
		{"DATABASE_URL", "postgres://localhost"},
		{"DATABASE_URL", "postgres:///previa"},
		{"DATABASE_URL", "postgres://localhost:70000/previa"},
		{"DATABASE_URL", "postgres://localhost:0/previa"},
		{"DATABASE_URL", "postgres://localhost/previa#fragment"},
		{"GITHUB_OAUTH_CLIENT_ID", " "},
		{"GITHUB_OAUTH_CLIENT_SECRET", " "},
		{"SESSION_SECRET", "too-short"},
	} {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.key, tt.value)
			cfg, err := LoadServer()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("expected error mentioning %s, got %v", tt.key, err)
			}
			if cfg != (ServerConfig{}) {
				t.Fatal("failed load returned partial configuration")
			}
		})
	}
}

func TestLoadWorkerRejectsInvalidEnvironment(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"DATABASE_URL", "postgres:///previa"},
		{"DATABASE_URL", "postgres://localhost:70000/previa"},
		{"MAX_CONCURRENT_BUILDS", "0"},
		{"MAX_CONCURRENT_BUILDS", "-1"},
		{"MAX_CONCURRENT_BUILDS", "1.5"},
		{"BUILD_TIMEOUT", "0s"},
		{"BUILD_TIMEOUT", "-1m"},
		{"BUILD_TIMEOUT", "25h"},
		{"BUILD_TIMEOUT", "invalid"},
		{"ENV_TTL_HOURS", "-1"},
		{"ENV_TTL_HOURS", "1.5"},
		{"HOUSEKEEPING_GRACE", "10s"},
		{"BUILD_CACHE_BUDGET", "100MiB"},
	} {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.key, tt.value)
			cfg, err := LoadWorker()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("expected %s error, got %v", tt.key, err)
			}
			if cfg != (WorkerConfig{}) {
				t.Fatal("failed load returned partial configuration")
			}
		})
	}
}

func TestRequiredSettingsByProcess(t *testing.T) {
	for _, key := range []string{"DATABASE_URL", "GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET", "SESSION_SECRET"} {
		t.Run("server/"+key, func(t *testing.T) {
			validEnvironment(t)
			unsetEnvironment(t, key)
			if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected missing %s error, got %v", key, err)
			}
		})
	}
	for _, key := range []string{"DATABASE_URL", "MAX_CONCURRENT_BUILDS"} {
		t.Run("worker/"+key, func(t *testing.T) {
			validEnvironment(t)
			unsetEnvironment(t, key)
			if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected missing %s error, got %v", key, err)
			}
		})
	}
}

func TestLoadSharedNeedsOnlyDatabase(t *testing.T) {
	validEnvironment(t)
	unsetEnvironment(t, "GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET", "SESSION_SECRET", "MAX_CONCURRENT_BUILDS")
	cfg, err := LoadShared()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != os.Getenv("DATABASE_URL") {
		t.Fatal("database configuration not loaded")
	}
}

func TestLoadDoesNotExposeDatabaseCredentials(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:private-password@localhost:invalid/previa")
	_, serverErr := LoadServer()
	_, workerErr := LoadWorker()
	shared, sharedErr := LoadShared()
	for _, err := range []error{serverErr, workerErr, sharedErr} {
		if err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("invalid database configuration must fail without exposing credentials")
		}
	}
	if shared != (SharedConfig{}) {
		t.Fatal("failed shared load returned partial configuration")
	}
}

func TestCustomBuildTimeout(t *testing.T) {
	validEnvironment(t)
	t.Setenv("BUILD_TIMEOUT", "15m")
	cfg, err := LoadWorker()
	if err != nil || cfg.BuildTimeout != 15*time.Minute {
		t.Fatalf("custom build timeout not loaded: %v", err)
	}
}

func TestGitHubAppConfigurationByProcess(t *testing.T) {
	validEnvironment(t)
	t.Setenv("GITHUB_APP_ID", "123")
	if _, err := LoadServer(); err == nil {
		t.Fatal("partial App settings accepted")
	}
	for key, value := range map[string]string{"GITHUB_APP_SLUG": "test-app", "GITHUB_APP_PRIVATE_KEY_PATH": "/private/app.pem", "GITHUB_APP_CLIENT_ID": "app-client", "GITHUB_APP_CLIENT_SECRET": "app-secret"} {
		t.Setenv(key, value)
	}
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_SLUG", "../other-app")
	if _, err := LoadServer(); err == nil {
		t.Fatal("unsafe App slug accepted")
	}
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "")
	if _, err := LoadWorker(); err != nil {
		t.Fatal("worker required HTTP App settings")
	}
}

func TestSecretsKeyringConfigurationByProcess(t *testing.T) {
	validEnvironment(t)
	unsetEnvironment(t, "SECRETS_ENCRYPTION_KEYS", "SECRETS_ACTIVE_KEY_VERSION")
	cfg, err := LoadServer()
	if err != nil || cfg.SecretsConfigured() {
		t.Fatalf("secrets are optional for the server: %v", err)
	}
	t.Setenv("SECRETS_ENCRYPTION_KEYS", "1:bm90LWEtcmVhbC1rZXk=")
	if _, err := LoadServer(); err == nil || strings.Contains(err.Error(), "bm90") {
		t.Fatalf("a keyring without an active version must fail without echoing keys: %v", err)
	}
	t.Setenv("SECRETS_ACTIVE_KEY_VERSION", "1")
	if cfg, err := LoadServer(); err != nil || !cfg.SecretsConfigured() {
		t.Fatalf("complete keyring: %v", err)
	}
	// The worker decrypts secrets for previews, so it validates the keyring
	// the same way; without one it runs previews of repositories without
	// secrets only.
	if cfg, err := LoadWorker(); err != nil || !cfg.SecretsConfigured() {
		t.Fatalf("worker keyring: %v", err)
	}
	t.Setenv("SECRETS_ACTIVE_KEY_VERSION", "")
	if _, err := LoadWorker(); err == nil || strings.Contains(err.Error(), "bm90") {
		t.Fatalf("a worker keyring without an active version must fail without echoing keys: %v", err)
	}
	unsetEnvironment(t, "SECRETS_ENCRYPTION_KEYS", "SECRETS_ACTIVE_KEY_VERSION")
	if _, err := LoadSecretsMaintenance(); err == nil {
		t.Fatal("re-encryption requires a keyring")
	}
	t.Setenv("SECRETS_ENCRYPTION_KEYS", "1:bm90LWEtcmVhbC1rZXk=")
	t.Setenv("SECRETS_ACTIVE_KEY_VERSION", "1")
	if _, err := LoadSecretsMaintenance(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionSettingsDefaultsAndOverrides(t *testing.T) {
	validEnvironment(t)
	executionKeys := []string{"EGRESS_IMAGE", "BUILDKIT_IMAGE", "BUILD_CPUS", "BUILD_MEMORY", "BUILD_PIDS", "BUILD_DISK_BUDGET", "BUILD_MAX_IMAGE_SIZE", "BUILD_MIN_FREE_DISK",
		"PREVIEW_ALLOW_ROOT", "PREVIEW_WRITABLE_ROOTFS", "PREVIEW_TMPFS_SIZE", "PREVIEW_PIDS", "PREVIEW_STARTUP_TIMEOUT", "PREVIEW_MEMORY_BUDGET", "PREVIEW_CPU_BUDGET", "PREVIEW_MAX_PER_REPOSITORY", "PREVIEW_OCI_RUNTIME"}
	unsetEnvironment(t, executionKeys...)
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	b, p := cfg.BuilderConfig, cfg.PreviewConfig
	if cfg.EgressImage != "previa-egress:dev" || b.BuildKitImage != DefaultBuildKitImage || b.BuildCPUs != 2 ||
		b.BuildMemory != 4<<30 || b.BuildPIDs != 2048 || b.BuildDiskBudget != 20<<30 || b.BuildMaxImageSize != 4<<30 || b.BuildMinFreeDisk != 15<<30 {
		t.Fatalf("builder defaults: %+v", b)
	}
	if p.PreviewAllowRoot || p.PreviewWritableRootfs || p.PreviewTmpfsSize != 256<<20 || p.PreviewPIDs != 512 || p.PreviewStartupTimeout != 2*time.Minute ||
		p.PreviewMemoryBudget != 0 || p.PreviewCPUBudget != 0 || p.PreviewMaxPerRepository != 10 || p.PreviewOCIRuntime != "" {
		t.Fatalf("preview defaults: %+v", p)
	}
	t.Setenv("BUILD_MEMORY", "512MiB")
	t.Setenv("BUILD_CPUS", "0.5")
	t.Setenv("BUILD_MAX_IMAGE_SIZE", "1073741824")
	t.Setenv("PREVIEW_ALLOW_ROOT", "true")
	t.Setenv("PREVIEW_MEMORY_BUDGET", "6GiB")
	t.Setenv("PREVIEW_OCI_RUNTIME", "runsc")
	e, err := LoadExecution()
	if err != nil || e.BuildMemory != 512<<20 || e.BuildCPUs != 0.5 || e.BuildMaxImageSize != 1<<30 || !e.PreviewAllowRoot || e.PreviewMemoryBudget != 6<<30 || e.PreviewOCIRuntime != "runsc" {
		t.Fatalf("overrides: %+v %v", e, err)
	}
	if got := ByteSize(512 << 20).String(); got != "512MiB" {
		t.Fatalf("String: %q", got)
	}
	for key, value := range map[string]string{
		"BUILD_MEMORY": "4GB", "BUILD_CPUS": "0", "BUILD_PIDS": "10", "BUILD_DISK_BUDGET": "100MiB",
		"BUILD_MAX_IMAGE_SIZE": "64GiB", "BUILD_MIN_FREE_DISK": "-1GiB", "EGRESS_IMAGE": " ",
		"PREVIEW_TMPFS_SIZE": "1MiB", "PREVIEW_PIDS": "8", "PREVIEW_STARTUP_TIMEOUT": "1s", "PREVIEW_MEMORY_BUDGET": "-1GiB",
		"PREVIEW_CPU_BUDGET": "-2", "PREVIEW_MAX_PER_REPOSITORY": "0", "PREVIEW_OCI_RUNTIME": "../runsc", "PREVIEW_ALLOW_ROOT": "maybe",
	} {
		t.Run(key, func(t *testing.T) {
			validEnvironment(t)
			unsetEnvironment(t, executionKeys...)
			t.Setenv(key, value)
			if _, err := LoadWorker(); err == nil {
				t.Fatalf("%s=%q accepted", key, value)
			}
		})
	}
}
