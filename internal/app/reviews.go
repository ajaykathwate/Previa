package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reviewActivity puts review changes on the activity timeline, in the
// review's own transaction.
type reviewActivity struct{ recorder *activity.Recorder }

func (a reviewActivity) RecordTx(ctx context.Context, tx pgx.Tx, e reviews.Event) error {
	return a.recorder.RecordTx(ctx, tx, activity.Event{
		PullRequestID: e.PullRequestID, PublicationID: e.PublicationID, ActorID: e.ActorID,
		Kind: e.Kind, Metadata: e.Metadata, DedupKey: e.DedupKey,
	})
}

// publicationActivity puts each new publication on the activity timeline, in
// the transaction that activates it.
type publicationActivity struct{ recorder *activity.Recorder }

func (a publicationActivity) PublishedTx(ctx context.Context, tx pgx.Tx, p environments.Publication) error {
	return a.recorder.RecordTx(ctx, tx, activity.Event{
		PullRequestID: p.PullRequestID, PublicationID: p.ID, Kind: activity.PreviewPublished,
		Metadata: map[string]any{"deployment": p.Sequence, "sha": p.HeadSHA},
		DedupKey: fmt.Sprintf("publication:%d", p.ID),
	})
}

// newReviews wires the review page and its feedback.
func newReviews(cfg config.ServerConfig, pool *pgxpool.Pool, live reviews.Cursor, users reviews.UserSession, logger *slog.Logger) (*reviews.Handler, error) {
	service, err := reviews.NewService(pool, reviewActivity{recorder: activity.NewRecorder()}, reviews.Options{PreviewDomain: cfg.PreviewBaseDomain})
	if err != nil {
		return nil, fmt.Errorf("configure reviews: %w", err)
	}
	return reviews.NewHandler(service, users, live, logger, cfg.AppBaseURL), nil
}
