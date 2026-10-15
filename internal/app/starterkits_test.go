package app

import (
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/starterkits"
)

// Every default kit passes the parser a build uses, and a config it rejects
// comes back as problems for the page to list.
func TestValidateKitUsesTheBuildsRules(t *testing.T) {
	offer := starterkits.Offer{Postgres: []string{"17"}, Redis: []string{"8"}}
	for _, s := range starterkits.Stacks() {
		c := starterkits.Fresh(s.ID)
		c.Postgres, c.Redis = "17", "8"
		if problems := validateKit([]byte(starterkits.Generate(c, offer).Config), offer); len(problems) > 0 {
			t.Errorf("%s: %+v", s.ID, problems)
		}
	}
	problems := validateKit([]byte("port: 3000\nservices:\n  postgres: 12\n"), offer)
	if len(problems) == 0 || problems[0].Message == "" {
		t.Fatalf("a version the server does not offer is a problem, got %+v", problems)
	}
	if problems := validateKit([]byte("port: [3000"), offer); len(problems) != 1 {
		t.Fatalf("unreadable YAML is one problem, got %+v", problems)
	}
}

func TestKitBuildTimeoutUsesLiveWorkerCeiling(t *testing.T) {
	offer := starterkits.Offer{BuildTimeout: 30 * time.Minute}
	if problems := validateKit([]byte("build_timeout: 20m"), offer); len(problems) != 0 {
		t.Fatalf("a timeout permitted by the worker was refused: %+v", problems)
	}
	if problems := validateKit([]byte("build_timeout: 31m"), offer); len(problems) != 1 || problems[0].Field != "build_timeout" {
		t.Fatalf("a timeout above the worker limit was accepted: %+v", problems)
	}
}
