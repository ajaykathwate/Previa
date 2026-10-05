package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationalDefaults(t *testing.T) {
	validEnvironment(t)
	unsetEnvironment(t, "APP_ENV", "INTERNAL_ADDR", "WORKER_INTERNAL_ADDR", "TRUSTED_PROXIES", "BUILDS_PAUSED", "BACKUP_STATUS_FILE")
	server, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if server.Production() || server.InternalAddr != "127.0.0.1:9090" || worker.InternalAddr != "127.0.0.1:9091" || worker.BuildsPaused {
		t.Fatalf("defaults: %+v %+v", server.EnvironmentConfig, worker.EnvironmentConfig)
	}
	t.Setenv("BUILDS_PAUSED", "true")
	if worker, err = LoadWorker(); err != nil || !worker.BuildsPaused {
		t.Fatalf("kill switch not read: %v", err)
	}
}

func TestOperationalSettingsAreValidated(t *testing.T) {
	for _, c := range []struct{ key, value, want string }{
		{"APP_ENV", "staging", "APP_ENV"},
		{"INTERNAL_ADDR", "9090", "INTERNAL_ADDR"},
		{"TRUSTED_PROXIES", "10.0.0.0/8, not-a-proxy", "TRUSTED_PROXIES"},
	} {
		validEnvironment(t)
		t.Setenv(c.key, c.value)
		if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s=%q: %v", c.key, c.value, err)
		}
	}
	validEnvironment(t)
	t.Setenv("WORKER_INTERNAL_ADDR", "localhost")
	if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "WORKER_INTERNAL_ADDR") {
		t.Fatalf("worker internal address: %v", err)
	}
}

func TestTrustedProxies(t *testing.T) {
	prefixes, err := ParseTrustedProxies(" 172.18.0.0/16 ,10.1.2.3,, fd00::/8 ")
	if err != nil || len(prefixes) != 3 || prefixes[1].Bits() != 32 {
		t.Fatalf("%v %v", prefixes, err)
	}
	if none, err := ParseTrustedProxies(""); err != nil || none != nil {
		t.Fatal("empty TRUSTED_PROXIES must trust nobody")
	}
}

func TestProductionRequiresHTTPSWebhookSecretAndPrivateKeys(t *testing.T) {
	validEnvironment(t)
	t.Setenv("APP_ENV", "production")
	if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain-HTTP localhost in production: %v", err)
	}
	t.Setenv("APP_BASE_URL", "https://previa.example.com")
	unsetEnvironment(t, "GITHUB_WEBHOOK_SECRET")
	if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), "GITHUB_WEBHOOK_SECRET") {
		t.Fatalf("no webhook secret in production: %v", err)
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", "webhook-secret")
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}

	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, []byte("not checked here"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GITHUB_APP_ID": "7", "GITHUB_APP_SLUG": "previa", "GITHUB_APP_PRIVATE_KEY_PATH": key, "GITHUB_APP_CLIENT_ID": "id", "GITHUB_APP_CLIENT_SECRET": "secret"} {
		t.Setenv(k, v)
	}
	if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable key accepted by the server: %v", err)
	}
	if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable key accepted by the worker: %v", err)
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorker(); err != nil {
		t.Fatal(err)
	}
	// Development keeps working with permissive modes (for example on /mnt/c).
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "development")
	if _, err := LoadWorker(); err != nil {
		t.Fatal(err)
	}
}
