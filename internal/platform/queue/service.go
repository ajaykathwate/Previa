package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type transactionalInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Service enqueues ping jobs through a transaction and returns the committed job ID.
type Service struct {
	pool   *pgxpool.Pool
	client transactionalInserter
}

func NewService(pool *pgxpool.Pool, client transactionalInserter) *Service {
	return &Service{pool: pool, client: client}
}

func (s *Service) EnqueuePing(ctx context.Context, key string) (id int64, duplicate bool, resultErr error) {
	if err := ValidatePRKey(key); err != nil {
		return 0, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin ping transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback ping transaction: %w", err))
		}
	}()
	job, err := s.client.InsertTx(ctx, tx, PingArgs{PRKey: key}, nil)
	if err != nil {
		return 0, false, fmt.Errorf("insert ping job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit ping job: %w", err)
	}
	return job.Job.ID, job.UniqueSkippedAsDuplicate, nil
}
