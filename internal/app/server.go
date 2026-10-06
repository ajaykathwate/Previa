package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const (
	webhookPath        = "/webhooks/github"
	webhookConcurrency = 32
	maxFormBytes       = 256 << 10 // the largest form is a 32 KiB secret value
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
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize server database: %w", err)
	}
	defer pool.Close()
	if err := database.CheckSchema(ctx, pool); err != nil {
		return err
	}
	logger.Info("database health check passed", "version", version.Version, "schema", database.SchemaVersion, "environment", cfg.AppEnv)
	store := pgxstore.New(pool)
	defer store.StopCleanup()
	sessions := auth.NewSessionManager(store, strings.HasPrefix(strings.ToLower(cfg.AppBaseURL), "https://"), logger)
	authHandler := auth.NewHandler(cfg, sessions, auth.NewRepository(pool), auth.NewGitHubOAuth(cfg), logger)
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return err
	}
	pings := queue.NewService(pool, producer)

	// The server records observations, owner requests and teardowns with
	// their durable wake-ups and operations; workers execute them.
	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		return err
	}
	environmentRepo := environments.NewRepository(pool, operations)
	coordinator := environments.NewRequestBroker(pool, producer)
	var appClient *githubapp.Client
	var upstream pullrequests.Source
	if cfg.GitHubAppID != 0 {
		if appClient, err = githubapp.New(cfg); err != nil {
			return fmt.Errorf("initialize GitHub App: %w", err)
		}
		upstream = pullrequests.NewGitHubSource(appClient)
		// With the App, workers can report: record report work for PR changes.
		environmentRepo.WithReports()
		coordinator.WithReports(environmentRepo)
	}
	syncService := pullrequests.NewSyncService(pool, producer, upstream, coordinator)
	var invalidate func(int64)
	service := installations.NewService(installations.NewRepository(pool), nil, nil, nil)
	if appClient != nil {
		invalidate = appClient.Invalidate
		service = installations.NewService(installations.NewRepository(pool, syncService), appClient, installations.NewGitHubAuthorization(cfg), nil)
	}
	installationHandler := installations.NewHandler(service, authHandler, sessions, cfg.GitHubAppSlug, logger)
	prRepository := pullrequests.NewRepository(pool)
	capabilities := workers.NewReader(pool)
	var prs *pullrequests.Handler // set below; the Overview reads its highlights
	summaryReader := func(ctx context.Context, userID int64) (dashboardpages.Summary, error) {
		summary, err := service.Summary(ctx, userID)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		delivery, err := prRepository.Delivery(ctx, userID)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		caps, err := capabilities.Current(ctx)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		builds, previews, err := prs.Highlights(ctx, userID, dashboardpages.HighlightRows)
		if err != nil {
			return dashboardpages.Summary{}, err
		}
		return dashboardpages.Summary{
			Installations: summary.Installations, Repositories: summary.Repositories, Available: summary.Available,
			OpenPRs: delivery.OpenPrs, Waiting: delivery.Waiting, Building: delivery.Building, Failed: delivery.Failed, Live: delivery.Live,
			Workers: dashboardpages.Workers{Discovery: caps.Discovery, Execution: caps.Execution, Reporting: caps.Reporting, Housekeeping: caps.Housekeeping, Paused: caps.Paused},
			Builds:  builds, Previews: previews,
		}, nil
	}
	appURL, err := url.Parse(cfg.AppBaseURL)
	if err != nil {
		return fmt.Errorf("parse APP_BASE_URL: %w", err)
	}
	proxies, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return err
	}
	// Ingress controls (docs/22-observability-and-hardening.md): only the
	// dashboard's host is served, except signed webhooks that may arrive
	// through a tunnel; bodies are capped (webhooks cap their own); sign-in
	// and installation callbacks are rate limited per client.
	router := httpx.New(logger, httpx.Options{
		AppHost: appURL.Host, HostExempt: []string{webhookPath},
		Secure: appURL.Scheme == "https", TrustedProxies: proxies,
		MaxBodyBytes: maxFormBytes, BodyExempt: []string{webhookPath},
	})
	router.Use(httpx.RateLimit(signInPaths, 2*time.Second, 20))
	router.Handle("/static/*", web.StaticHandler())
	// The router sends unknown and removed preview hosts here. Public and
	// outside the session group: it reads and sets no cookie.
	router.Get(routing.UnavailablePath, routing.UnavailablePage)
	if cfg.PreviewBaseDomain != "" && config.PreviewSameSite(cfg.PreviewBaseDomain, cfg.AppBaseURL) {
		logger.Warn("previews share the dashboard's registrable domain; a separate domain isolates them fully", "preview_base_domain", cfg.PreviewBaseDomain)
	}
	deliveries := webhooks.NewDeliveryService(pool, coordinator, invalidate, syncService)
	// A bounded number of deliveries run at once; the rest get 503 before
	// anything is recorded, so redelivery and deduplication work as usual.
	router.Post(webhookPath, httpx.LimitConcurrency(webhookConcurrency, webhooks.NewHandler(cfg.GitHubWebhookSecret, deliveries, logger)).ServeHTTP)
	policyStore := installations.NewRepository(pool)
	if appClient != nil {
		policyStore.WithReports(coordinator) // a switch to Manual updates cancelled PRs' reports
	}
	policy, err := installations.NewBuildPolicyService(policyStore)
	if err != nil {
		return err
	}
	// One LISTEN session per process feeds committed-change events to live pages.
	hub := realtime.NewHub()
	listenCtx, stopListening := context.WithCancel(ctx)
	defer stopListening()
	go realtime.Listen(listenCtx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	}, hub, logger)
	logStore := buildlogs.NewStore(pool)
	prs = pullrequests.NewHandler(prRepository, syncService, policy, authHandler, logger, cfg.GitHubAppID > 0).
		WithLiveUpdates(hub).WithLogTail(logStore)
	if appClient != nil {
		// Builds need fresh GitHub reads; without the App the controls say so.
		prs.WithActions(pullrequests.Actions{
			Builds: coordinator, Verifier: syncService, Teardowns: environmentRepo, Capabilities: capabilities,
			PreviewBaseDomain: cfg.PreviewBaseDomain,
		})
	}
	installationHandler.WithNavigator(prs)
	var keyring *secrets.Keyring
	if cfg.SecretsConfigured() {
		keyring, err = secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion)
		if err != nil {
			return fmt.Errorf("configure repository secrets: %w", err)
		}
	}
	secretService, err := secrets.NewService(secrets.NewRepository(pool), keyring)
	if err != nil {
		return err
	}
	secretHandler := secrets.NewHandler(secretService, authHandler, logger)
	logHandler := buildlogs.NewHandler(logStore, authHandler, logger).WithLiveUpdates(hub, realtime.DefaultStreamOptions)
	router.Group(func(r chi.Router) {
		r.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
		authHandler.Mount(r)
		// Queue diagnostics are for development; production does not serve them.
		dashboard.MountWithSummary(r, authHandler, auth.RequireUser, dashboard.Options{
			Summary: summaryReader, Identity: authHandler.UserIDs, DeveloperTools: !cfg.Production(),
		})
		installationHandler.Mount(r, auth.RequireUser)
		prs.Mount(r, auth.RequireUser)
		secretHandler.Mount(r, auth.RequireUser)
		logHandler.Mount(r, auth.RequireUser)
		if !cfg.Production() {
			dashboard.MountPing(r, pings, auth.RequireUser, logger)
		}
	})
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	// Event streams outlive WriteTimeout by design; closing the hub ends them so
	// graceful shutdown does not wait for browsers to disconnect.
	server.RegisterOnShutdown(hub.Close)
	registerServerState(pool, environmentRepo, capabilities, cfg.BackupStatusFile, logger)
	stopInternal, err := httpx.ServeInternal(cfg.InternalAddr, httpx.Internal([]httpx.Check{
		{Name: "database", Run: func(ctx context.Context) error { return database.HealthCheck(ctx, pool) }},
		{Name: "schema", Run: func(ctx context.Context) error { return database.CheckSchema(ctx, pool) }},
	}, func() map[string]any { return map[string]any{"version": version.Version} }), logger)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopInternal(ctx)
	}()
	listener, err := net.Listen("tcp", server.Addr)
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
