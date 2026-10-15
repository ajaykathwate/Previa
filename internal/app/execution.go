package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
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
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
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
}

func newExecution(ctx context.Context, in pipelineInput, upstream *pullrequests.GitHubSource) (*execution, error) {
	cfg, log, pool := in.cfg, in.logger, in.pool
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		return nil, err
	}
	identity, err := in.github.App(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the GitHub App's identity for PR reports: %w", err)
	}
	x := &execution{}
	if x.gateway, err = newEgressGateway(in); err != nil {
		return nil, err
	}
	provider, err := newSecretsProvider(in)
	if err != nil {
		return nil, err
	}
	if x.catalog, err = cfg.ServiceCatalog(); err != nil {
		return nil, err
	}
	workspaces, err := source.NewWorkspaces(cfg.SourceWorkspaceRoot)
	if err != nil {
		return nil, err
	}
	preparer, err := newPreparer(in, workspaces, x.catalog)
	if err != nil {
		return nil, err
	}
	builder, err := builds.NewBuilder(in.docker, x.gateway, builds.ConfigFrom(cfg.BuilderConfig, instance, cfg.BuildTimeout), log)
	if err != nil {
		return nil, err
	}
	x.logs = buildlogs.NewStore(pool)
	if x.runtime, err = newRuntime(in, instance, x.gateway, preparer, builder, provider, x.logs); err != nil {
		return nil, err
	}
	operations, err := environments.NewOperationBroker(in.producer)
	if err != nil {
		return nil, err
	}
	x.repo = environments.NewRepository(pool, operations).WithHostnames(routing.NewLabel).WithReports().
		WithLifetime(cfg.PreviewLifetime()).WithBuildSlots(cfg.MaxConcurrentBuilds).
		WithAccountLimits(environments.AccountLimits{BuildsPerAccount: cfg.BuildsPerAccount, MinutesPerDay: cfg.BuildMinutesPerAccountDay})
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
	if x.keeper, err = newHousekeeper(cfg, pool, log, repo, x.broker, x.sweeper, x.controller, x.runtime, x.sync, builder, workspaces); err != nil {
		return nil, err
	}
	return x, nil
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

// newSecretsProvider decrypts repository secrets for previews. Without a key
// it still runs: previews of repositories with secrets do not start.
func newSecretsProvider(in pipelineInput) (*secrets.Provider, error) {
	var keyring *secrets.Keyring
	if in.cfg.SecretsConfigured() {
		var err error
		if keyring, err = secrets.ParseKeyring(in.cfg.SecretsEncryptionKeys, in.cfg.SecretsActiveKeyVersion); err != nil {
			return nil, fmt.Errorf("configure repository secrets: %w", err)
		}
	}
	return secrets.NewProvider(secrets.NewRepository(in.pool), keyring)
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

func newRuntime(in pipelineInput, instance string, gateway *egress.Gateway, preparer *source.Preparer, builder *builds.Builder, provider *secrets.Provider, logStore *buildlogs.Store) (*runtime.Runtime, error) {
	targets := runtime.NewTargetStore(in.pool)
	return runtime.New(runtime.Parts{
		Instance: instance, Admission: runtime.NewAdmissionLock(in.pool), Docker: in.docker, Egress: gateway,
		Targets: targets, Sources: preparer, Builder: builder, Secrets: provider,
		Components: targets, Destinations: outbound.NewRepository(in.pool, nil), Outbound: targets,
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
