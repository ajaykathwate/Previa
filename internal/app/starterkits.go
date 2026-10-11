package app

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/starterkits"
)

// newStarterKits wires the Starter kits page. The service versions on offer
// come from the running workers, or Previa's default catalog when none that
// builds is running, and every kit is checked with the parser a build uses.
func newStarterKits(users starterkits.UserSession, caps starterkits.Capabilities, logger *slog.Logger) (*starterkits.Handler, error) {
	defaults, err := config.ParseServiceCatalog("")
	if err != nil {
		return nil, fmt.Errorf("read the default service catalog: %w", err)
	}
	fallback := starterkits.Offer{Postgres: defaults.Versions("postgres"), Redis: defaults.Versions("redis")}
	return starterkits.NewHandler(users, caps, fallback, validateKit, logger), nil
}

// validateKit checks a generated .previewrc.yaml as a build would, against
// the versions offered.
func validateKit(data []byte, offer starterkits.Offer) []starterkits.Problem {
	platform := repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: offeredCatalog{"postgres": offer.Postgres, "redis": offer.Redis}}
	_, err := repoconfig.ParseFor(data, platform)
	if err == nil {
		return nil
	}
	var invalid *repoconfig.ValidationError
	if !errors.As(err, &invalid) {
		return []starterkits.Problem{{Field: "config", Message: "Previa could not read the generated .previewrc.yaml."}}
	}
	problems := make([]starterkits.Problem, 0, len(invalid.Issues))
	for _, issue := range invalid.Issues {
		problems = append(problems, starterkits.Problem{Field: issue.Field, Message: issue.Message, Line: issue.Line})
	}
	return problems
}

// offeredCatalog stands in for the worker's catalog: the parser only asks
// which versions exist, never pulls an image.
type offeredCatalog map[string][]string

func (c offeredCatalog) Versions(kind string) []string { return c[kind] }

func (c offeredCatalog) Image(kind, version string) (string, bool) {
	return kind + ":" + version, slices.Contains(c[kind], version)
}
