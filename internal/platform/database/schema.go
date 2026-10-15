package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion is the newest Goose migration this build was written for.
// A process refuses to start against an older schema, and readiness fails
// if the schema falls behind (for example after a restore from an old
// backup). A newer schema is accepted: migrations stay backward compatible
// for one release (docs/25-production-deployment.md).
const SchemaVersion = 35

// AppliedVersion is the highest Goose migration currently applied. A
// version's newest row decides, so a rolled-back migration does not count.
func AppliedVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(version_id), 0) FROM (
		SELECT DISTINCT ON (version_id) version_id, is_applied
		FROM goose_db_version ORDER BY version_id, id DESC
	) latest WHERE is_applied`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read the schema version (run migrations first): %w", err)
	}
	return v, nil
}

// CheckSchema fails when the database is behind SchemaVersion.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	v, err := AppliedVersion(ctx, pool)
	if err != nil {
		return err
	}
	if v < SchemaVersion {
		return fmt.Errorf("the database schema is at migration %d but this build needs %d: run migrations first", v, SchemaVersion)
	}
	return nil
}
