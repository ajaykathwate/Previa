package environments

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/jackc/pgx/v5"
)

// Publication is the immutable reference to a candidate that started serving
// a pull request's preview (docs/30-reviews.md). Reviews record findings
// against it; it is not a runnable environment. Sequence numbers a pull
// request's deployments 1, 2, 3, ...
type Publication struct {
	ID, PullRequestID    int64
	Sequence             int32
	HeadSHA              string
	Generation, Revision int64
}

// PublicationRecorder hears about each new publication inside the activating
// transaction, so what it writes (a timeline event) commits with it.
// internal/app connects the activity feature.
type PublicationRecorder interface {
	PublishedTx(ctx context.Context, tx pgx.Tx, p Publication) error
}

// WithPublications passes every new publication to rec as well.
func (r *Repository) WithPublications(rec PublicationRecorder) *Repository {
	r.publications = rec
	return r
}

// recordPublicationTx writes the publication of the candidate activateTx just
// made current. A retried activation of the same candidate, or a pull request
// whose installation belongs to no workspace, records nothing.
func (r *Repository) recordPublicationTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, op Operation, headSHA string, served Artifact) error {
	row, err := q.RecordPublication(ctx, dbgen.RecordPublicationParams{
		PullRequestID: op.PullRequestID, HeadSha: headSHA, Generation: op.Generation, Revision: op.Revision,
		ContainerID: text(served.ContainerID), ImageTag: text(served.ImageTag),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the publication: %w", err)
	}
	if r.publications == nil {
		return nil
	}
	return r.publications.PublishedTx(ctx, tx, Publication{
		ID: row.ID, PullRequestID: op.PullRequestID, Sequence: row.Sequence, HeadSHA: headSHA,
		Generation: op.Generation, Revision: op.Revision,
	})
}
