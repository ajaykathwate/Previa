package environments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ComputeUsage is the execution and publication contract this feature needs.
// Cleanup, teardown and reads deliberately have no entitlement dependency.
type ComputeUsage interface {
	AdmitPRTx(context.Context, pgx.Tx, int64, string, time.Time) (string, time.Time, error)
	PublicationTx(context.Context, pgx.Tx, int64, time.Time) (string, time.Time, error)
	Run(context.Context, string, func(context.Context) error) error
}

func (r *Repository) WithUsage(u ComputeUsage) *Repository { r.usage = u; return r }

func (r *Repository) reserveUsageTx(ctx context.Context, tx pgx.Tx, request Request, now time.Time) (string, *WaitingError, error) {
	if r.usage == nil {
		return "", nil, nil
	}
	key := uuid.NewString()
	reason, until, err := r.usage.AdmitPRTx(ctx, tx, request.PullRequestID, key, now)
	if err != nil {
		return "", nil, err
	}
	if reason == "" {
		return key, nil, nil
	}
	if err = r.releaseComputeSlotTx(ctx, tx, request); err != nil {
		return "", nil, err
	}
	if err = r.markWaitingTx(ctx, tx, request, reason, until); err != nil {
		return "", nil, err
	}
	return "", &WaitingError{Reason: reason, Until: until}, nil
}

func (r *Repository) publicationUsageTx(ctx context.Context, tx pgx.Tx, pr int64) error {
	if r.usage == nil {
		return nil
	}
	reason, until, err := r.usage.PublicationTx(ctx, tx, pr, time.Now())
	if err != nil {
		return err
	}
	if reason != "" {
		return &WaitingError{Reason: reason, Until: until}
	}
	return nil
}

func (r *Repository) releaseComputeSlotTx(ctx context.Context, tx pgx.Tx, request Request) error {
	_, err := tx.Exec(ctx, `UPDATE build_intents SET state='queued',started_at=NULL WHERE pull_request_id=$1 AND generation=$2 AND revision=$3 AND state='running'`, request.PullRequestID, request.Generation, request.Revision)
	if err != nil {
		return fmt.Errorf("release waiting compute slot: %w", err)
	}
	return nil
}

func usageWait(reason string) bool {
	switch reason {
	case "workspace_builds", "workspace_minutes", "workspace_daily", "workspace_previews", "workspace_restricted", "workspace_accounting":
		return true
	}
	return false
}
