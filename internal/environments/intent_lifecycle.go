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
)

// AdvanceIntent proves domain transition/fencing, not actual job execution. It
// never creates a container, changes an environment or schedules a River job.
func (r *IntentRepository) AdvanceIntent(ctx context.Context, command IntentTransition) (result BuildIntent, err error) {
	if err := command.validate(); err != nil {
		return result, err
	}
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := lockIntentScope(ctx, q, command.UserID, command.PullRequestID)
		if err != nil {
			return err
		}
		current, err := readLockedIntent(ctx, q, command.PullRequestID)
		if err != nil {
			return err
		}
		if !current.forHead(scope) || current.Revision != command.Revision || current.Generation != command.Generation || scope.pr.State != StateOpen {
			return ErrIntentConflict
		}
		if command.Next == IntentRunning {
			if err := BuildBlockReason(scope.pr.BuildStatus, scope.pr.NeedsRefresh); err != nil {
				return err
			}
		}
		if current.State == command.Next {
			result = *current
			return nil // Same fenced transition is idempotent; it does not repeat effects.
		}
		if !transitionAllowed(current.State, command.Next) {
			return ErrIntentAction
		}
		if !current.allowedIn(scope.mode) {
			return ErrIntentConflict
		}
		row, err := q.AdvanceBuildIntent(ctx, dbgen.AdvanceBuildIntentParams{
			PrID: command.PullRequestID, Generation: command.Generation,
			Revision: command.Revision, PreviousState: string(current.State), NextState: string(command.Next),
		})
		if errors.Is(err, pgx.ErrNoRows) && command.Next == IntentRunning {
			return ErrIntentNotDue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIntentConflict
		}
		if err != nil {
			return fmt.Errorf("advance current build intent: %w", err)
		}
		result = intentFromRow(row)
		return nil
	})
	if err != nil {
		return BuildIntent{}, err
	}
	return result, nil
}

func (r *IntentRepository) ObserveIntentTx(ctx context.Context, tx pgx.Tx, userID, prID int64, deadline time.Time) (ObservationOutcome, error) {
	if tx == nil || userID <= 0 || prID <= 0 {
		return ObservationOutcome{}, ErrIntentUnavailable
	}
	q := dbgen.New(tx)
	scope, err := lockIntentScope(ctx, q, userID, prID)
	if err != nil {
		return ObservationOutcome{}, err
	}
	current, err := readLockedIntent(ctx, q, prID)
	if err != nil {
		return ObservationOutcome{}, err
	}
	closed := scope.pr.State != StateOpen
	stale := current != nil && !current.forHead(scope)
	// A fork's commit is never built without the owner: in Automatic mode it
	// is observed like Manual, and waits for Build latest commit.
	manual := scope.mode == modeManual || scope.pr.Untrusted
	if closed || (stale && manual) {
		if _, err := q.SupersedeBuildIntent(ctx, prID); err != nil {
			return ObservationOutcome{}, fmt.Errorf("supersede observed intent: %w", err)
		}
		return ObservationOutcome{TeardownRequired: closed}, nil
	}
	if manual {
		return ObservationOutcome{Intent: current}, nil
	}
	if current != nil && !stale {
		return ObservationOutcome{Intent: current}, nil // Duplicate observation cannot reset deadlines.
	}
	if deadline.IsZero() {
		return ObservationOutcome{}, errors.New("automatic intent requires a durable eligibility deadline")
	}
	baseRevision := int64(0)
	if current != nil {
		baseRevision = current.Revision
	}
	row, err := q.ReplaceBuildIntent(ctx, dbgen.ReplaceBuildIntentParams{
		PrID: prID, BaseRevision: baseRevision, Generation: scope.pr.Generation, HeadSha: scope.pr.HeadSha,
		Source: modeAutomatic, RequestAction: modeAutomatic, IdempotencyKey: fmt.Sprintf("auto-%d-%d", prID, scope.pr.Generation),
		AvailableAt: pgtype.Timestamptz{Time: deadline.UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ObservationOutcome{}, ErrIntentConflict
	}
	if err != nil {
		return ObservationOutcome{}, fmt.Errorf("persist automatic intent: %w", err)
	}
	intent := intentFromRow(row)
	return ObservationOutcome{Intent: &intent}, nil
}
