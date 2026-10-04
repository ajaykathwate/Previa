package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// RunServer loads configuration, checks the database, and serves HTTP until canceled.
func RunServer(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return fmt.Errorf("configure server: %w", err)
	}
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize server database: %w", err)
	}
	defer pool.Close()
	logger.Info("database health check passed")
	store := pgxstore.New(pool)
	defer store.StopCleanup()
	sessions := auth.NewSessionManager(store, strings.HasPrefix(strings.ToLower(cfg.AppBaseURL), "https://"), logger)
	authHandler := auth.NewHandler(cfg, sessions, auth.NewRepository(pool), auth.NewGitHubOAuth(cfg), logger)
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return err
	}
	pings := queue.NewService(pool, producer)

	coordinator := environments.NewBroker(pool, producer)
	syncService := pullrequests.NewSyncService(pool, producer, nil, coordinator)
	var invalidate func(int64)
	service := installations.NewService(installations.NewRepository(pool), nil, nil, nil)
	if cfg.GitHubAppID != 0 {
		appClient, err := githubapp.New(cfg)
		if err != nil {
			return fmt.Errorf("initialize GitHub App: %w", err)
		}
		invalidate = appClient.Invalidate
		service = installations.NewService(installations.NewRepository(pool, syncService), appClient, installations.NewGitHubAuthorization(cfg), nil)
	}
	installationHandler := installations.NewHandler(service, authHandler, sessions, cfg.GitHubAppSlug, logger)
	summaryReader := func(ctx context.Context, userID int64) (dashboardpages.Summary, error) {
		summary, err := service.Summary(ctx, userID)
		return dashboardpages.Summary{Installations: summary.Installations, Repositories: summary.Repositories, Available: summary.Available}, err
	}
	router := httpx.NewRouter(logger)
	router.Handle("/static/*", web.StaticHandler())
	deliveries := webhooks.NewDeliveryService(pool, coordinator, invalidate, syncService)
	router.Post("/webhooks/github", webhooks.NewHandler(cfg.GitHubWebhookSecret, deliveries, logger).ServeHTTP)
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(pool))
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
	prs := pullrequests.NewHandler(pullrequests.NewRepository(pool), syncService, policy, authHandler, logger, cfg.GitHubAppID > 0).WithLiveUpdates(hub)
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
	logHandler := buildlogs.NewHandler(buildlogs.NewStore(pool), authHandler, logger).WithLiveUpdates(hub, realtime.DefaultStreamOptions)
	router.Group(func(r chi.Router) {
		r.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
		authHandler.Mount(r)
		dashboard.MountWithSummary(r, authHandler, auth.RequireUser, summaryReader, authHandler.UserIDs)
		installationHandler.Mount(r, auth.RequireUser)
		prs.Mount(r, auth.RequireUser)
		secretHandler.Mount(r, auth.RequireUser)
		logHandler.Mount(r, auth.RequireUser)
		dashboard.MountPing(r, pings, auth.RequireUser, logger)
	})
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	// Event streams outlive WriteTimeout by design; closing the hub ends them so
	// graceful shutdown does not wait for browsers to disconnect.
	server.RegisterOnShutdown(hub.Close)
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
