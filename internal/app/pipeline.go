package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/reporting"
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
	unavailable  []string      // why capabilities are off, for the startup log
	checks       []httpx.Check // readiness of the dependencies it uses
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
		river.AddWorker(rc.Workers, &buildlogs.PruneWorker{Store: buildlogs.NewStore(in.pool), Logger: log})
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

// addExecution composes the full pipeline. Every dependency is checked here,
// at startup: a misconfigured worker refuses to start rather than fail builds.
func (p *pipeline) addExecution(ctx context.Context, in pipelineInput, upstream *pullrequests.GitHubSource) error {
	cfg, log, pool := in.cfg, in.logger, in.pool
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		return err
	}
	identity, err := in.github.App(ctx)
	if err != nil {
		return fmt.Errorf("read the GitHub App's identity for PR reports: %w", err)
	}
	gateway, err := egress.NewGateway(in.docker, cfg.EgressImage)
	if err != nil {
		return err
	}
	var keyring *secrets.Keyring
	if cfg.SecretsConfigured() {
		if keyring, err = secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion); err != nil {
			return fmt.Errorf("configure repository secrets: %w", err)
		}
	} else {
		p.unavailable = append(p.unavailable, "SECRETS_ENCRYPTION_KEYS is not set: repositories with secrets cannot start previews")
	}
	provider, err := secrets.NewProvider(secrets.NewRepository(pool), keyring)
	if err != nil {
		return err
	}
	archives, err := source.NewGitHubArchives(in.github, in.archiveOptions...)
	if err != nil {
		return err
	}
	workspaces, err := source.NewWorkspaces(cfg.SourceWorkspaceRoot)
	if err != nil {
		return err
	}
	preparer, err := source.NewPreparer(archives, workspaces, source.DefaultLimits(), repoconfig.DefaultLimits())
	if err != nil {
		return err
	}
	builder, err := builds.NewBuilder(in.docker, gateway, builds.ConfigFrom(cfg.BuilderConfig, instance, cfg.BuildTimeout), log)
	if err != nil {
		return err
	}
	logStore := buildlogs.NewStore(pool)
	rt, err := runtime.New(runtime.Parts{
		Instance: instance, Admission: runtime.NewAdmissionLock(pool), Docker: in.docker, Egress: gateway,
		Targets: runtime.NewTargetStore(pool), Sources: preparer, Builder: builder, Secrets: provider,
		Logs: func(ctx context.Context, attempt environments.Attempt, redactor *secrets.Redactor) (runtime.LogSink, error) {
			return logStore.Open(ctx, buildlogs.Binding{EnvironmentID: attempt.EnvironmentID, Epoch: attempt.LogEpoch}, redactor, buildlogs.DefaultLimits())
		},
	}, runtime.PolicyFrom(cfg.PreviewConfig), log)
	if err != nil {
		return err
	}
	operations, err := environments.NewOperationBroker(in.producer)
	if err != nil {
		return err
	}
	repo := environments.NewRepository(pool, operations).WithHostnames(routing.NewLabel).WithReports().
		WithLifetime(cfg.PreviewLifetime()).WithBuildSlots(cfg.MaxConcurrentBuilds)
	settings := routing.Settings{
		BaseDomain: cfg.PreviewBaseDomain, EntryPoint: cfg.RouterEntryPoint,
		UnavailableURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + routing.UnavailablePath,
	}
	traefik, err := routing.NewTraefik(in.docker, cfg.RouterContainer, cfg.RouterAPIAddress)
	if err != nil {
		return err
	}
	file, err := routing.NewFile(cfg.RouterDynamicDir)
	if err != nil {
		return err
	}
	backends := routing.BackendFunc(func(ctx context.Context, c routing.Candidate) (routing.Backend, error) {
		endpoint, err := rt.Endpoint(ctx, runtime.Expected(c))
		if err != nil {
			return routing.Backend{}, runtime.AsOperationFailure(err)
		}
		return routing.Backend(endpoint), nil
	})
	controller, err := routing.NewController(repo, backends, traefik, file, settings, log)
	if err != nil {
		return err
	}
	api, err := reporting.NewGitHubAPI(in.github, identity.GetID(), identity.GetSlug())
	if err != nil {
		return err
	}
	reporter, err := reporting.NewReporter(pool, api, reporting.Settings{
		AppBaseURL: strings.TrimSuffix(cfg.AppBaseURL, "/"), PreviewBaseDomain: cfg.PreviewBaseDomain,
	}, log)
	if err != nil {
		return err
	}
	executors := environments.Executors{
		environments.ActivateRoute: controller, environments.RemoveRoute: controller,
		environments.CleanupArtifact: rt.CleanupExecutor(), environments.ReportStatus: reporter,
	}
	operationWorker, err := environments.NewOperationWorker(repo, executors, log, operationLease, operationTimeouts)
	if err != nil {
		return err
	}
	service, err := environments.NewService(repo, rt, log, cfg.BuildTimeout)
	if err != nil {
		return err
	}
	broker, err := environments.NewExecutionBroker(pool, in.producer, service, operationWorker, nil)
	if err != nil {
		return err
	}
	sync := pullrequests.NewSyncService(pool, in.producer, upstream, broker)
	service.WithVerifier(sync).WithHeadroom(rt).WithBuildLogs(logStore)
	sweeper, err := environments.NewTeardownSweeper(repo, rt, log, 50)
	if err != nil {
		return err
	}
	keeper, err := newHousekeeper(cfg, pool, log, repo, broker, sweeper, controller, rt, sync, builder, workspaces)
	if err != nil {
		return err
	}
	p.addDiscovery(sync)
	p.capabilities.Execution, p.capabilities.Routing, p.capabilities.Reporting, p.capabilities.Housekeeping = true, true, true, true
	if cfg.BuildsPaused {
		// The kill switch: everything stays registered so teardown, cleanup,
		// routing, reports and housekeeping keep working; builds wait.
		service.Paused()
		p.capabilities.Execution, p.capabilities.Paused = false, true
		p.unavailable = append(p.unavailable, "BUILDS_PAUSED is set: new builds wait; teardown, cleanup, routing, reports and housekeeping continue")
	}
	docker := in.docker
	p.checks = append(p.checks, httpx.Check{Name: "docker", Run: func(ctx context.Context) error {
		_, err := docker.Ping(ctx, client.PingOptions{})
		return err
	}})
	p.register = append(p.register, func(rc *river.Config) {
		environments.RegisterWorkers(rc.Workers, service, broker)
		river.AddWorker(rc.Workers, operationWorker)
		river.AddWorker(rc.Workers, &environments.TeardownSweepWorker{Sweeper: sweeper})
		rc.Queues[environments.BuildQueue] = river.QueueConfig{MaxWorkers: cfg.MaxConcurrentBuilds}
		rc.Queues[environments.TeardownQueue] = river.QueueConfig{MaxWorkers: teardownWorkers}
		rc.Queues[environments.CoordinationQueue] = river.QueueConfig{MaxWorkers: coordinationWorkers}
		rc.Queues[environments.RouteOperationQueue] = river.QueueConfig{MaxWorkers: routeWorkers}
		rc.Queues[environments.ReportOperationQueue] = river.QueueConfig{MaxWorkers: reportWorkers}
		rc.Queues[environments.CleanupOperationQueue] = river.QueueConfig{MaxWorkers: cleanupWorkers}
		rc.Queues[environments.TeardownSweepQueue] = river.QueueConfig{MaxWorkers: 1}
		keeper.Register(rc)
	})
	p.startup = append(p.startup,
		func(ctx context.Context) error {
			if _, err := gateway.Ensure(ctx); err != nil {
				return fmt.Errorf("start the egress proxy (make egress-image): %w", err)
			}
			return nil
		},
		// The router's file always reflects the accepted table before work starts.
		controller.Sync,
	)
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
	if keeper.Prune, err = housekeeping.NewPrune(builder, workspaces, pool, log, settings); err != nil {
		return nil, err
	}
	return keeper, nil
}
