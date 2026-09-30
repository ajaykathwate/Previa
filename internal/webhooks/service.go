package webhooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/webhooks/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PREvent struct {
	RepoID          int64
	Number          int32
	HeadSHA, Action string
}
type eventEnqueuer interface {
	EnqueueTx(context.Context, pgx.Tx, environments.Request) (int64, error)
}
type Service struct {
	pool *pgxpool.Pool
	jobs eventEnqueuer
}

func NewService(pool *pgxpool.Pool, jobs eventEnqueuer) *Service {
	return &Service{pool: pool, jobs: jobs}
}

// ProcessPR processes an already authenticated/validated webhook event. Transport,
// signature verification, repo installation, and delivery deduplication are not
// implemented here. Each successful invocation increments the generation once.
func (s *Service) ProcessPR(ctx context.Context, event PREvent) (request environments.Request, err error) {
	state := "open"
	switch event.Action {
	case "opened", "synchronize":
	case "closed":
		state = "closed"
	default:
		return request, errors.New("unsupported PR action")
	}
	if event.RepoID <= 0 || event.Number <= 0 || event.HeadSHA == "" {
		return request, errors.New("invalid PR event")
	}
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		pr, err := dbgen.New(tx).RecordPREvent(ctx, dbgen.RecordPREventParams{RepoID: event.RepoID, Number: event.Number, HeadSha: event.HeadSHA, State: state})
		if err != nil {
			return fmt.Errorf("record PR event: %w", err)
		}
		request = environments.NewRequest(pr.ID, pr.Generation, pr.HeadSha, pr.State)
		_, err = s.jobs.EnqueueTx(ctx, tx, request)
		return err
	})
	if err != nil {
		return environments.Request{}, err
	}
	return request, nil
}
