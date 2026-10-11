package starterkits

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// catalog offers Previa's default service versions, as a worker would.
type testCatalog map[string][]string

func (c testCatalog) Versions(kind string) []string { return c[kind] }
func (c testCatalog) Image(kind, version string) (string, bool) {
	return "example/" + kind + ":" + version, slices.Contains(c[kind], version)
}

var defaults = Offer{Postgres: []string{"17", "16"}, Redis: []string{"8", "7"}}

func parse(t *testing.T, config string) repoconfig.Config {
	t.Helper()
	platform := repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: testCatalog{"postgres": defaults.Postgres, "redis": defaults.Redis}}
	result, err := repoconfig.ParseFor([]byte(config), platform)
	if err != nil {
		t.Fatalf("Previa rejects the generated config: %v\n%s", err, config)
	}
	return result.Config
}

// Every combination of every stack writes a .previewrc.yaml that Previa's
// own parser accepts, with the choices it was asked for.
func TestEveryKitPassesPreviasConfigRules(t *testing.T) {
	services := [][2]string{{"", ""}, {"17", ""}, {"", "8"}, {"16", "7"}}
	for _, s := range Stacks() {
		setups := append(presetKeys(s.Setup), Custom, "")
		workers := append(presetKeys(s.Workers), Custom, "")
		for _, svc := range services {
			for _, setup := range setups {
				for _, worker := range workers {
					name := fmt.Sprintf("%s/pg%s-redis%s/setup-%s/worker-%s", s.ID, svc[0], svc[1], setup, worker)
					t.Run(name, func(t *testing.T) {
						c := Fresh(s.ID)
						c.Postgres, c.Redis = svc[0], svc[1]
						c.Setup, c.SetupWith, c.SetupCommand = setup != "", setup, "npm run seed && echo seeded"
						c.Worker, c.WorkerWith, c.WorkerCommand = worker != "", worker, "bin/worker --queue default"
						c.Docs = true
						kit := Generate(c, defaults)
						if s.NoShell && setup == Custom {
							if !hasProblem(kit, "setup") {
								t.Fatal("a shell command in an image without a shell must be refused")
							}
							return
						}
						if len(kit.Problems) > 0 {
							t.Fatalf("unexpected problems: %+v", kit.Problems)
						}
						cfg := parse(t, kit.Config)
						checkKit(t, s, kit, cfg)
					})
				}
			}
		}
	}
}

func checkKit(t *testing.T, s Stack, kit Kit, cfg repoconfig.Config) {
	t.Helper()
	c := kit.Choices
	if cfg.Port != c.Port || cfg.HealthCheckPath != c.Health || cfg.ResourceTier != c.Tier {
		t.Fatalf("port/health/tier: got %d %q %q, chose %d %q %q", cfg.Port, cfg.HealthCheckPath, cfg.ResourceTier, c.Port, c.Health, c.Tier)
	}
	wantServices := 0
	for _, v := range []string{c.Postgres, c.Redis} {
		if v != "" && !s.Static {
			wantServices++
		}
	}
	if len(cfg.Services) != wantServices {
		t.Fatalf("services: got %d, want %d", len(cfg.Services), wantServices)
	}
	if (c.Setup && !s.Static) != (len(cfg.Setup) > 0) {
		t.Fatalf("setup: chose %v, got %q", c.Setup, cfg.Setup)
	}
	if (c.Worker && !s.Static) != (len(cfg.Processes) == 1) {
		t.Fatalf("worker: chose %v, got %+v", c.Worker, cfg.Processes)
	}
	if s.OwnFile {
		if kit.Dockerfile != "" {
			t.Fatal("a repository with its own Dockerfile gets no new one")
		}
		return
	}
	if !strings.Contains(kit.Dockerfile, fmt.Sprintf("EXPOSE %d", c.Port)) {
		t.Fatalf("the Dockerfile must expose port %d:\n%s", c.Port, kit.Dockerfile)
	}
	first := firstInstruction(kit.Dockerfile)
	if !strings.HasPrefix(first, "FROM ") {
		t.Fatalf("the Dockerfile must start with FROM, got %q", first)
	}
	if len(s.Wrapper) > 0 && !strings.Contains(kit.Dockerfile, "/usr/local/bin/preview-env") {
		t.Fatal("setup and processes call preview-env, so the image must have it")
	}
}

func firstInstruction(dockerfile string) string {
	for line := range strings.SplitSeq(dockerfile, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

func presetKeys(presets []Preset) []string {
	keys := make([]string, len(presets))
	for i, p := range presets {
		keys[i] = p.Key
	}
	return keys
}

func hasProblem(kit Kit, field string) bool {
	return slices.ContainsFunc(kit.Problems, func(p Problem) bool { return p.Field == field })
}

func TestChoicesAreCheckedAndCorrected(t *testing.T) {
	c := Fresh("django")
	c.Port, c.Health, c.Detail, c.Tier = 70000, "health", "my-site", "huge"
	c.Postgres = "12"
	kit := Generate(c, defaults)
	for _, field := range []string{"port", "health", "detail"} {
		if !hasProblem(kit, field) {
			t.Errorf("expected a problem for %s, got %+v", field, kit.Problems)
		}
	}
	if kit.Choices.Port != 8000 || kit.Choices.Detail != "mysite" || kit.Choices.Tier != "standard" || kit.Choices.Postgres != "17" {
		t.Fatalf("invalid choices fall back to safe values, got %+v", kit.Choices)
	}
	parse(t, kit.Config)

	none := Generate(Choices{Stack: "rails", Postgres: "17"}, Offer{Known: true})
	if !hasProblem(none, "postgres") || none.Choices.Postgres != "" {
		t.Fatalf("a server that offers no PostgreSQL says so: %+v", none.Problems)
	}
	static := Generate(Choices{Stack: "vite", Postgres: "17", Setup: true, Worker: true}, defaults)
	if static.Choices.Postgres != "" || strings.Contains(static.Config, "setup:") || strings.Contains(static.Config, "processes:") {
		t.Fatalf("a static site runs no services, setup or processes:\n%s", static.Config)
	}
}

func TestCustomCommandsSplitOrUseTheShell(t *testing.T) {
	for _, tc := range []struct {
		stack, text string
		want        []string
	}{
		{"node-api", "npm run seed", []string{"npm", "run", "seed"}},
		{"node-api", "npm run migrate && npm run seed", []string{"sh", "-c", "npm run migrate && npm run seed"}},
		{"laravel", "php artisan db:seed", []string{"preview-env", "php", "artisan", "db:seed"}},
		{"go", "/app/server seed --demo", []string{"/app/server", "seed", "--demo"}},
	} {
		got, problem := commandFor(Find(tc.stack), true, Custom, tc.text, nil, Choices{})
		if problem != "" || !slices.Equal(got, tc.want) {
			t.Errorf("%s %q: got %q (%s), want %q", tc.stack, tc.text, got, problem, tc.want)
		}
	}
	if _, problem := commandFor(Find("go"), true, Custom, "a && b", nil, Choices{}); problem == "" {
		t.Error("an image without a shell refuses shell syntax")
	}
	if _, problem := commandFor(Find("node-api"), true, Custom, "  ", nil, Choices{}); problem == "" {
		t.Error("an empty command is a problem")
	}
}

func TestUserTextIsQuotedInTheConfig(t *testing.T) {
	c := Fresh("node-api")
	c.Worker, c.WorkerWith, c.WorkerCommand = true, Custom, `node -e "console.log('a: b # c')"`
	kit := Generate(c, defaults)
	cfg := parse(t, kit.Config)
	if got := cfg.Processes[0].Command; !slices.Equal(got, []string{"sh", "-c", c.WorkerCommand}) {
		t.Fatalf("the command must survive YAML unchanged, got %q", got)
	}
	c.WorkerCommand = "npm run migrate && npm run seed > /tmp/seed.log"
	if config := Generate(c, defaults).Config; !strings.Contains(config, `"npm run migrate && npm run seed > /tmp/seed.log"`) {
		t.Fatalf("commands stay readable, without escapes:\n%s", config)
	}
}
