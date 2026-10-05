package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/routing"
)

// RunRouteSync writes the accepted route table from PostgreSQL to the router's
// dynamic directory, as the worker's route controller does at startup. Before
// the first preview it writes the unavailable-page fallback, whose router also
// makes Traefik obtain the wildcard certificate. It prints the version only.
func RunRouteSync(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadRouteSync()
	if err != nil {
		return err
	}
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize route database: %w", err)
	}
	defer pool.Close()
	file, err := routing.NewFile(cfg.RouterDynamicDir)
	if err != nil {
		return err
	}
	settings := routing.Settings{
		BaseDomain: cfg.PreviewBaseDomain, EntryPoint: cfg.RouterEntryPoint,
		UnavailableURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + routing.UnavailablePath,
	}
	if err := routing.Sync(ctx, environments.NewRepository(pool), file, settings); err != nil {
		return fmt.Errorf("sync route table: %w", err)
	}
	version, err := file.Version()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Routes: %s holds route table version %d\n", file.Path(), version)
	return err
}
