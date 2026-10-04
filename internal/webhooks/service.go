package webhooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/webhooks/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PREvent struct {
	RepoID          int64
	Number          int32
	HeadSHA, Action string
	Merged          bool
	Mergeable       *bool
	MergeableState  string
}
type eventEnqueuer interface {
	ObserveTx(context.Context, pgx.Tx, environments.Request) (environments.Request, error)
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
	case "opened", "reopened", "synchronize":
	case "closed":
		state = "closed"
		if event.Merged {
			state = "merged"
		}
	default:
		return request, errors.New("unsupported PR action")
	}
	if event.RepoID <= 0 || event.Number <= 0 || event.HeadSHA == "" {
		return request, errors.New("invalid PR event")
	}
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := q.FindEventRepository(ctx, event.RepoID)
		if err != nil {
			return fmt.Errorf("find event repository: %w", err)
		}
		if _, err := q.LockEventInstallation(ctx, scope); err != nil {
			return fmt.Errorf("lock event installation: %w", err)
		}
		if _, err := q.LockEventRepository(ctx, dbgen.LockEventRepositoryParams{ID: event.RepoID, InstallationID: scope}); err != nil {
			return fmt.Errorf("lock event repository: %w", err)
		}
		mergeable := pgtype.Bool{}
		if event.Mergeable != nil {
			mergeable = pgtype.Bool{Bool: *event.Mergeable, Valid: true}
		}
		pr, err := q.RecordPREvent(ctx, dbgen.RecordPREventParams{RepoID: event.RepoID, Number: event.Number, HeadSha: event.HeadSHA, State: state, Mergeable: mergeable, MergeableState: event.MergeableState, BuildStatus: pullrequests.MergeabilityStatus(event.Mergeable, event.MergeableState)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("record PR event: %w", err)
		}
		request = environments.NewRequest(pr.ID, pr.Generation, pr.HeadSha, pr.State)
		request, err = s.jobs.ObserveTx(ctx, tx, request)
		return err
	})
	if err != nil {
		return environments.Request{}, err
	}
	return request, nil
}
