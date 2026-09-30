package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// RunRiverMigration adapts River's migrator to DATABASE_URL without credential flags.
func RunRiverMigration(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return errors.New("usage: river-migrate <up|status>; connection comes from DATABASE_URL")
	}
	cfg, err := config.LoadShared()
	if err != nil {
		return err
	}
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("initialize River migration database: %w", err)
	}
	defer pool.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("initialize River migrator: %w", err)
	}
	if args[0] == "up" {
		result, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
		if err != nil {
			return fmt.Errorf("apply River migrations: %w", err)
		}
		if _, err := fmt.Fprintf(out, "River: applied %d migrations\n", len(result.Versions)); err != nil {
			return fmt.Errorf("write River migration result: %w", err)
		}
	}
	applied, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return fmt.Errorf("read River migration status: %w", err)
	}
	versions := make(map[int]bool, len(applied))
	for _, migration := range applied {
		versions[migration.Version] = true
	}
	for _, migration := range migrator.AllVersions() {
		state := "pending"
		if versions[migration.Version] {
			state = "applied"
		}
		if _, err := fmt.Fprintf(out, "%03d %s %s\n", migration.Version, state, migration.Name); err != nil {
			return fmt.Errorf("write River migration status: %w", err)
		}
	}
	return nil
}
