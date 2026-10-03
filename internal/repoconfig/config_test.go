package repoconfig

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConfigFixtures(t *testing.T) {
	for _, tc := range []struct {
		file, code, field string
	}{
		{"valid.yaml", "", ""},
		{"minimal.yaml", "", ""},
		{"unknown_key.yaml", "unknown_field", "$"},
		{"duplicate_key.yaml", "duplicate_key", "$"},
		{"path_traversal.yaml", "invalid_path", "dockerfile"},
		{"absolute_path.yaml", "invalid_path", "dockerfile"},
		{"invalid_port.yaml", "invalid_port", "port"},
		{"invalid_env.yaml", "invalid_env", "env[0]"},
		{"malformed_resources.yaml", "invalid_type", "resource_tier"},
		{"unknown_tier.yaml", "invalid_tier", "resource_tier"},
		{"invalid_health_path.yaml", "invalid_health_path", "health_check_path"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			result, err := Parse(fixture(t, tc.file), DefaultLimits())
			if tc.code != "" {
				assertIssue(t, err, tc.code, tc.field)
				return
			}
			if err != nil || len(result.Hash) != 64 {
				t.Fatalf("result hash=%q err=%v", result.Hash, err)
			}
			if tc.file == "valid.yaml" {
				if result.Config.Dockerfile != "docker/Dockerfile" || result.Config.Port != 8080 || result.Config.HealthCheckPath != "/health" || result.Config.Resources.CPUMillis != 500 || len(result.Config.Env) != 2 {
					t.Fatal("explicit config not preserved/normalized")
				}
			} else if result.Config.Dockerfile != "Dockerfile" || result.Config.Port != 3000 || result.Config.HealthCheckPath != "" || len(result.Config.Env) != 0 || result.Config.ResourceTier != "standard" || result.Config.Resources != (Resources{1000, 1024 * 1024 * 1024}) {
				t.Fatal("approved defaults not applied")
			}
		})
	}
}

func assertIssue(t *testing.T, err error, code, field string) {
	t.Helper()
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected ValidationError; got %v", err)
	}
	for _, issue := range validation.Issues {
		if issue.Code == code && issue.Field == field {
			return
		}
	}
	t.Fatalf("expected %s at %s, got %v", code, field, validation)
}

func TestResourceTiersAndCeilings(t *testing.T) {
	for tier, expected := range map[string]Resources{
		"small": {500, 512 * 1024 * 1024}, "standard": {1000, 1024 * 1024 * 1024}, "large": {2000, 2 * 1024 * 1024 * 1024},
	} {
		t.Run(tier, func(t *testing.T) {
			result, err := Parse([]byte("resource_tier: "+tier), DefaultLimits())
			if err != nil || result.Config.Resources != expected || len(result.Notices) != 0 {
				t.Fatalf("resources=%v notices=%v err=%v", result.Config.Resources, result.Notices, err)
			}
		})
	}
	limits := Resources{750, 768 * 1024 * 1024}
	result, err := Parse(fixture(t, "over_ceiling.yaml"), limits)
	if err != nil || result.Config.Resources != limits || len(result.Notices) != 2 {
		t.Fatalf("clamp resources=%v notices=%v err=%v", result.Config.Resources, result.Notices, err)
	}
	if result.Notices[0].Code != "cpu_clamped" || result.Notices[1].Code != "memory_clamped" {
		t.Fatal("clamp notices missing")
	}
	for _, limits := range []Resources{{0, 1024}, {1, 0}, {-1, 1024}} {
		if _, err := Parse([]byte("{}"), limits); err == nil {
			t.Fatal("accepted invalid platform ceilings")
		}
	}
}

func TestStrictTypesAndDocuments(t *testing.T) {
	for _, input := range []string{
		`port: "3000"`, "port: 3000.5", "port: true", "port: null", "env: null",
		"dockerfile: 123", "dockerfile: null", "env: [123]", "env: [true]", "env: {NAME: value}",
		"health_check_path: false", "resource_tier: 1", "port: !custom 3000",
		"dockerfile: &file Dockerfile", "env: [&secret PRIVATE=value, *secret]",
		"<<: {port: 3000}", "? [port]\n: 3000", "[]", "null", "", "# comment only",
		"{}\n---\n{}", "{}\n---", "port: [", "port: 3000\n\"port\": 4000",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := Parse([]byte(input), DefaultLimits()); err == nil {
				t.Fatal("accepted ambiguous/incorrect YAML")
			}
		})
	}
}

func TestPathsPortsAndEnvironmentValidation(t *testing.T) {
	for _, value := range []string{"", ".", "./", "dir/", "a/../Dockerfile", `C:\Dockerfile`, `..\Dockerfile`, " Dockerfile", "Dockerfile\x00"} {
		if validDockerfile(value) {
			t.Fatalf("accepted unsafe path %q", value)
		}
	}
	for _, value := range []string{"Dockerfile", "./docker/Dockerfile", "docker/my Dockerfile", ".docker/Dockerfile"} {
		if !validDockerfile(value) {
			t.Fatalf("rejected relative file path %q", value)
		}
	}
	for _, value := range []string{"//example.com/health", "/../health", "/%2e%2e/health", "/health?x=1", "/health#x", "/health check"} {
		if validHealthPath(value) {
			t.Fatalf("accepted invalid health path %q", value)
		}
	}
	for _, value := range []string{"port: 0", "port: -1", "port: 65536"} {
		_, err := Parse([]byte(value), DefaultLimits())
		assertIssue(t, err, "invalid_port", "port")
	}
	for _, port := range []string{"1", "65535"} {
		if _, err := Parse([]byte("port: "+port), DefaultLimits()); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Parse([]byte("env: [NAME=a, NAME=b]"), DefaultLimits())
	assertIssue(t, err, "duplicate_env", "env[1]")
	_, err = Parse([]byte("env: [NO_ASSIGNMENT]"), DefaultLimits())
	assertIssue(t, err, "invalid_env", "env[0]")
	if _, err := Parse([]byte("env: [EMPTY=, _VALID=a=b]"), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}

func TestSafeValidationAndSizeLimit(t *testing.T) {
	const secret = "fixture-private-value"
	for _, input := range []string{"env: [1BAD=" + secret + "]", "port: '" + secret + "'", "env: [NAME=" + secret + ", NAME=other]", "env: [NAME=" + secret + "\nport: ["} {
		_, err := Parse([]byte(input), DefaultLimits())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe/missing validation error: %v", err)
		}
		encoded, encodeErr := json.Marshal(err)
		if encodeErr != nil || strings.Contains(string(encoded), secret) {
			t.Fatal("structured error leaked source")
		}
	}
	_, err := Parse([]byte(strings.Repeat(" ", MaxConfigBytes+1)), DefaultLimits())
	assertIssue(t, err, "too_large", "$")
	_, err = Parse([]byte("port: 0\nenv: [INVALID]\nresource_tier: huge"), DefaultLimits())
	var validation *ValidationError
	if !errors.As(err, &validation) || len(validation.Issues) != 3 {
		t.Fatalf("expected aggregated field issues: %v", err)
	}
}

func TestEffectiveConfigHash(t *testing.T) {
	parse := func(input string, limits Resources) Result {
		t.Helper()
		result, err := Parse([]byte(input), limits)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	base := parse("{}", DefaultLimits())
	explicit := parse("# same effective config\ndockerfile: ./Dockerfile\nport: 3000\nenv: []\nresource_tier: standard\nhealth_check_path: ''", DefaultLimits())
	if base.Hash != explicit.Hash {
		t.Fatal("defaults/spelling/formatting changed effective hash")
	}
	a := parse("env: [B=two, A=one]", DefaultLimits())
	b := parse("env: [A=one, B=two]", DefaultLimits())
	if a.Hash != b.Hash || !reflect.DeepEqual(a.Config.Env, b.Config.Env) {
		t.Fatal("environment ordering changed effective config")
	}
	for _, input := range []string{"port: 3001", "env: [A=different]", "dockerfile: Otherfile", "health_check_path: /health", "resource_tier: small"} {
		if parse(input, DefaultLimits()).Hash == base.Hash {
			t.Fatal("runtime-affecting change did not change hash")
		}
	}
	limits := Resources{500, 512 * 1024 * 1024}
	if parse("resource_tier: large", limits).Hash != parse("resource_tier: small", limits).Hash {
		t.Fatal("same effective resources produced different hashes")
	}
}
