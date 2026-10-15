package environments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type IntentRepository struct{ pool *pgxpool.Pool }

func NewIntentRepository(pool *pgxpool.Pool) *IntentRepository { return &IntentRepository{pool: pool} }

type intentScope struct {
	pr      dbgen.LockIntentPRRow
	mode    string
	private bool
}

// Lock order also serializes mode changes and installation revocation. Recheck
// repo membership after locking the installation: relinking may race the lookup.
func lockIntentScope(ctx context.Context, q *dbgen.Queries, userID, prID int64) (intentScope, error) {
	scope, err := q.FindIntentScope(ctx, prID)
	if err != nil {
		return intentScope{}, intentAccessError("find intent scope", err)
	}
	if _, err := q.LockIntentInstallation(ctx, dbgen.LockIntentInstallationParams{InstallationID: scope.InstallationID, UserID: pgtype.Int8{Int64: userID, Valid: true}}); err != nil {
		return intentScope{}, intentAccessError("lock intent installation", err)
	}
	repo, err := q.LockIntentRepository(ctx, dbgen.LockIntentRepositoryParams{RepoID: scope.RepoID, InstallationID: scope.InstallationID, UserID: pgtype.Int8{Int64: userID, Valid: true}})
	if err != nil {
		return intentScope{}, intentAccessError("lock intent repository", err)
	}
	pr, err := q.LockIntentPR(ctx, dbgen.LockIntentPRParams{PrID: prID, RepoID: scope.RepoID})
	if err != nil {
		return intentScope{}, intentAccessError("lock intent PR", err)
	}
	return intentScope{pr: pr, mode: repo.BuildMode, private: repo.PrivatePreviews}, nil
}

// lockOwnerIntentScope locks the intent scope on behalf of the PR's owner,
// whose access every system step (admission, results, routing, handoff) acts
// under; step names it in errors.
func lockOwnerIntentScope(ctx context.Context, q *dbgen.Queries, prID int64, step string) (intentScope, error) {
	owner, err := q.FindPROwner(ctx, prID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return intentScope{}, ErrIntentUnavailable
		}
		return intentScope{}, fmt.Errorf("find %s owner: %w", step, err)
	}
	return lockIntentScope(ctx, q, owner, prID)
}

func intentAccessError(operation string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIntentUnavailable
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func readLockedIntent(ctx context.Context, q *dbgen.Queries, prID int64) (*BuildIntent, error) {
	row, err := q.LockBuildIntent(ctx, prID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock build intent: %w", err)
	}
	intent := intentFromRow(row)
	return &intent, nil
}

func (r *IntentRepository) RequestIntent(ctx context.Context, command IntentCommand) (result IntentOutcome, err error) {
	if err := command.validate(); err != nil {
		return result, err
	}
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		var e error
		result, e = r.requestIntentTx(ctx, tx, command, time.Time{})
		return e
	})
	if err != nil {
		return IntentOutcome{}, err
	}
	return result, nil
}

func (r *IntentRepository) requestIntentTx(ctx context.Context, tx pgx.Tx, command IntentCommand, availableAt time.Time) (result IntentOutcome, err error) {
	q := dbgen.New(tx)
	scope, err := lockIntentScope(ctx, q, command.UserID, command.PullRequestID)
	if err != nil {
		return IntentOutcome{}, err
	}
	if scope.pr.State != StateOpen {
		return IntentOutcome{}, ErrPRClosed
	}
	if err := BuildBlockReason(scope.pr.BuildStatus, scope.pr.NeedsRefresh); err != nil {
		return IntentOutcome{}, err
	}
	current, err := readLockedIntent(ctx, q, command.PullRequestID)
	if err != nil {
		return IntentOutcome{}, err
	}
	coalesce, err := checkIntentCommand(command, scope, current)
	if err != nil {
		return IntentOutcome{}, err
	}
	if coalesce {
		result = IntentOutcome{Intent: *current, Coalesced: true}
		return result, nil
	}
	row, err := q.ReplaceBuildIntent(ctx, dbgen.ReplaceBuildIntentParams{
		PrID: command.PullRequestID, BaseRevision: command.ExpectedRevision,
		Generation: scope.pr.Generation, HeadSha: scope.pr.HeadSha,
		Source: modeManual, RequestAction: string(command.Action),
		AvailableAt: pgtype.Timestamptz{Time: availableAt, Valid: !availableAt.IsZero()},
		RequestedBy: pgtype.Int8{Int64: command.UserID, Valid: true}, IdempotencyKey: command.IdempotencyKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IntentOutcome{}, ErrIntentConflict
	}
	if err != nil {
		return IntentOutcome{}, fmt.Errorf("persist manual intent: %w", err)
	}
	if err := q.ClearLocalTeardownForBuild(ctx, command.PullRequestID); err != nil {
		return IntentOutcome{}, fmt.Errorf("clear previous teardown for manual build: %w", err)
	}
	result.Intent = intentFromRow(row)
	return result, nil
}

func checkIntentCommand(command IntentCommand, scope intentScope, current *BuildIntent) (bool, error) {
	revision := int64(0)
	if current != nil {
		revision = current.Revision
		if current.IdempotencyKey == command.IdempotencyKey {
			if current.Action != string(command.Action) || current.RequestedBy != command.UserID || current.BaseRevision != command.ExpectedRevision {
				return false, ErrIntentKeyConflict
			}
			if !current.forHead(scope) {
				return false, ErrIntentConflict
			}
			return true, nil
		}
	}
	if command.ExpectedRevision != revision {
		return false, ErrIntentConflict
	}
	if command.Action != BuildLatest {
		if !current.forHead(scope) {
			return false, ErrIntentConflict
		}
		if (command.Action == RetryBuild && current.State != IntentFailed) || (command.Action == Rebuild && current.State != IntentSucceeded) {
			return false, ErrIntentAction
		}
		return false, nil
	}
	if !current.forHead(scope) {
		return false, nil
	}
	switch current.State {
	case IntentPending, IntentQueued:
		// A manual override creates a new immediate revision, fencing any delayed
		// or queued automatic wake-up. Manual pending work simply coalesces.
		return current.Source == modeManual, nil
	case IntentRunning, IntentSucceeded, IntentFailed:
		return true, nil
	default:
		return false, nil
	}
}

func (r *IntentRepository) ReadIntent(ctx context.Context, userID, prID int64) (IntentView, error) {
	if userID <= 0 || prID <= 0 {
		return IntentView{}, ErrIntentUnavailable
	}
	row, err := dbgen.New(r.pool).ReadOwnedIntentView(ctx, dbgen.ReadOwnedIntentViewParams{PrID: prID, UserID: pgtype.Int8{Int64: userID, Valid: true}})
	if err != nil {
		return IntentView{}, intentAccessError("read owned intent", err)
	}
	view := IntentView{PullRequestID: row.ID, Generation: row.Generation, HeadSHA: row.HeadSha, PRState: row.PrState, BuildMode: row.BuildMode}
	if row.Revision.Valid {
		intent := intentFromRow(dbgen.BuildIntent{
			PullRequestID: row.ID, Revision: row.Revision.Int64, BaseRevision: row.BaseRevision.Int64,
			Generation: row.RequestedGeneration.Int64, HeadSha: row.RequestedSha.String,
			Source: row.Source.String, RequestAction: row.RequestAction.String, RequestedBy: row.RequestedBy,
			IdempotencyKey: row.IdempotencyKey.String, State: row.IntentState.String, AvailableAt: row.AvailableAt,
			RequestedAt: row.RequestedAt, StartedAt: row.StartedAt, CompletedAt: row.CompletedAt,
		})
		view.Current = &intent
	}
	return view, nil
}

func intentFromRow(row dbgen.BuildIntent) BuildIntent {
	return BuildIntent{
		ExecutionJobID: row.ExecutionJobID.Int64, PullRequestID: row.PullRequestID, Revision: row.Revision, BaseRevision: row.BaseRevision,
		Generation: row.Generation, HeadSHA: row.HeadSha, Source: row.Source, Action: row.RequestAction,
		RequestedBy: row.RequestedBy.Int64, IdempotencyKey: row.IdempotencyKey, State: IntentState(row.State),
		AvailableAt: row.AvailableAt.Time, RequestedAt: row.RequestedAt.Time,
		StartedAt: row.StartedAt.Time, CompletedAt: row.CompletedAt.Time,
	}
}
