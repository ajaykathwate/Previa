package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/onboarding"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// checkRules versions what a setup check verifies; a change makes earlier
// results stale.
const checkRules = "1"

// onboardingPlatform is what setup checks validate against: the services and
// build deadline the running build workers offer, or Previa's default catalog
// when none that builds is running, exactly as the Starter kits do.
type onboardingPlatform struct {
	caps     *workers.Reader
	defaults config.ServiceCatalog
}

func newOnboardingPlatform(pool *pgxpool.Pool) (onboardingPlatform, error) {
	defaults, err := config.ParseServiceCatalog("")
	if err != nil {
		return onboardingPlatform{}, fmt.Errorf("read the default service catalog: %w", err)
	}
	return onboardingPlatform{caps: workers.NewReader(pool), defaults: defaults}, nil
}

func (p onboardingPlatform) Platform(ctx context.Context) (repoconfig.Platform, string, error) {
	caps, err := p.caps.Current(ctx)
	if err != nil {
		return repoconfig.Platform{}, "", fmt.Errorf("read the services on offer: %w", err)
	}
	offer := offeredCatalog{}
	known := caps.Execution || caps.Paused
	timeout := caps.BuildTimeout
	if known {
		for _, entry := range caps.Services { // "postgres 17, 16"
			kind, versions, _ := strings.Cut(entry, " ")
			offer[kind] = strings.Split(versions, ", ")
		}
	} else {
		timeout = 0
		for _, kind := range repoconfig.ServiceKinds {
			offer[kind] = p.defaults.Versions(kind)
		}
	}
	platform := repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: offer, BuildTimeout: timeout}
	return platform, platformFingerprint(offer, timeout.String(), known), nil
}

// platformFingerprint changes whenever the offered services, the build
// deadline or the check rules change.
func platformFingerprint(offer offeredCatalog, timeout string, known bool) string {
	kinds := make([]string, 0, len(offer))
	for kind, versions := range offer {
		kinds = append(kinds, kind+"="+strings.Join(versions, ","))
	}
	slices.Sort(kinds)
	sum := sha256.Sum256([]byte("rules " + checkRules + "|" + strings.Join(kinds, ";") + "|timeout " + timeout + "|known " + strconv.FormatBool(known)))
	return hex.EncodeToString(sum[:16])
}

// onboardingSecrets lists secret names through the secrets feature, which
// lets only repository managers read them.
type onboardingSecrets struct{ service *secrets.Service }

func (s onboardingSecrets) Names(ctx context.Context, userID, repoID int64) ([]string, error) {
	list, err := s.service.List(ctx, userID, repoID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, secret := range list {
		names = append(names, secret.Name)
	}
	return names, nil
}

func (s onboardingSecrets) Configured() bool { return s.service.Configured() }

// newOnboarding wires the Setup tab: check requests go on the queue for a
// worker; pages read worker capabilities, secrets and the offered platform.
func newOnboarding(cfg config.ServerConfig, pool *pgxpool.Pool, producer *river.Client[pgx.Tx], caps *workers.Reader, secretsService *secrets.Service, live onboarding.Cursor, users onboarding.UserSession, logger *slog.Logger) (*onboarding.Service, *onboarding.Handler, error) {
	platform, err := newOnboardingPlatform(pool)
	if err != nil {
		return nil, nil, err
	}
	service, err := onboarding.NewService(pool, producer, platform, caps, onboardingSecrets{service: secretsService}, onboarding.Options{
		PreviewDomain: cfg.PreviewBaseDomain, Production: cfg.Production(), Discover: cfg.GitHubAppID != 0,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("configure repository setup: %w", err)
	}
	return service, onboarding.NewHandler(service, users, live, logger), nil
}

// addSetupChecks lets a worker with the GitHub App run setup checks: a few
// GitHub reads each, no Docker or preview domain needed.
func (p *pipeline) addSetupChecks(in pipelineInput) error {
	files, err := onboarding.NewGitHubFiles(in.github)
	if err != nil {
		return err
	}
	platform, err := newOnboardingPlatform(in.pool)
	if err != nil {
		return err
	}
	runner, err := onboarding.NewRunner(in.pool, files, platform, in.logger)
	if err != nil {
		return err
	}
	p.register = append(p.register, func(rc *river.Config) { onboarding.Register(rc, runner) })
	return nil
}
