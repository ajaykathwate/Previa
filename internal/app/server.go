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
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/go-chi/chi/v5"
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

	var installationHandler *installations.Handler
	var installationList dashboard.InstallationList
	if cfg.GitHubAppID != 0 {
		appClient, err := githubapp.New(cfg)
		if err != nil {
			return fmt.Errorf("initialize GitHub App: %w", err)
		}
		service := installations.NewService(installations.NewRepository(pool), appClient, installations.NewGitHubAuthorization(cfg), nil)
		installationHandler = installations.NewHandler(service, authHandler, sessions, cfg.GitHubAppSlug, logger)
		installationList = func(ctx context.Context, userID int64) ([]dashboardpages.Installation, error) {
			linked, err := service.List(ctx, userID)
			if err != nil {
				return nil, err
			}
			pages := make([]dashboardpages.Installation, 0, len(linked))
			for _, installation := range linked {
				page := dashboardpages.Installation{ID: installation.ID, Account: installation.Account, Status: installation.Status}
				for _, repo := range installation.Repositories {
					page.Repositories = append(page.Repositories, dashboardpages.Repository{FullName: repo.FullName, Accessible: repo.Accessible})
				}
				pages = append(pages, page)
			}
			return pages, nil
		}
	}
	router := httpx.NewRouter(logger)
	router.Handle("/static/*", web.StaticHandler())
	router.Group(func(r chi.Router) {
		r.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
		authHandler.Mount(r)
		dashboard.MountWithInstallations(r, authHandler, auth.RequireUser, installationList, authHandler.UserIDs)
		if installationHandler != nil {
			installationHandler.Mount(r, auth.RequireUser)
		}
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
