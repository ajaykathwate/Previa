package config

import (
	"strings"
	"testing"
)

func TestCheckPreviewDomainKeepsTheDashboardOffPreviewHosts(t *testing.T) {
	for _, c := range []struct {
		domain, app string
		ok          bool
	}{
		{"preview.example.com", "https://previa.example.com", true},
		{"preview-example.net", "https://previa.example.com", true},
		{"preview.localhost", "http://localhost:8080", true},
		{"preview.example.com", "https://preview.example.com", false},     // the dashboard is the preview domain
		{"preview.example.com", "https://app.preview.example.com", false}, // inside the wildcard
		{"example.com", "https://previa.example.com", false},              // a wildcard over the dashboard
		{"Preview.example.com", "https://previa.example.com", false},
		{"preview", "https://previa.example.com", false},
		{"preview.example.com", "http://previa.example.com", false}, // plain HTTP outside localhost
		{"preview.example.com", "", false},
		{"pre view.example.com", "https://previa.example.com", false},
	} {
		if err := CheckPreviewDomain(c.domain, c.app); (err == nil) != c.ok {
			t.Fatalf("CheckPreviewDomain(%q, %q) = %v", c.domain, c.app, err)
		}
	}
}

func TestPreviewSameSite(t *testing.T) {
	if !PreviewSameSite("preview.example.com", "https://previa.example.com") {
		t.Fatal("same registrable domain not detected")
	}
	if PreviewSameSite("preview.example-usercontent.com", "https://previa.example.com") {
		t.Fatal("separate registrable domains reported as same site")
	}
	// Public suffixes: two customers of one hosting suffix are different sites.
	if PreviewSameSite("preview.a.github.io", "https://b.github.io") {
		t.Fatal("public suffix treated as one site")
	}
}

func TestWorkerRoutingSettings(t *testing.T) {
	validEnvironment(t)
	dir := t.TempDir()
	cfg, err := LoadWorker()
	if err != nil || cfg.RoutingConfigured() {
		t.Fatalf("routing should be optional: %+v %v", cfg.RoutingConfig, err)
	}
	t.Setenv("PREVIEW_BASE_DOMAIN", "preview.example.com")
	t.Setenv("APP_BASE_URL", "https://previa.example.com")
	t.Setenv("ROUTER_DYNAMIC_DIR", dir)
	cfg, err = LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RoutingConfigured() || cfg.RouterContainer != "previa-router" || cfg.RouterEntryPoint != "websecure" || cfg.RouterAPIAddress != "127.0.0.1:8080" {
		t.Fatalf("routing defaults %+v", cfg.RoutingConfig)
	}
	for key, value := range map[string]string{
		"ROUTER_DYNAMIC_DIR": "relative/dir",
		"ROUTER_API_ADDRESS": "0.0.0.0:8080",
		"ROUTER_CONTAINER":   "bad name",
		"ROUTER_ENTRYPOINT":  "web secure",
		"APP_BASE_URL":       "https://app.preview.example.com",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "routing") {
				t.Fatalf("accepted %s=%s: %v", key, value, err)
			}
		})
	}
	// Each refusal names the fix, not only the rule.
	for value, want := range map[string]string{
		"":                "ROUTER_DYNAMIC_DIR is required when PREVIEW_BASE_DOMAIN is set",
		"~/previa/router": "must be a full, clean path",
		dir + "/":         "no trailing slash",
		dir + "/missing":  "run make router-up",
	} {
		t.Setenv("ROUTER_DYNAMIC_DIR", value)
		if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ROUTER_DYNAMIC_DIR=%q: %v, want a message with %q", value, err, want)
		}
	}
}

func TestWorkerRouterDirectoryNeedsAPreviewDomain(t *testing.T) {
	validEnvironment(t)
	t.Setenv("ROUTER_DYNAMIC_DIR", t.TempDir())
	unsetEnvironment(t, "PREVIEW_BASE_DOMAIN")
	if _, err := LoadWorker(); err == nil {
		t.Fatal("accepted a router directory without a preview domain")
	}
}

func TestServerRefusesOverlappingPreviewDomain(t *testing.T) {
	validEnvironment(t)
	t.Setenv("APP_BASE_URL", "https://previa.example.com")
	t.Setenv("PREVIEW_BASE_DOMAIN", "example.com")
	if _, err := LoadServer(); err == nil {
		t.Fatal("server accepted previews over the dashboard host")
	}
	t.Setenv("PREVIEW_BASE_DOMAIN", "preview.example.com")
	if _, err := LoadServer(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRouteSyncNeedsACompleteRoutingConfiguration(t *testing.T) {
	validEnvironment(t)
	if _, err := LoadRouteSync(); err == nil {
		t.Fatal("route sync ran without PREVIEW_BASE_DOMAIN")
	}
	t.Setenv("PREVIEW_BASE_DOMAIN", "preview.example.com")
	t.Setenv("APP_BASE_URL", "https://previa.example.com")
	if _, err := LoadRouteSync(); err == nil {
		t.Fatal("route sync ran without ROUTER_DYNAMIC_DIR")
	}
	t.Setenv("ROUTER_DYNAMIC_DIR", t.TempDir())
	cfg, err := LoadRouteSync()
	if err != nil || cfg.RouterEntryPoint != "websecure" || cfg.DatabaseURL == "" {
		t.Fatalf("route sync configuration %+v %v", cfg.RoutingConfig, err)
	}
}
