package repoconfig

import (
	"testing"
	"time"
)

func TestBuildContextAndTimeout(t *testing.T) {
	platform := Platform{Limits: DefaultLimits(), BuildTimeout: 30 * time.Minute}
	result, err := ParseFor([]byte("context: apps/web\nbuild_timeout: 20m\n"), platform)
	if err != nil || result.Config.Context != "apps/web" || result.Config.Dockerfile != "Dockerfile" || result.Config.BuildTimeout != 20*time.Minute {
		t.Fatalf("build settings: %+v %v", result.Config, err)
	}
	defaults, err := ParseFor([]byte("{}"), platform)
	if err != nil || defaults.Config.Context != "." || defaults.Config.BuildTimeout != platform.BuildTimeout || defaults.Hash == result.Hash {
		t.Fatalf("defaults: %+v %v", defaults, err)
	}
	for _, tc := range []struct{ yaml, code, field string }{
		{"context: ../app", "invalid_context", "context"},
		{"context: /app", "invalid_context", "context"},
		{"context: 'app/../web'", "invalid_context", "context"},
		{"context: ''", "invalid_context", "context"},
		{"context: null", "invalid_type", "context"},
		{"build_timeout: 31m", "invalid_build_timeout", "build_timeout"},
		{"build_timeout: -1m", "invalid_build_timeout", "build_timeout"},
		{"build_timeout: 0s", "invalid_build_timeout", "build_timeout"},
		{"build_timeout: nope", "invalid_build_timeout", "build_timeout"},
		{"build_timeout: ''", "invalid_type", "build_timeout"},
		{"build_timeout: 12", "invalid_type", "build_timeout"},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			_, err := ParseFor([]byte(tc.yaml), platform)
			assertIssue(t, err, tc.code, tc.field)
		})
	}
}

func TestMySQLCatalogResolution(t *testing.T) {
	platform := Platform{Limits: DefaultLimits(), Services: catalog{"mysql": {"8.4": "mysql@sha256:pinned"}}}
	result, err := ParseFor([]byte("services: {mysql: 8.4}"), platform)
	if err != nil || len(result.Config.Services) != 1 || result.Config.Services[0].Image != "mysql@sha256:pinned" {
		t.Fatalf("mysql: %+v %v", result.Config.Services, err)
	}
}
