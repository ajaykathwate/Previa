package starterkits

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runPreviewEnv(t *testing.T, dockerfile string, env []string, command string) string {
	t.Helper()
	_, script, ok := strings.Cut(dockerfile, "COPY --chmod=755 <<'EOF' /usr/local/bin/preview-env\n")
	if !ok {
		t.Fatal("generated image has no preview-env wrapper")
	}
	script, _, ok = strings.Cut(script, "\nEOF\n")
	if !ok {
		t.Fatal("generated wrapper is incomplete")
	}
	script = strings.ReplaceAll(script, "/tmp/storage", filepath.Join(t.TempDir(), "storage"))
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script, "preview-env", "sh", "-c", command)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preview-env: %v\n%s", err, out)
	}
	return string(out)
}

func TestMySQLFrameworkWrappersUseTheSelectedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, postgres, databaseURL, laravel, spring string
	}{
		{"mysql", "", "mysql://preview:pass@mysql:3306/preview", "mysql", "jdbc:mysql://mysql:3306/preview"},
		{"postgres-wins", "17", "postgresql://preview:pass@postgres:5432/preview", "pgsql", "jdbc:postgresql://postgres:5432/preview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"DATABASE_URL=" + tc.databaseURL, "MYSQL_HOST=mysql", "MYSQL_PORT=3306", "MYSQL_DATABASE=preview", "MYSQL_USER=preview", "MYSQL_PASSWORD=pass"}
			if tc.postgres != "" {
				env = append(env, "PGHOST=postgres", "PGPORT=5432", "PGDATABASE=preview", "PGUSER=preview", "PGPASSWORD=pass")
			}
			laravel := Generate(Choices{Stack: "laravel", MySQL: "8.4", Postgres: tc.postgres}, defaults)
			if !strings.Contains(laravel.Dockerfile, "pdo_mysql") {
				t.Fatal("MySQL kit needs the PHP driver")
			}
			if got := runPreviewEnv(t, laravel.Dockerfile, env, `printf '%s\n%s' "$DB_CONNECTION" "$DB_URL"`); got != tc.laravel+"\n"+tc.databaseURL {
				t.Fatalf("Laravel database mapping: %q", got)
			}
			spring := Generate(Choices{Stack: "spring", MySQL: "8.4", Postgres: tc.postgres}, defaults)
			if got := runPreviewEnv(t, spring.Dockerfile, env, `printf '%s\n%s\n%s' "$SPRING_DATASOURCE_URL" "$SPRING_DATASOURCE_USERNAME" "$SPRING_DATASOURCE_PASSWORD"`); got != tc.spring+"\npreview\npass" {
				t.Fatalf("Spring database mapping: %q", got)
			}
		})
	}
}
