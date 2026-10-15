package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/source"
)

// RunHousekeepingDryRun runs one inventory, expiry and prune pass as a dry
// run, whatever HOUSEKEEPING_DRY_RUN says: every decision is logged to out and
// nothing is deleted, scheduled or changed. PR reconciliation and recovery are
// skipped because they write state. workspaceRoot, when set, is the source
// workspace directory to inspect.
func RunHousekeepingDryRun(ctx context.Context, out io.Writer, workspaceRoot string) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(out, nil))
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize housekeeping database: %w", err)
	}
	defer pool.Close()
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		return err
	}
	docker, err := dockerx.New(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = docker.Close() }()
	gateway, err := egress.NewGateway(docker, cfg.EgressImage)
	if err != nil {
		return err
	}
	builder, err := builds.NewBuilder(docker, gateway, builds.ConfigFrom(cfg.BuilderConfig, instance, cfg.BuildTimeout), logger)
	if err != nil {
		return err
	}
	inventory, err := runtime.NewInventory(docker, instance, logger)
	if err != nil {
		return err
	}
	settings := housekeeping.SettingsFrom(cfg.HousekeepingConfig, cfg.BuildTimeout)
	settings.DryRun = true
	repo := environments.NewRepository(pool)
	resources, err := housekeeping.NewResources(repo, inventory, pool, logger, settings)
	if err != nil {
		return err
	}
	expiry, err := housekeeping.NewExpiry(pool, repo, logger, settings)
	if err != nil {
		return err
	}
	var workspaces housekeeping.Workspaces
	if workspaceRoot != "" {
		if workspaces, err = source.NewWorkspaces(workspaceRoot); err != nil {
			return err
		}
	}
	prune, err := housekeeping.NewPrune(builder, workspaces, pool, logger, settings)
	if err != nil {
		return err
	}
	records, err := housekeeping.NewRecords(pool, logger, settings)
	if err != nil {
		return err
	}
	inventoryPass, inventoryErr := resources.RunOnce(ctx)
	expiryPass, expiryErr := expiry.RunOnce(ctx)
	prunePass, pruneErr := prune.RunOnce(ctx, false)
	recordsPass, recordsErr := records.RunOnce(ctx)
	_, err = fmt.Fprintf(out, "Dry run for instance %s (nothing was changed):\n"+
		"  orphans of known PRs: %d (%d objects younger than the %s grace period left alone)\n"+
		"  objects of PRs the database does not know: %d from %d PRs\n"+
		"  previews past their expiry: %d\n"+
		"  untagged images: %d, leftover workspaces: %d, build caches: %d (%d bytes)\n"+
		"  expired records (first batch): %d webhook deliveries, %d operations, %d closed pull requests\n",
		instance, len(inventoryPass.Orphans), inventoryPass.Young, settings.Grace,
		len(inventoryPass.Removed), inventoryPass.UnknownPRs, expiryPass.Due,
		prunePass.Images, prunePass.Workspaces, prunePass.Caches, prunePass.CacheBytes,
		recordsPass.Deliveries, recordsPass.Operations, recordsPass.PullRequests)
	return errors.Join(inventoryErr, expiryErr, pruneErr, recordsErr, err)
}
