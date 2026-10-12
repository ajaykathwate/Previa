package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/appoutput"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
	"github.com/ajaykathwatee/previa/internal/idle"
	"github.com/ajaykathwatee/previa/internal/outbound"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
	"github.com/riverqueue/river"
)

// GitHubApp is the App access the worker needs: installation clients for
// PRs, archives, comments and checks, and the App itself (its ID and slug).
// *githubapp.Client implements it.
type GitHubApp interface {
	InstallationClient(id int64) (*github.Client, error)
	App(ctx context.Context) (*github.App, error)
}

// Queue sizes per worker process. Builds are additionally bounded across all
// workers by MAX_CONCURRENT_BUILDS (environments build slots).
const (
	teardownWorkers     = 2
	coordinationWorkers = 4
	routeWorkers        = 2
	reportWorkers       = 4
	cleanupWorkers      = 2
	operationLease      = 4 * time.Minute
)

// operationTimeouts bound each kind of external operation; all stay below
// the operation lease, and reports below the reporter's lease.
var operationTimeouts = map[environments.OperationKind]time.Duration{
	environments.ActivateRoute:   2 * time.Minute,
	environments.RemoveRoute:     2 * time.Minute,
	environments.CleanupArtifact: 90 * time.Second,
	environments.ReportStatus:    time.Minute,
}

// pipelineInput is everything the worker's pipeline is built from. Tests
// point GitHub and archive downloads at fakes.
type pipelineInput struct {
	cfg            config.WorkerConfig
	pool           *pgxpool.Pool
	producer       *river.Client[pgx.Tx]
	docker         *client.Client // nil: no Docker client was created
	github         GitHubApp      // nil: the GitHub App is not configured
	archiveOptions []source.ArchiveOption
	logger         *slog.Logger
}

// pipeline is what one worker process runs: its capabilities, its River
// registrations and what must happen once before it starts working.
type pipeline struct {
	capabilities workers.Capabilities
	register     []func(*river.Config)
	startup      []func(context.Context) error
	// background loops run beside River until the worker stops.
	background  []func(context.Context)
	unavailable []string      // why capabilities are off, for the startup log
	checks      []httpx.Check // readiness of the dependencies it uses
}

// buildPipeline composes discovery, execution (source → config → image →
// candidate → route), reporting and housekeeping from real, validated
// dependencies. A capability whose dependencies are missing is left out
// entirely and named in unavailable: no worker is registered for it, so its
// jobs wait durably instead of being handled by a no-op.
func buildPipeline(ctx context.Context, in pipelineInput) (*pipeline, error) {
	p := &pipeline{}
	cfg, log := in.cfg, in.logger
	// Log retention needs only the database.
	p.register = append(p.register, func(rc *river.Config) {
		river.AddWorker(rc.Workers, &buildlogs.PruneWorker{Store: buildlogs.NewStore(in.pool).WithRetention(cfg.BuildLogRetention()), Logger: log})
		rc.Queues[buildlogs.PruneQueue] = river.QueueConfig{MaxWorkers: 1}
		rc.PeriodicJobs = append(rc.PeriodicJobs, buildlogs.PeriodicPrune())
	})
	if in.github == nil {
		p.unavailable = append(p.unavailable, "GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH are not set: no discovery, builds or PR reports")
		return p, nil
	}
	upstream := pullrequests.NewGitHubSource(in.github)
	if !cfg.RoutingConfigured() {
		// Discovery and mergeability only; webhooks and requests still record
		// durable intent and wake-ups, which wait for an execution worker.
		discovery := pullrequests.NewSyncService(in.pool, in.producer, upstream, environments.NewBroker(in.pool, in.producer))
		p.addDiscovery(discovery)
		p.unavailable = append(p.unavailable, "PREVIEW_BASE_DOMAIN is not set: previews cannot be published, so this worker builds nothing")
		return p, nil
	}
	if in.docker == nil {
		return nil, errors.New("builds need Docker: start Docker or unset PREVIEW_BASE_DOMAIN to run without builds")
	}
	if err := p.addExecution(ctx, in, upstream); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *pipeline) addDiscovery(sync *pullrequests.SyncService) {
	p.capabilities.Discovery = true
	p.register = append(p.register, func(rc *river.Config) {
		river.AddWorker(rc.Workers, &pullrequests.SyncWorker{Service: sync})
		river.AddWorker(rc.Workers, &pullrequests.MergeabilityWorker{Service: sync})
		rc.Queues[pullrequests.SyncQueue] = river.QueueConfig{MaxWorkers: 2}
	})
}

// addExecution adds the full pipeline: builds, previews, routing, reports,
// teardown and housekeeping. Every dependency is checked at startup: a
// misconfigured worker refuses to start rather than fail builds.
func (p *pipeline) addExecution(ctx context.Context, in pipelineInput, upstream *pullrequests.GitHubSource) error {
	x, err := newExecution(ctx, in, upstream)
	if err != nil {
		return err
	}
	cfg := in.cfg
	if !cfg.SecretsConfigured() {
		p.unavailable = append(p.unavailable, "SECRETS_ENCRYPTION_KEYS is not set: repositories with secrets cannot start previews")
	}
	p.addDiscovery(x.sync)
	p.capabilities.Execution, p.capabilities.Routing, p.capabilities.Reporting, p.capabilities.Housekeeping = true, true, true, true
	p.capabilities.Services = x.catalog.Offers()
	p.capabilities.BuildTimeout = cfg.BuildTimeout
	if x.catalog.Empty() {
		p.unavailable = append(p.unavailable, "PREVIEW_SERVICE_CATALOG is none: previews cannot request managed PostgreSQL, Redis or MySQL")
	}
	if cfg.BuildsPaused {
		// The kill switch: everything stays registered so teardown, cleanup,
		// routing, reports and housekeeping keep working; builds wait.
		x.service.Paused()
		p.capabilities.Execution, p.capabilities.Paused = false, true
		p.unavailable = append(p.unavailable, "BUILDS_PAUSED is set: new builds wait; teardown, cleanup, routing, reports and housekeeping continue")
	}
	docker := in.docker
	visits := &routing.VisitHealth{}
	sleeper, err := idle.New(in.pool, x.runtime, x.controller, visits, cfg.PreviewIdleSleep, in.logger)
	if err != nil {
		return err
	}
	p.checks = append(p.checks, httpx.Check{Name: "docker", Run: func(ctx context.Context) error {
		_, err := docker.Ping(ctx, client.PingOptions{})
		return err
	}})
	p.register = append(p.register, func(rc *river.Config) {
		environments.RegisterWorkers(rc.Workers, x.service, x.broker)
		river.AddWorker(rc.Workers, x.operations)
		river.AddWorker(rc.Workers, &environments.TeardownSweepWorker{Sweeper: x.sweeper})
		rc.Queues[environments.BuildQueue] = river.QueueConfig{MaxWorkers: cfg.MaxConcurrentBuilds}
		rc.Queues[environments.TeardownQueue] = river.QueueConfig{MaxWorkers: teardownWorkers}
		rc.Queues[environments.CoordinationQueue] = river.QueueConfig{MaxWorkers: coordinationWorkers}
		rc.Queues[environments.RouteOperationQueue] = river.QueueConfig{MaxWorkers: routeWorkers}
		rc.Queues[environments.ReportOperationQueue] = river.QueueConfig{MaxWorkers: reportWorkers}
		rc.Queues[environments.CleanupOperationQueue] = river.QueueConfig{MaxWorkers: cleanupWorkers}
		rc.Queues[environments.TeardownSweepQueue] = river.QueueConfig{MaxWorkers: 1}
		x.keeper.Register(rc)
		outbound.Register(rc, x.runtime)
		river.AddWorker(rc.Workers, &appoutput.FollowWorker{Store: appoutput.NewStore(in.pool), Source: x.runtime, Logger: in.logger})
		rc.Queues[appoutput.Queue] = river.QueueConfig{MaxWorkers: appoutput.MaxFollowers}
		sleeper.Register(rc)
		river.AddWorker(rc.Workers, &routing.SyncWorker{Controller: x.controller})
		rc.Queues[routing.SyncQueue] = river.QueueConfig{MaxWorkers: 1}
	})
	p.startup = append(p.startup,
		func(ctx context.Context) error {
			if _, err := x.gateway.Ensure(ctx); err != nil {
				return fmt.Errorf("start the egress proxy (make egress-image): %w", err)
			}
			return nil
		},
		// The router's file always reflects the accepted table before work starts.
		x.controller.Sync,
		func(ctx context.Context) error {
			warnCapacity(ctx, docker, cfg, in.logger)
			return nil
		},
	)
	crashLog := runtime.CrashLogFunc(func(ctx context.Context, environmentID, generation, revision int64, redactor *secrets.Redactor) (runtime.LogSink, error) {
		return x.logs.OpenFinished(ctx, environmentID, generation, revision, redactor)
	})
	p.background = append(p.background, func(ctx context.Context) {
		x.runtime.WatchCrashes(ctx, runtime.NewTargetStore(in.pool), crashLog)
	})
	router, err := routing.NewTraefik(docker, cfg.RouterContainer, cfg.RouterAPIAddress)
	if err != nil {
		return err
	}
	p.background = append(p.background, func(ctx context.Context) {
		routing.WatchVisits(ctx, router, cfg.MetricsAddress(), x.repo, visits, in.logger)
	})
	return nil
}

func newHousekeeper(cfg config.WorkerConfig, pool *pgxpool.Pool, log *slog.Logger, repo *environments.Repository, broker *environments.Broker,
	sweeper *environments.TeardownSweeper, controller *routing.Controller, rt *runtime.Runtime, sync *pullrequests.SyncService,
	builder *builds.Builder, workspaces *source.Workspaces) (*housekeeping.Housekeeper, error) {
	settings := housekeeping.SettingsFrom(cfg.HousekeepingConfig, cfg.BuildTimeout)
	keeper, err := housekeeping.NewHousekeeper(pool, log)
	if err != nil {
		return nil, err
	}
	if keeper.Recovery, err = housekeeping.NewRecovery(broker, repo, sweeper, controller, log, settings.BatchSize); err != nil {
		return nil, err
	}
	if keeper.Resources, err = housekeeping.NewResources(repo, rt, pool, log, settings); err != nil {
		return nil, err
	}
	if keeper.PullRequests, err = housekeeping.NewPullRequests(pool, sync, log, settings); err != nil {
		return nil, err
	}
	if keeper.Expiry, err = housekeeping.NewExpiry(pool, repo, log, settings); err != nil {
		return nil, err
	}
	if keeper.Records, err = housekeeping.NewRecords(pool, log, settings); err != nil {
		return nil, err
	}
	if keeper.Prune, err = housekeeping.NewPrune(builder, workspaces, pool, log, settings); err != nil {
		return nil, err
	}
	return keeper, nil
}
