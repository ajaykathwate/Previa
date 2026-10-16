package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/appoutput"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/idle"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/onboarding"
	"github.com/ajaykathwatee/previa/internal/outbound"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/previewaccess"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/starterkits"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const (
	webhookPath        = "/webhooks/github"
	webhookConcurrency = 32
	maxFormBytes       = 256 << 10 // includes bounded 192 KiB workspace-logo uploads
)

// signInPaths are reachable without a session or start one; each client may
// call them about once every two seconds after a burst of twenty.
var signInPaths = []string{"/auth/github/login", "/auth/github/callback", "/installations/start", "/installations/callback", "/installations/authorize/callback"}

// RunServer loads configuration, checks the database, and serves HTTP until canceled.
func RunServer(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return fmt.Errorf("configure server: %w", err)
	}
	cfg.ApplicationName = "previa-server"
	pool, err := openDatabase(ctx, cfg.SharedConfig, "server", cfg.AppEnv, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := pgxstore.New(pool)
	defer store.StopCleanup()
	// One LISTEN session per process feeds committed-change events to live pages.
	hub := realtime.NewHub()
	listenCtx, stopListening := context.WithCancel(ctx)
	defer stopListening()
	go realtime.Listen(listenCtx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	}, hub, logger)
	features, err := newServerFeatures(cfg, pool, store, hub, logger)
	if err != nil {
		return err
	}
	router, err := features.routes(cfg, logger)
	if err != nil {
		return err
	}
	server := newHTTPServer(cfg.HTTPAddr, router, logger)
	// Event streams outlive WriteTimeout by design; closing the hub ends them so
	// graceful shutdown does not wait for browsers to disconnect.
	server.RegisterOnShutdown(hub.Close)
	registerServerState(pool, features.environments, features.capabilities, cfg.BackupStatusFile, logger)
	stopInternal, err := httpx.ServeInternal(cfg.InternalAddr, httpx.Internal(databaseChecks(pool),
		func() map[string]any { return map[string]any{"version": version.Version} }), logger)
	if err != nil {
		return err
	}
	defer stopWithin(stopInternal, 5*time.Second)
	return serveHTTP(ctx, server, logger)
}

// serverFeatures is every feature the server serves, wired to one database
// and queue. The server records observations, owner requests and teardowns
// with their durable wake-ups and operations; workers execute them.
type serverFeatures struct {
	sessions       *scs.SessionManager
	auth           *auth.Handler
	installations  *installations.Handler
	prs            *pullrequests.Handler
	secrets        *secrets.Handler
	outbound       *outbound.Handler
	logs           *buildlogs.Handler
	output         *appoutput.Handler
	wake           *idle.WakePage
	access         *previewaccess.Handler
	deliveries     *webhooks.DeliveryService
	pings          *queue.Service
	summary        dashboard.SummaryReader
	environments   *environments.Repository
	capabilities   *workers.Reader
	kits           *starterkits.Handler
	workspaces     *workspaces.Handler
	workspaceScope *workspaces.Middleware
	publicIDs      *publicid.Catalog
	setup          *onboarding.Handler
	reviews        *reviews.Handler
}

func newServerFeatures(cfg config.ServerConfig, pool *pgxpool.Pool, store scs.Store, hub *realtime.Hub, logger *slog.Logger) (*serverFeatures, error) {
	f := &serverFeatures{capabilities: workers.NewReader(pool)}
	f.sessions = auth.NewSessionManager(store, strings.HasPrefix(strings.ToLower(cfg.AppBaseURL), "https://"), logger)
	f.publicIDs = publicid.New(pool)
	f.auth = auth.NewHandler(cfg, f.sessions, auth.NewRepository(pool), auth.NewGitHubOAuth(cfg), logger)
	kits, err := newStarterKits(f.auth, f.capabilities, logger)
	if err != nil {
		return nil, err
	}
	f.kits = kits
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return nil, err
	}
	f.pings = queue.NewService(pool, producer)
	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		return nil, err
	}
	f.environments = environments.NewRepository(pool, operations)
	coordinator := environments.NewRequestBroker(pool, producer).WithResets(resetActivity{recorder: activity.NewRecorder()})
	var appClient *githubapp.Client
	var upstream pullrequests.Source
	if cfg.GitHubAppID != 0 {
		if appClient, err = githubapp.New(cfg); err != nil {
			return nil, fmt.Errorf("initialize GitHub App: %w", err)
		}
		upstream = pullrequests.NewGitHubSource(appClient)
		// With the App, workers can report: record report work for PR changes.
		f.environments.WithReports()
		coordinator.WithReports(f.environments)
	}
	syncService := pullrequests.NewSyncService(pool, producer, upstream, coordinator)
	var invalidate func(int64)
	service := installations.NewService(installations.NewRepository(pool), nil, nil, nil)
	if appClient != nil {
		invalidate = appClient.Invalidate
		service = installations.NewService(installations.NewRepository(pool, syncService), appClient, installations.NewGitHubAuthorization(cfg), nil)
	}
	f.installations = installations.NewHandler(service, f.auth, f.sessions, cfg.GitHubAppSlug, logger)
	f.deliveries = webhooks.NewDeliveryService(pool, coordinator, invalidate, syncService)
	policyStore := installations.NewRepository(pool)
	if appClient != nil {
		policyStore.WithReports(coordinator) // a switch to Manual updates cancelled PRs' reports
	}
	policy, err := installations.NewBuildPolicyService(policyStore)
	if err != nil {
		return nil, err
	}
	logStore := buildlogs.NewStore(pool).WithRetention(cfg.BuildLogRetention())
	prRepository := pullrequests.NewRepository(pool)
	f.prs = pullrequests.NewHandler(prRepository, syncService, policy, f.auth, logger, cfg.GitHubAppID > 0).
		WithLiveUpdates(hub).WithLogTail(logStore).WithPreviewDomain(cfg.PreviewBaseDomain)
	if appClient != nil {
		// Builds need fresh GitHub reads; without the App the controls say so.
		f.prs.WithCommits(pullrequests.NewGitHubSource(appClient))
		f.prs.WithActions(pullrequests.Actions{
			Builds: coordinator, Verifier: syncService, Teardowns: f.environments, Capabilities: f.capabilities,
			PreviewBaseDomain: cfg.PreviewBaseDomain,
		})
	}
	f.installations.WithNavigator(f.prs)
	secretsService, err := newSecretsService(cfg, pool)
	if err != nil {
		return nil, err
	}
	f.secrets = secrets.NewHandler(secretsService, f.auth, logger)
	setup, setupHandler, err := newOnboarding(cfg, pool, producer, f.capabilities, secretsService, hub, f.auth, logger)
	if err != nil {
		return nil, err
	}
	f.setup = setupHandler
	if f.reviews, err = newReviews(cfg, pool, hub, f.auth, logger); err != nil {
		return nil, err
	}
	f.summary = overviewSummary(service, prRepository, f.capabilities, f.prs, setup)
	if f.outbound, err = newOutboundHandler(cfg, pool, producer, f.auth, logger); err != nil {
		return nil, err
	}
	f.logs = buildlogs.NewHandler(logStore, f.auth, logger).WithLiveUpdates(hub, realtime.DefaultStreamOptions).
		WithRetention(cfg.BuildLogRetention())
	f.output = appoutput.NewHandler(appoutput.NewStore(pool), f.auth, producer, hub, realtime.DefaultStreamOptions, logger)
	f.wake = idle.NewWakePage(pool, producer, cfg.PreviewBaseDomain, routing.UnavailablePage, logger)
	var permissions previewaccess.Permissions
	if appClient != nil {
		permissions = workspaceGitHub{app: appClient}
	}
	f.access = previewaccess.NewHandler(pool, f.auth, permissions, producer, cfg.SessionSecret, cfg.PreviewBaseDomain, cfg.AppBaseURL, logger).WithCapabilities(f.capabilities)
	workspaceAccess := workspaceGitHub{}
	if appClient != nil {
		workspaceAccess.app = appClient
	}
	workspaceService := workspaces.NewService(workspaces.NewRepository(pool), workspaceAccess, workspaceAccess, coordinator)
	f.prs.WithSourceAccess(workspaceService)
	f.auth.WithAvatarAudience(workspaceService).WithActivity(workspaceService)
	f.workspaces = workspaces.NewHandler(workspaceService, f.auth, logger, cfg.AppBaseURL)
	f.workspaceScope = workspaces.NewMiddleware(workspaceService, f.auth, logger)
	return f, nil
}

// overviewSummary reads the Overview's counts, worker capabilities, latest
// builds and previews, and where this person's first-preview setup stands.
func overviewSummary(service *installations.Service, prs *pullrequests.Repository, capabilities *workers.Reader, highlights *pullrequests.Handler, setup *onboarding.Service) dashboard.SummaryReader {
	return func(ctx context.Context, userID int64) (dashboardpages.Summary, error) {
		summary, err := service.Summary(ctx, userID)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		delivery, err := prs.Delivery(ctx, userID, pullrequests.AttentionSince(time.Now()))
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		caps, err := capabilities.Current(ctx)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		builds, previews, err := highlights.Highlights(ctx, userID, dashboardpages.HighlightRows)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		selection := scope.Current(ctx)
		journey, err := setup.Journey(ctx, selection.WorkspaceID, userID)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		return dashboardpages.Summary{
			Installations: summary.Installations, Repositories: summary.Repositories, Available: summary.Available,
			OpenPRs: delivery.OpenPrs, Waiting: delivery.Waiting, Building: delivery.Building, Failed: delivery.Failed, FailedOlder: delivery.FailedOlder, Live: delivery.Live,
			AttentionDays: int(pullrequests.FailureAttentionWindow / (24 * time.Hour)),
			Workers:       dashboardpages.Workers{Discovery: caps.Discovery, Execution: caps.Execution, Reporting: caps.Reporting, Housekeeping: caps.Housekeeping, Paused: caps.Paused},
			Builds:        builds, Previews: previews,
			Journey: overviewJourney(selection.Role, journey),
		}, nil
	}
}

// overviewJourney is the Overview's view of first-preview setup.
func overviewJourney(role string, j onboarding.Journey) dashboardpages.Journey {
	out := dashboardpages.Journey{Manager: role == "owner" || role == "admin", Repositories: j.Repositories, Available: j.Available,
		Checked: j.Checked, Published: j.Published, Opened: j.Opened, FocusID: j.FocusID, FocusName: j.FocusName}
	for _, m := range j.Managers {
		out.Managers = append(out.Managers, dashboardpages.Person{Login: m.Login, Name: m.Name})
	}
	return out
}

// newSecretsService manages repository secrets; without an encryption key
// the Secrets tab says secrets are unavailable.
func newSecretsService(cfg config.ServerConfig, pool *pgxpool.Pool) (*secrets.Service, error) {
	var keyring *secrets.Keyring
	if cfg.SecretsConfigured() {
		var err error
		if keyring, err = secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion); err != nil {
			return nil, fmt.Errorf("configure repository secrets: %w", err)
		}
	}
	return secrets.NewService(secrets.NewRepository(pool), keyring)
}

// newOutboundHandler serves owner-approved outbound connections; a change
// wakes the worker's refresh so a removal reaches running previews within
// seconds.
func newOutboundHandler(cfg config.ServerConfig, pool *pgxpool.Pool, producer *river.Client[pgx.Tx], users *auth.Handler, logger *slog.Logger) (*outbound.Handler, error) {
	ports, err := cfg.OutboundPorts()
	if err != nil {
		return nil, err
	}
	service, err := outbound.NewService(outbound.NewRepository(pool, producer), ports)
	if err != nil {
		return nil, err
	}
	return outbound.NewHandler(service, users, logger), nil
}

// routes is the server's HTTP surface. Ingress controls
// (docs/23-observability-and-hardening.md): only the dashboard's host is
// served, except signed webhooks that may arrive through a tunnel; bodies are
// capped (webhooks cap their own); sign-in and installation callbacks are
// rate limited per client.
func (f *serverFeatures) routes(cfg config.ServerConfig, logger *slog.Logger) (http.Handler, error) {
	appURL, err := url.Parse(cfg.AppBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse APP_BASE_URL: %w", err)
	}
	proxies, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	router := httpx.New(logger, httpx.Options{
		AppHost: appURL.Host, HostExempt: []string{webhookPath, routing.AccessPath},
		Secure: appURL.Scheme == "https", TrustedProxies: proxies,
		MaxBodyBytes: maxFormBytes, BodyExempt: []string{webhookPath},
	})
	router.Use(httpx.RateLimit(signInPaths, 2*time.Second, 20))
	router.Handle("/static/*", web.StaticHandler())
	router.Handle("/favicon.ico", web.FaviconHandler())
	// The router sends unknown and removed preview hosts here. Public and
	// outside the session group: it reads and sets no cookie.
	router.Get(routing.UnavailablePath, routing.UnavailablePage)
	// A sleeping preview's visitors wait here while it wakes; public too.
	router.Get(routing.WakePath+"/*", f.wake.ServeHTTP)
	router.Post(routing.WakePath+"/*", f.wake.ServeHTTP)
	// The router asks here before each request to a private preview; it
	// arrives with the preview's host, so the host check exempts it.
	f.access.MountPublic(router)
	if cfg.PreviewBaseDomain != "" && config.PreviewSameSite(cfg.PreviewBaseDomain, cfg.AppBaseURL) {
		logger.Warn("previews share the dashboard's registrable domain; a separate domain isolates them fully", "preview_base_domain", cfg.PreviewBaseDomain)
	}
	// A bounded number of deliveries run at once; the rest get 503 before
	// anything is recorded, so redelivery and deduplication work as usual.
	router.Post(webhookPath, httpx.LimitConcurrency(webhookConcurrency, webhooks.NewHandler(cfg.GitHubWebhookSecret, f.deliveries, logger)).ServeHTTP)
	router.Group(func(r chi.Router) {
		r.Use(f.sessions.LoadAndSave, f.auth.LoadUser, f.auth.CSRF)
		if f.publicIDs != nil {
			r.Use(f.publicIDs.Middleware)
		}
		if f.workspaceScope != nil {
			r.Use(f.workspaceScope.Load)
		}
		f.auth.Mount(r)
		if f.workspaces != nil {
			f.workspaces.Mount(r, auth.RequireUser)
		}
		// Queue diagnostics are for development; production does not serve them.
		dashboard.MountWithSummary(r, f.auth, auth.RequireUser, dashboard.Options{
			Summary: f.summary, Identity: f.auth.UserIDs, DeveloperTools: !cfg.Production(),
		})
		f.installations.Mount(r, auth.RequireUser)
		f.prs.Mount(r, auth.RequireUser)
		if f.setup != nil {
			f.setup.Mount(r, auth.RequireUser)
		}
		if f.reviews != nil {
			f.reviews.Mount(r, auth.RequireUser)
		}
		f.secrets.Mount(r, auth.RequireUser)
		f.outbound.Mount(r, auth.RequireUser)
		f.logs.Mount(r, auth.RequireUser)
		f.output.Mount(r, auth.RequireUser)
		f.access.Mount(r, auth.RequireUser)
		f.kits.Mount(r, auth.RequireUser)
		if !cfg.Production() {
			dashboard.MountPing(r, f.pings, auth.RequireUser, logger)
		}
	})
	return router, nil
}

func newHTTPServer(addr string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

// serveHTTP serves until ctx ends, then shuts the server down gracefully.
func serveHTTP(ctx context.Context, server *http.Server, logger *slog.Logger) error {
	listener, err := listenHTTP(server.Addr)
	if err != nil {
		return fmt.Errorf("listen for HTTP: %w", err)
	}
	logger.Info("HTTP server listening", "address", listener.Addr().String())
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()

	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			if closeErr := server.Close(); closeErr != nil {
				return fmt.Errorf("shutdown HTTP server: %w", errors.Join(err, closeErr))
			}
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("stop HTTP server: %w", err)
		}
	}
	return nil
}

func listenHTTP(addr string) (net.Listener, error) {
	// An explicit IPv4 bind must create an IPv4 socket. Go's generic TCP
	// listener can turn 0.0.0.0 into a dual-stack IPv6 socket, which WSL may
	// forward only on Windows ::1; Docker Desktop then cannot reach it by IPv4.
	network := "tcp"
	host, _, err := net.SplitHostPort(addr)
	if err == nil && net.ParseIP(host).To4() != nil {
		network = "tcp4"
	}
	return net.Listen(network, addr)
}
