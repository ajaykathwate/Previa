package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/ajaykathwatee/previa/db"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// RunMigrations applies (up) or reports (status) the embedded Goose
// migrations and River's own, in that order, from DATABASE_URL: the release
// image's one-shot migration step. Migrations only move forward here;
// rolling back is a restore (docs/25-production-deployment.md).
func RunMigrations(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return errors.New("usage: migrate <up|status>; the connection comes from DATABASE_URL")
	}
	cfg, err := config.LoadShared()
	if err != nil {
		return err
	}
	cfg.ApplicationName = "previa-migrate"
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("initialize migration database: %w", err)
	}
	defer pool.Close()
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	if err := gooseMigrations(ctx, sqlDB, args[0], out); err != nil {
		return err
	}
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
			return err
		}
	}
	applied, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return fmt.Errorf("read River migration status: %w", err)
	}
	_, err = fmt.Fprintf(out, "River: %d of %d migrations applied\n", len(applied), len(migrator.AllVersions()))
	return err
}

func gooseMigrations(ctx context.Context, sqlDB *sql.DB, command string, out io.Writer) error {
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		return fmt.Errorf("initialize schema migrations: %w", err)
	}
	if command == "up" {
		results, err := provider.Up(ctx)
		for _, r := range results {
			if _, werr := fmt.Fprintf(out, "Goose: applied %s (%s)\n", filepath.Base(r.Source.Path), r.Duration.Round(1e6)); werr != nil {
				return werr
			}
		}
		if err != nil {
			return fmt.Errorf("apply schema migrations: %w", err)
		}
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version < database.SchemaVersion {
		return fmt.Errorf("the schema is at migration %d but this release needs %d", version, database.SchemaVersion)
	}
	_, err = fmt.Fprintf(out, "Goose: schema at migration %d\n", version)
	return err
}
