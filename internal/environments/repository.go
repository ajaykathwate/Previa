package environments

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Artifact struct{ ContainerID, ImageTag string }
type Result struct {
	Status   string
	Artifact Artifact
}
type Snapshot struct {
	Generation, AppliedGeneration int64
	Status                        string
	Artifact                      Artifact
}
type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Snapshot is an optimization only. Apply always performs its own locked SQL guard.
func (r *Repository) Snapshot(ctx context.Context, id int64) (Snapshot, error) {
	q := dbgen.New(r.pool)
	pr, err := q.ReadRequest(ctx, id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read PR generation: %w", err)
	}
	e, err := q.ReadEnvironment(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("read environment: %w", err)
	}
	return Snapshot{Generation: pr.Generation, AppliedGeneration: e.AppliedGeneration, Status: e.Status, Artifact: Artifact{e.ContainerID.String, e.ImageTag.String}}, nil
}

// Apply atomically checks the locked PR generation and writes the environment.
// publish executes only after the guarded UPDATE returns a row, while that same
// PR lock is still held. A newer webhook cannot commit between the guard and
// router registration/reporting. A false result means superseded, not an error.
func (r *Repository) Apply(ctx context.Context, request Request, result Result, publish func(context.Context, Artifact) error) (applied bool, err error) {
	if err := request.validate(); err != nil {
		return false, err
	}
	if result.Status != "running" && result.Status != "failed" && result.Status != "torn_down" {
		return false, errors.New("invalid environment result status")
	}
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		previous, err := q.ApplyResult(ctx, dbgen.ApplyResultParams{
			PrID: request.PullRequestID, ExpectedGeneration: request.Generation,
			ResultStatus: result.Status, HeadSha: request.HeadSHA,
			ContainerID: pgtype.Text{String: result.Artifact.ContainerID, Valid: result.Artifact.ContainerID != ""},
			ImageTag:    pgtype.Text{String: result.Artifact.ImageTag, Valid: result.Artifact.ImageTag != ""},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			latest, readErr := q.ReadRequest(ctx, request.PullRequestID)
			if readErr != nil {
				return fmt.Errorf("read rejected result generation: %w", readErr)
			}
			if latest.Generation == request.Generation {
				return errors.New("no active environment provisioned for current request")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply guarded environment result: %w", err)
		}
		if publish != nil {
			if err := publish(ctx, Artifact{previous.ContainerID.String, previous.ImageTag.String}); err != nil {
				return fmt.Errorf("publish current result: %w", err)
			}
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// Cleanup serializes resource deletion against publication. The adapter must
// delete only this PR's resources with generation <= request.Generation and
// preserve keep, even when it belongs to an older generation still serving traffic.
func (r *Repository) Cleanup(ctx context.Context, request Request, clean func(context.Context, Request, Artifact) error) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockRequest(ctx, request.PullRequestID); err != nil {
			return fmt.Errorf("lock PR for cleanup: %w", err)
		}
		e, err := q.ReadEnvironment(ctx, request.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read retained artifact: %w", err)
		}
		return clean(ctx, request, Artifact{e.ContainerID.String, e.ImageTag.String})
	})
}
