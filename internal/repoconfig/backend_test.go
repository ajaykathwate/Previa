package repoconfig

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// catalog is a fixed ServiceCatalog for tests.
type catalog map[string]map[string]string

func (c catalog) Image(kind, version string) (string, bool) {
	image, ok := c[kind][version]
	return image, ok
}

func (c catalog) Versions(kind string) []string {
	var versions []string
	for v := range c[kind] {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	slices.Reverse(versions)
	return versions
}

var testCatalog = catalog{
	"postgres": {"17": "postgres@sha256:17", "16": "postgres@sha256:16"},
	"redis":    {"7": "redis@sha256:7"},
}

func backendPlatform() Platform { return Platform{Limits: DefaultLimits(), Services: testCatalog} }

func TestBackendConfig(t *testing.T) {
	result, err := ParseFor(fixture(t, "backend.yaml"), backendPlatform())
	if err != nil {
		t.Fatal(err)
	}
	cfg := result.Config
	want := []BackendService{{Kind: "postgres", Version: "17", Image: "postgres@sha256:17"}, {Kind: "redis", Version: "7", Image: "redis@sha256:7"}}
	if !slices.Equal(cfg.Services, want) {
		t.Fatalf("services %+v", cfg.Services)
	}
	if !slices.Equal(cfg.Setup, []string{"/app/bin/api", "migrate", "--seed"}) || cfg.DocsPath != "/docs" {
		t.Fatalf("setup %q docs %q", cfg.Setup, cfg.DocsPath)
	}
	if len(cfg.Processes) != 2 || cfg.Processes[0].Name != "scheduler" || cfg.Processes[1].Name != "worker" {
		t.Fatalf("processes not sorted by name: %+v", cfg.Processes)
	}
	if cfg.Processes[1].ResourceTier != "small" || cfg.Processes[1].Resources != (Resources{500, 512 << 20}) || cfg.Processes[0].Resources != (Resources{1000, 1 << 30}) {
		t.Fatalf("process tiers %+v", cfg.Processes)
	}

	// The hash follows what runs: commands, versions, images and the docs
	// path change it; the order of processes and services does not.
	base := result.Hash
	reordered, err := ParseFor([]byte(strings.Replace(string(fixture(t, "backend.yaml")), "  redis: 7\n  postgres: \"17\"\n", "  postgres: 17\n  redis: \"7\"\n", 1)), backendPlatform())
	if err != nil || reordered.Hash != base {
		t.Fatalf("reordered services changed the hash: %v", err)
	}
	for _, change := range []struct{ from, to string }{
		{`"17"`, `"16"`},
		{`"--seed"`, `"--no-seed"`},
		{`"schedule"`, `"cron"`},
		{"docs_path: /docs", "docs_path: /api-docs"},
	} {
		changed, err := ParseFor([]byte(strings.Replace(string(fixture(t, "backend.yaml")), change.from, change.to, 1)), backendPlatform())
		if err != nil || changed.Hash == base {
			t.Errorf("%s -> %s left the hash unchanged (%v)", change.from, change.to, err)
		}
	}
	other := Platform{Limits: DefaultLimits(), Services: catalog{"postgres": {"17": "postgres@sha256:other"}, "redis": {"7": "redis@sha256:7"}}}
	if repinned, err := ParseFor(fixture(t, "backend.yaml"), other); err != nil || repinned.Hash == base {
		t.Fatalf("a re-pinned image left the hash unchanged (%v)", err)
	}

	// Without backend keys the hash is what it always was.
	plain, err := Parse([]byte("port: 8080\n"), DefaultLimits())
	if err != nil || plain.Config.Services != nil || plain.Config.Setup != nil || len(plain.Config.Processes) != 0 || plain.Config.DocsPath != "" {
		t.Fatalf("plain config %+v %v", plain.Config, err)
	}
}

func TestBackendConfigProblems(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, code, field string
		platform                Platform
	}{
		{"unknown service", "services: {mongo: 8}", "unknown_service", "services", backendPlatform()},
		{"version not offered", "services: {postgres: 15}", "unknown_service_version", "services.postgres", backendPlatform()},
		{"no catalog", "services: {postgres: 17}", "service_unavailable", "services.postgres", Platform{Limits: DefaultLimits()}},
		{"kind not in catalog", "services: {redis: 7}", "service_unavailable", "services.redis", Platform{Limits: DefaultLimits(), Services: catalog{"postgres": {"17": "p@sha256:1"}}}},
		{"version text", "services: {postgres: latest}", "invalid_service_version", "services.postgres", backendPlatform()},
		{"services as a list", "services: [postgres]", "invalid_type", "services", backendPlatform()},
		{"service without version", "services: {postgres: {version: 17}}", "invalid_type", "services", backendPlatform()},
		{"duplicate service", "services:\n  postgres: 17\n  postgres: 16\n", "duplicate_key", "services", backendPlatform()},
		{"setup as a string", "setup: npm run migrate", "invalid_type", "setup", backendPlatform()},
		{"setup number argument", "setup: [sleep, 5]", "invalid_type", "setup[1]", backendPlatform()},
		{"empty setup", "setup: []", "invalid_command", "setup", backendPlatform()},
		{"blank program", `setup: [" ", migrate]`, "invalid_command", "setup", backendPlatform()},
		{"too many arguments", "setup: [" + strings.Repeat("a, ", MaxCommandArgs) + "a]", "invalid_command", "setup", backendPlatform()},
		{"process without command", "processes: [{name: worker}]", "missing_field", "processes[0].command", backendPlatform()},
		{"process unknown key", "processes: [{name: worker, command: [run], replicas: 2}]", "unknown_field", "processes[0]", backendPlatform()},
		{"process name", "processes: [{name: Worker_1, command: [run]}]", "invalid_process_name", "processes[0].name", backendPlatform()},
		{"reserved name", "processes: [{name: setup, command: [run]}]", "reserved_process_name", "processes[0].name", backendPlatform()},
		{"duplicate name", "processes: [{name: w, command: [a]}, {name: w, command: [b]}]", "duplicate_process", "processes[1].name", backendPlatform()},
		{"too many processes", "processes: [{name: a, command: [a]}, {name: b, command: [b]}, {name: c, command: [c]}, {name: d, command: [d]}]", "too_many_processes", "processes", backendPlatform()},
		{"process tier", "processes: [{name: w, command: [a], resource_tier: huge}]", "invalid_tier", "processes[0].resource_tier", backendPlatform()},
		{"process as a string", "processes: [worker]", "invalid_type", "processes[0]", backendPlatform()},
		{"anchored setup", "setup: &cmd [run]\n", "invalid_type", "setup", backendPlatform()},
		{"aliased command", "setup: &cmd [run]\nprocesses: [{name: w, command: *cmd}]\n", "invalid_type", "processes[0].command", backendPlatform()},
		{"seed without a database", "services: {redis: 7}\nseed: [run, seed]", "seed_without_database", "seed", backendPlatform()},
		{"seed as a string", "services: {postgres: 17}\nseed: npm run seed", "invalid_type", "seed", backendPlatform()},
		{"empty seed", "services: {postgres: 17}\nseed: []", "invalid_command", "seed", backendPlatform()},
		{"process named seed", "processes: [{name: seed, command: [run]}]", "reserved_process_name", "processes[0].name", backendPlatform()},
		{"docs path with a host", "docs_path: //example.com/docs", "invalid_docs_path", "docs_path", backendPlatform()},
		{"docs path traversal", "docs_path: /a/../b", "invalid_docs_path", "docs_path", backendPlatform()},
		{"docs path as a list", "docs_path: [/docs]", "invalid_type", "docs_path", backendPlatform()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFor([]byte(tc.yaml), tc.platform)
			assertIssue(t, err, tc.code, tc.field)
		})
	}
}

func TestBackendMessagesNeverEchoSource(t *testing.T) {
	_, err := ParseFor([]byte("services: {\"<script>steal()</script>\": 1, postgres: \"1; DROP\"}"), backendPlatform())
	if err == nil || strings.Contains(err.Error(), "script") || strings.Contains(err.Error(), "DROP") {
		t.Fatalf("message echoed source text: %v", err)
	}
}

func TestProcessTiersAreClamped(t *testing.T) {
	result, err := ParseFor([]byte("processes: [{name: worker, command: [run], resource_tier: large}]"), Platform{Limits: Resources{CPUMillis: 750, MemoryBytes: 768 << 20}, Services: testCatalog})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Config.Processes[0].Resources; got != (Resources{750, 768 << 20}) {
		t.Fatalf("process resources %v", got)
	}
	fields := 0
	for _, n := range result.Notices {
		if n.Field == "processes[0].resource_tier" {
			fields++
		}
	}
	if fields != 2 {
		t.Fatalf("notices %+v", result.Notices)
	}
}

// seed is a separate one-off command that creates the starting data; it is
// part of what runs, so it changes the configuration hash, and a
// configuration without it hashes as before.
func TestSeedCommand(t *testing.T) {
	base, err := ParseFor([]byte("services: {postgres: 17}\nsetup: [bin/migrate]\n"), backendPlatform())
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := ParseFor([]byte("services: {postgres: 17}\nsetup: [bin/migrate]\nseed: [bin/seed, --demo]\n"), backendPlatform())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seeded.Config.Seed, []string{"bin/seed", "--demo"}) || base.Config.Seed != nil {
		t.Fatalf("seed %q / %q", seeded.Config.Seed, base.Config.Seed)
	}
	if seeded.Hash == base.Hash {
		t.Fatal("the seed command does not change the configuration hash")
	}
	if strings.Contains(string(mustJSON(t, base.Config)), "seed") {
		t.Fatal("a configuration without seed encodes a seed field")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
