package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/secrets"
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

// resetActivity puts each Reset test data on the activity timeline, in the
// transaction that requests its Rebuild.
type resetActivity struct{ recorder *activity.Recorder }

func (a resetActivity) ResetRequestedTx(ctx context.Context, tx pgx.Tx, r environments.ResetRequest) error {
	metadata := map[string]any{"sha": r.HeadSHA, "revision": r.Revision}
	if r.Source != "" {
		metadata["source"] = r.Source
	}
	return a.recorder.RecordTx(ctx, tx, activity.Event{
		PullRequestID: r.PullRequestID, ActorID: r.ActorID, Kind: activity.ResetRequested,
		Metadata: metadata, DedupKey: fmt.Sprintf("reset:%d:%d", r.PullRequestID, r.Revision),
	})
}

// newReviews wires the review page and its feedback. Test-access secrets are
// sealed with the repository secrets' keyring; without one, only the account
// text can be stored.
func newReviews(cfg config.ServerConfig, pool *pgxpool.Pool, live reviews.Cursor, users reviews.UserSession, logger *slog.Logger) (*reviews.Handler, error) {
	opts := reviews.Options{PreviewDomain: cfg.PreviewBaseDomain}
	if cfg.SecretsConfigured() {
		keys, err := secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion)
		if err != nil {
			return nil, fmt.Errorf("configure test access encryption: %w", err)
		}
		opts.Sealer = keys
	}
	service, err := reviews.NewService(pool, reviewActivity{recorder: activity.NewRecorder()}, opts)
	if err != nil {
		return nil, fmt.Errorf("configure reviews: %w", err)
	}
	return reviews.NewHandler(service, users, live, logger, cfg.AppBaseURL), nil
}

// testAccessRotation lets key rotation re-encrypt sealed test access beside
// repository secrets.
type testAccessRotation struct{ store *reviews.Rotation }

func (r testAccessRotation) NeedingRotation(ctx context.Context, active int32, after int64, limit int32) ([]secrets.StoredSecret, error) {
	rows, err := r.store.NeedingRotation(ctx, active, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]secrets.StoredSecret, len(rows))
	for i, row := range rows {
		out[i] = secrets.StoredSecret{ID: row.ID, RepoID: row.RepoID, SealedSecret: secrets.SealedSecret{Name: reviews.TestAccessName, Value: row.Value, Version: row.Version}}
	}
	return out, nil
}

func (r testAccessRotation) Rotate(ctx context.Context, s secrets.StoredSecret, sealed []byte, version int32) (bool, error) {
	return r.store.Rotate(ctx, reviews.SealedAccess{ID: s.ID, RepoID: s.RepoID, Version: s.Version, Value: s.Value}, sealed, version)
}

func (r testAccessRotation) CountByVersion(ctx context.Context) (map[int32]int64, error) {
	return r.store.CountByVersion(ctx)
}
