package runtime

import (
	"slices"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

func TestMySQLCredentialsAndResources(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		settings := repoconfig.Config{Services: []repoconfig.BackendService{{Kind: "mysql", Version: "8.4", Image: "mysql@sha256:pinned"}}}
		if postgres {
			settings.Services = append(settings.Services, repoconfig.BackendService{Kind: "postgres"})
		}
		b, err := planBackend("candidate", settings)
		if err != nil {
			t.Fatal(err)
		}
		s := b.services[0]
		if s.password == s.rootPassword || !slices.Contains(b.secrets, s.password) || !slices.Contains(b.secrets, s.rootPassword) {
			t.Fatal("MySQL root and app need separate redacted credentials")
		}
		if !strings.HasPrefix(b.env["MYSQL_URL"], "mysql://preview:") || b.env["MYSQL_HOST"] != s.alias || b.env["MYSQL_PORT"] != "3306" {
			t.Fatal("MySQL connection settings missing")
		}
		if postgres != strings.HasPrefix(b.env["DATABASE_URL"], "postgres://") {
			t.Fatal("DATABASE_URL must select PostgreSQL when both services are requested")
		}
		if !slices.Contains(b.credentialNames(), "MYSQL_PASSWORD") || !slices.Contains(b.credentialNames(), "MYSQL_URL") {
			t.Fatal("MySQL credentials must be labelled for later app output redaction")
		}
		cfg, host, _ := serviceSpec(s, nil, "net", nil, Policy{})
		if cfg.User != "mysql" || !slices.Contains(cfg.Env, "MYSQL_ROOT_HOST=localhost") || host.Memory != 1<<30 || host.NanoCPUs != 1e9 || !host.ReadonlyRootfs || len(host.PortBindings) != 0 {
			t.Fatal("MySQL service restrictions missing")
		}
	}
}
