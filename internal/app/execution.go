package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"

	"github.com/ajaykathwatee/previa/internal/accounts"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/outbound"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/reporting"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/usage"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/google/go-github/v91/github"
)

// execution is the composed pipeline that turns a build request into a
// routed preview (source -> config -> image -> candidate -> route), with the
// reports, teardown and housekeeping around it.
type execution struct {
	catalog    config.ServiceCatalog
	gateway    *egress.Gateway
	runtime    *runtime.Runtime
	controller *routing.Controller
	service    *environments.Service
	broker     *environments.Broker
	operations *environments.OperationWorker
	sweeper    *environments.TeardownSweeper
	keeper     *housekeeping.Housekeeper
	sync       *pullrequests.SyncService
	logs       *buildlogs.Store
	repo       *environments.Repository
	usage      *usage.Store
	accounts   *accounts.Service
	// testData runs baseline refreshes; nil without baseline storage.
	testData *sampledata.Jobs
}

func newExecution(ctx context.Context, in pipelineInput, upstream *pullrequests.GitHubSource) (*execution, error) {
	cfg, log, pool := in.cfg, in.logger, in.pool
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		return nil, err
	}
	identityCtx, cancelIdentity := context.WithTimeout(ctx, 10*time.Second)
	identity, err := in.github.App(identityCtx)
	cancelIdentity()
	if err != nil {
		if ctx.Err() != nil || cfg.GitHubAppID <= 0 {
			return nil, fmt.Errorf("read GitHub App identity: %w", err)
		}
		// Cleanup must restart during a GitHub outage. The configured immutable
		// App ID still proves comment ownership; login-based fallback waits.
		log.WarnContext(ctx, "GitHub App identity unavailable; using configured App ID so local cleanup can start", "error", err)
		appID := cfg.GitHubAppID
		identity = &github.App{ID: &appID}
	}
	x := &execution{usage: usage.NewStore(pool, cfg.BuildMinutesPerAccountDay, cfg.BuildsPerAccount).WithPreviewPolicy(cfg.PreviewLifetime(), cfg.PreviewIdleSleep, cfg.BuildLogRetention())}
	if x.gateway, err = newEgressGateway(in); err != nil {
		return nil, err
	}
	provider, keyring, err := newSecretsProvider(in)
	if err != nil {
		return nil, err
	}
	testDataStore, err := sampledata.NewStore(pool, in.producer, environments.BuildQueue)
	if err != nil {
		return nil, err
	}
	var baselines *sampledata.Storage
	if keyring != nil {
		if baselines, err = sampledata.NewStorage(cfg.PreviewBaselineDir, keyring, int64(cfg.PreviewBaselineMaxSize)); err != nil {
			return nil, fmt.Errorf("prepare PREVIEW_BASELINE_DIR: %w", err)
		}
	}
	if x.catalog, err = cfg.ServiceCatalog(); err != nil {
		return nil, err
	}
	sources, err := source.NewWorkspaces(cfg.SourceWorkspaceRoot)
	if err != nil {
		return nil, err
	}
	preparer, err := newPreparer(in, sources, x.catalog)
	if err != nil {
		return nil, err
	}
	builder, err := builds.NewBuilder(in.docker, x.gateway, builds.ConfigFrom(cfg.BuilderConfig, instance, cfg.BuildTimeout), log)
	if err != nil {
		return nil, err
	}
	x.logs = buildlogs.NewStore(pool)
	if x.runtime, err = newRuntime(in, instance, x.gateway, preparer, builder, provider, x.logs, startingData{store: testDataStore, storage: baselines}); err != nil {
		return nil, err
	}
	operations, err := environments.NewOperationBroker(in.producer)
	if err != nil {
		return nil, err
	}
	x.repo = environments.NewRepository(pool, operations).WithHostnames(routing.NewLabel).WithReports().
		WithLifetime(cfg.PreviewLifetime()).WithBuildSlots(cfg.MaxConcurrentBuilds).
		WithAccountLimits(environments.AccountLimits{BuildsPerAccount: cfg.BuildsPerAccount, MinutesPerDay: cfg.BuildMinutesPerAccountDay}).
		WithUsage(x.usage).
		WithPublications(publicationActivity{recorder: activity.NewRecorder()})
	repo := x.repo
	if x.controller, err = newRouteController(in, repo, x.runtime); err != nil {
		return nil, err
	}
	reporter, err := newReporter(in, identity)
	if err != nil {
		return nil, err
	}
	executors := environments.Executors{
		environments.ActivateRoute: x.controller, environments.RemoveRoute: x.controller,
		environments.CleanupArtifact: x.runtime.CleanupExecutor(), environments.ReportStatus: reporter,
	}
	if x.operations, err = environments.NewOperationWorker(repo, executors, log, operationLease, operationTimeouts); err != nil {
		return nil, err
	}
	if x.service, err = environments.NewService(repo, x.runtime, log, cfg.BuildTimeout); err != nil {
		return nil, err
	}
	if x.broker, err = environments.NewExecutionBroker(pool, in.producer, x.service, x.operations, nil); err != nil {
		return nil, err
	}
	x.sync = pullrequests.NewSyncService(pool, in.producer, upstream, x.broker)
	x.service.WithVerifier(x.sync).WithHeadroom(x.runtime).WithBuildLogs(x.logs)
	if x.sweeper, err = environments.NewTeardownSweeper(repo, x.runtime, log, 50); err != nil {
		return nil, err
	}
	if x.keeper, err = newHousekeeper(cfg, pool, log, repo, x.broker, x.sweeper, x.controller, x.runtime, x.sync, builder, sources); err != nil {
		return nil, err
	}
	if baselines != nil {
		if x.testData, err = newTestDataJobs(in, testDataStore, baselines, x); err != nil {
			return nil, err
		}
	}
	dataLifecycle := sampledata.NewLifecycle(testDataStore, baselines, x.runtime)
	workspaceLifecycle := workspaces.NewService(workspaces.NewRepository(pool), nil, nil, nil)
	supportLifecycle := newSupport(pool, cfg.CustomerConfig)
	api, err := reporting.NewGitHubAPI(in.github, identity.GetID(), identity.GetSlug())
	if err != nil {
		return nil, err
	}
	x.accounts = accounts.NewService(pool).WithPreviews(repositoryLifecycle{environments.NewLifecycle(repo, x.broker, x.runtime), dataLifecycle}).
		WithCleaners(dataLifecycle, housekeeping.NewLifecycle(pool, builder, sources)).WithComments(reporting.NewDisconnector(pool, api)).
		WithRepositoryErasers(installations.NewRepository(pool)).WithInstallations(installations.NewRepository(pool)).
		WithWorkspaces(workspaceLifecycle, supportLifecycle).WithAccountFences(workspaceLifecycle).WithAccountErasers(supportLifecycle, workspaceLifecycle, auth.NewRepository(pool))
	return x, nil
}

// newTestDataJobs runs baseline refreshes on the build queue, and the
// dispatcher, daily freshness check and cleanup of persistent test data.
func newTestDataJobs(in pipelineInput, store *sampledata.Store, storage *sampledata.Storage, x *execution) (*sampledata.Jobs, error) {
	branches, err := sampledata.NewGitHubBranches(in.github)
	if err != nil {
		return nil, err
	}
	runner, err := sampledata.NewRunner(store, sampledata.RunnerParts{
		Builder: baselineBuilder{runtime: x.runtime, logs: x.logs, storage: storage, logger: in.logger}, Branches: branches,
		Admission: x.repo, Usage: x.usage, Activity: testDataActivity{recorder: activity.NewRecorder()}, Files: storage,
		Timeout: in.cfg.BuildTimeout, MinInterval: in.cfg.PreviewBaselineMinInterval, Paused: in.cfg.BuildsPaused,
	}, in.logger)
	if err != nil {
		return nil, err
	}
	return sampledata.NewJobs(store, sampledata.JobParts{
		Runner: runner, Branches: branches, Directory: storage, Leftovers: x.runtime,
		Offered: func(kind, version string) bool { _, ok := x.catalog.Image(kind, version); return ok },
	}, in.logger)
}

// newEgressGateway prepares the egress proxy. The worker owns its
// owner-approved outbound forwarding and exports its connection metrics.
func newEgressGateway(in pipelineInput) (*egress.Gateway, error) {
	gateway := in.gateway
	if gateway == nil {
		var err error
		gateway, err = egress.NewGateway(in.docker, in.cfg.EgressImage)
		if err != nil {
			return nil, err
		}
	}
	outboundPorts, err := in.cfg.OutboundPorts()
	if err != nil {
		return nil, err
	}
	if err := gateway.ManageForwarding(outboundPorts); err != nil {
		return nil, err
	}
	metrics.SetEgressSource(func(ctx context.Context) (map[string]int64, int64, int64, error) {
		s, err := gateway.Stats(ctx)
		return s.Connections, s.BytesUp, s.BytesDown, err
	})
	return gateway, nil
}

// newSecretsProvider decrypts repository secrets for previews and returns
// the keyring, which also encrypts test-data baselines. Without a key it
// still runs: previews of repositories with secrets do not start, and no
// baseline is stored.
func newSecretsProvider(in pipelineInput) (*secrets.Provider, *secrets.Keyring, error) {
	var keyring *secrets.Keyring
	if in.cfg.SecretsConfigured() {
		var err error
		if keyring, err = secrets.ParseKeyring(in.cfg.SecretsEncryptionKeys, in.cfg.SecretsActiveKeyVersion); err != nil {
			return nil, nil, fmt.Errorf("configure repository secrets: %w", err)
		}
	}
	provider, err := secrets.NewProvider(secrets.NewRepository(in.pool), keyring)
	return provider, keyring, err
}

// newPreparer downloads and validates a commit's source and configuration,
// against the worker's service catalog.
func newPreparer(in pipelineInput, workspaces *source.Workspaces, catalog config.ServiceCatalog) (*source.Preparer, error) {
	archives, err := source.NewGitHubArchives(in.github, in.archiveOptions...)
	if err != nil {
		return nil, err
	}
	return source.NewPreparer(archives, workspaces, source.DefaultLimits(), repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: catalog, BuildTimeout: in.cfg.BuildTimeout})
}

func newRuntime(in pipelineInput, instance string, gateway *egress.Gateway, preparer *source.Preparer, builder *builds.Builder, provider *secrets.Provider, logStore *buildlogs.Store, testData runtime.StartingData) (*runtime.Runtime, error) {
	targets := runtime.NewTargetStore(in.pool).WithAuthorization(workspaces.NewService(workspaces.NewRepository(in.pool), nil, workspaceGitHub{app: in.github}, nil))
	return runtime.New(runtime.Parts{
		Instance: instance, Admission: runtime.NewAdmissionLock(in.pool), Docker: in.docker, Egress: gateway,
		Targets: targets, Sources: preparer, Builder: builder, Secrets: provider,
		Components: targets, Destinations: outbound.NewRepository(in.pool, nil), Outbound: targets,
		StartingData: testData, Deployments: targets,
		Logs: func(ctx context.Context, attempt environments.Attempt, redactor *secrets.Redactor) (runtime.LogSink, error) {
			return logStore.Open(ctx, buildlogs.Binding{EnvironmentID: attempt.EnvironmentID, Epoch: attempt.LogEpoch}, redactor, buildlogs.DefaultLimits())
		},
	}, runtime.PolicyFrom(in.cfg.PreviewConfig), in.logger)
}

// newRouteController publishes verified candidates on the router.
func newRouteController(in pipelineInput, repo *environments.Repository, rt *runtime.Runtime) (*routing.Controller, error) {
	cfg := in.cfg
	settings := routing.Settings{
		BaseDomain: cfg.PreviewBaseDomain, EntryPoint: cfg.RouterEntryPoint,
		UnavailableURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + routing.UnavailablePath,
		WakeURL:        strings.TrimSuffix(cfg.AppBaseURL, "/") + routing.WakePath,
		AccessURL:      cfg.PreviewAccessURL,
	}
	traefik, err := routing.NewTraefik(in.docker, cfg.RouterContainer, cfg.RouterAPIAddress)
	if err != nil {
		return nil, err
	}
	file, err := routing.NewFile(cfg.RouterDynamicDir)
	if err != nil {
		return nil, err
	}
	backends := routing.BackendFunc(func(ctx context.Context, c routing.Candidate) (routing.Backend, error) {
		endpoint, err := rt.Endpoint(ctx, runtime.Expected(c))
		if err != nil {
			return routing.Backend{}, runtime.AsOperationFailure(err)
		}
		return routing.Backend(endpoint), nil
	})
	return routing.NewController(repo, backends, traefik, file, settings, in.logger)
}

// newReporter writes PR comments and checks as the GitHub App.
func newReporter(in pipelineInput, identity *github.App) (*reporting.Reporter, error) {
	api, err := reporting.NewGitHubAPI(in.github, identity.GetID(), identity.GetSlug())
	if err != nil {
		return nil, err
	}
	return reporting.NewReporter(in.pool, api, reporting.Settings{
		AppBaseURL: strings.TrimSuffix(in.cfg.AppBaseURL, "/"), PreviewBaseDomain: in.cfg.PreviewBaseDomain,
	}, in.logger)
}
