package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
)

// reconcileExecutionTx stops re-enqueuing a request whose exact River execution
// has already ended. Older deployments are resolved once by their captured args;
// newly dispatched executions use a direct River primary-key lookup.
func (b *Broker) reconcileExecutionTx(ctx context.Context, tx pgx.Tx, i *BuildIntent) (bool, error) {
	if i == nil || (i.State != IntentRunning && i.State != IntentQueued) {
		return false, nil
	}
	var state string
	var err error
	if i.ExecutionJobID > 0 {
		err = tx.QueryRow(ctx, "SELECT state::text FROM river_job WHERE id=$1", i.ExecutionJobID).Scan(&state)
	} else {
		args, marshalErr := json.Marshal(map[string]any{"pull_request_id": i.PullRequestID, "generation": i.Generation, "revision": i.Revision, "head_sha": i.HeadSHA})
		if marshalErr != nil {
			return false, fmt.Errorf("encode legacy execution identity: %w", marshalErr)
		}
		err = tx.QueryRow(ctx, "SELECT state::text FROM river_job WHERE kind='build' AND args @> $1::jsonb ORDER BY id DESC LIMIT 1", args).Scan(&state)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read execution state: %w", err)
	}
	if state != "discarded" && state != "cancelled" && state != "completed" {
		return false, nil
	}
	request := NewRequest(i.PullRequestID, i.Generation, i.HeadSHA, StateOpen)
	request.Revision = i.Revision
	changed, err := b.failExecutionTx(ctx, tx, request)
	return changed, err
}
func (b *Broker) noteExecutionFailure(ctx context.Context, r Request, terminal bool) error {
	return database.InTransaction(ctx, b.pool, func(tx pgx.Tx) error {
		if _, err := lockOwnerIntentScope(ctx, dbgen.New(tx), r.PullRequestID, "execution failure"); err != nil {
			if errors.Is(err, ErrIntentUnavailable) {
				return nil
			}
			return err
		}

		if terminal {
			_, err := b.failExecutionTx(ctx, tx, r)
			return err
		}
		n, err := dbgen.New(tx).NoteExecutionRetry(ctx, dbgen.NoteExecutionRetryParams{PullRequestID: r.PullRequestID, Revision: r.Revision, Generation: r.Generation, HeadSha: r.HeadSHA})
		if err != nil {
			return fmt.Errorf("record execution retry: %w", err)
		}
		if n > 0 {
			return b.reportTx(ctx, tx, r.PullRequestID)
		}
		return nil
	})
}
func (b *Broker) failExecutionTx(ctx context.Context, tx pgx.Tx, r Request) (bool, error) {
	q := dbgen.New(tx)
	n, err := q.FailAbandonedExecution(ctx, dbgen.FailAbandonedExecutionParams{ID: r.PullRequestID, Revision: r.Revision, Generation: r.Generation, HeadSha: r.HeadSHA})
	if err != nil {
		return false, fmt.Errorf("record terminal execution failure: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := q.FinishAbandonedStream(ctx, dbgen.FinishAbandonedStreamParams{PullRequestID: r.PullRequestID, Generation: r.Generation, Revision: r.Revision, HeadSha: r.HeadSHA}); err != nil {
		return false, fmt.Errorf("finish abandoned build output: %w", err)
	}
	if err := q.RecordFinishedBuildTime(ctx, dbgen.RecordFinishedBuildTimeParams{PrID: r.PullRequestID, Revision: r.Revision}); err != nil {
		return false, fmt.Errorf("record failed execution usage: %w", err)
	}
	if err := b.reportTx(ctx, tx, r.PullRequestID); err != nil {
		return false, err
	}
	return true, nil
}
