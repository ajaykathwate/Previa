package buildlogs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/buildlogs/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention is how long an attempt's logs are kept after it finishes (or, if it
// never finished, after it started). Admission of the next attempt clears them
// sooner.
const Retention = 14 * 24 * time.Hour

var (
	// ErrNoStream means the attempt has no log stream, for example because the
	// PR has no lifetime environment yet.
	ErrNoStream = errors.New("build has no log stream")
	// ErrSuperseded means a newer attempt owns the stream, or the attempt ended.
	ErrSuperseded = errors.New("build log stream belongs to a newer attempt")
	// ErrUnavailable hides whether a PR exists from users who do not own it.
	ErrUnavailable = errors.New("build logs are unavailable")
)

// Binding identifies the attempt a writer may append to: environments.Attempt's
// EnvironmentID and LogEpoch.
type Binding struct {
	EnvironmentID int64
	Epoch         int64
}

// Line is one stored line. Sequence numbers start at 0 for every attempt.
type Line struct {
	Sequence int64
	System   bool
	Text     string
}

// Stream describes the attempt whose output is stored now.
type Stream struct {
	EnvironmentID, Epoch, Generation, Revision int64
	HeadSHA                                    string
	Finished                                   bool
	Lines, StoredBytes                         int64
	Truncated                                  bool
	OmittedLines, LostLines                    int64
	OpenedAt, FinishedAt, PrunedAt             time.Time
}

// Intent is the PR's current build request.
type Intent struct {
	Generation, Revision int64
	HeadSHA, State       string
	AvailableAt          time.Time
}

// Target is an owner-visible PR whose logs are requested.
type Target struct {
	PullRequestID, RepoID int64
	Number                int32
	Title, HeadSHA        string
	PRState, Repository   string
	Available             bool
	EnvironmentID         int64 // 0 without a lifetime environment
	Intent                *Intent
}

// Store persists and reads current-build logs.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// Open binds a writer to an admitted attempt. redactor must hold every injected
// value and platform credential the attempt can print; an empty redactor is
// valid when there are none. It fails with ErrNoStream for an attempt without
// a stream and ErrSuperseded when the attempt is no longer current.
func (s *Store) Open(ctx context.Context, b Binding, redactor *secrets.Redactor, limits Limits) (*Writer, error) {
	if redactor == nil {
		return nil, errors.New("build log writer requires a redactor")
	}
	if b.EnvironmentID <= 0 || b.Epoch <= 0 {
		return nil, ErrNoStream
	}
	row, err := dbgen.New(s.pool).OpenStream(ctx, dbgen.OpenStreamParams{EnvironmentID: b.EnvironmentID, Epoch: b.Epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSuperseded
	}
	if err != nil {
		return nil, fmt.Errorf("open build log stream: %w", err)
	}
	return newWriter(s, b, stored{lines: row.NextSequence, bytes: row.StoredBytes, truncated: row.Truncated}, redactor, limits, s.now), nil
}

func (s *Store) append(ctx context.Context, b Binding, batch batch) error {
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		first, err := q.AllocateLines(ctx, dbgen.AllocateLinesParams{
			Lines: int64(len(batch.texts)), Bytes: batch.bytes, Truncated: batch.truncated,
			Omitted: batch.omitted, Lost: batch.lost, EnvironmentID: b.EnvironmentID, Epoch: b.Epoch,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSuperseded
		}
		if err != nil {
			return fmt.Errorf("allocate build log sequences: %w", err)
		}
		if len(batch.texts) == 0 {
			return nil
		}
		if err := q.InsertLines(ctx, dbgen.InsertLinesParams{EnvironmentID: b.EnvironmentID, Epoch: b.Epoch, FirstSequence: first, Kinds: batch.kinds, Texts: batch.texts}); err != nil {
			return fmt.Errorf("store build log lines: %w", err)
		}
		return nil
	})
}

// Finish marks the attempt's stream as complete. It is idempotent and does
// nothing for an attempt that a newer one has replaced.
func (s *Store) Finish(ctx context.Context, environmentID, epoch int64) error {
	if _, err := dbgen.New(s.pool).FinishStream(ctx, dbgen.FinishStreamParams{EnvironmentID: environmentID, Epoch: epoch}); err != nil {
		return fmt.Errorf("finish build log stream: %w", err)
	}
	return nil
}

// Target returns the PR and environment for an owner, or ErrUnavailable.
func (s *Store) Target(ctx context.Context, userID, repoID int64, number int32) (Target, error) {
	if userID <= 0 || repoID <= 0 || number <= 0 {
		return Target{}, ErrUnavailable
	}
	row, err := dbgen.New(s.pool).OwnedTarget(ctx, dbgen.OwnedTargetParams{ID: repoID, OwnerUserID: pgtype.Int8{Int64: userID, Valid: true}, Number: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrUnavailable
	}
	if err != nil {
		return Target{}, fmt.Errorf("read build log target: %w", err)
	}
	t := Target{
		PullRequestID: row.PullRequestID, RepoID: row.RepoID, Number: row.Number, Title: row.Title,
		HeadSHA: row.HeadSha, PRState: row.PrState, Repository: row.FullName, Available: row.Available.Bool,
		EnvironmentID: row.EnvironmentID.Int64,
	}
	if row.IntentState.Valid {
		t.Intent = &Intent{Generation: row.IntentGeneration.Int64, Revision: row.IntentRevision.Int64, HeadSHA: row.IntentHeadSha.String, State: row.IntentState.String, AvailableAt: row.IntentAvailableAt.Time}
	}
	return t, nil
}

// Stream returns the environment's current stream, or ErrNoStream.
func (s *Store) Stream(ctx context.Context, environmentID int64) (Stream, error) {
	if environmentID <= 0 {
		return Stream{}, ErrNoStream
	}
	row, err := dbgen.New(s.pool).ReadStream(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stream{}, ErrNoStream
	}
	if err != nil {
		return Stream{}, fmt.Errorf("read build log stream: %w", err)
	}
	return Stream{
		EnvironmentID: row.EnvironmentID, Epoch: row.Epoch, Generation: row.Generation, Revision: row.Revision,
		HeadSHA: row.HeadSha, Finished: row.State == "finished", Lines: row.NextSequence, StoredBytes: row.StoredBytes,
		Truncated: row.Truncated, OmittedLines: row.OmittedLines, LostLines: row.LostLines,
		OpenedAt: row.OpenedAt.Time, FinishedAt: row.FinishedAt.Time, PrunedAt: row.PrunedAt.Time,
	}, nil
}

// LinesFrom returns up to limit lines with sequence >= from, in order.
func (s *Store) LinesFrom(ctx context.Context, environmentID, epoch, from int64, limit int) ([]Line, error) {
	rows, err := dbgen.New(s.pool).LinesFrom(ctx, dbgen.LinesFromParams{EnvironmentID: environmentID, Epoch: epoch, Sequence: max(from, 0), Limit: int32(min(max(limit, 1), maxReadLines))})
	if err != nil {
		return nil, fmt.Errorf("read build log lines: %w", err)
	}
	lines := make([]Line, len(rows))
	for i, r := range rows {
		lines[i] = Line{Sequence: r.Sequence, System: r.Kind == kindSystem, Text: r.Line}
	}
	return lines, nil
}

// LinesBefore returns up to limit lines with sequence < before, in order.
func (s *Store) LinesBefore(ctx context.Context, environmentID, epoch, before int64, limit int) ([]Line, error) {
	rows, err := dbgen.New(s.pool).LinesBefore(ctx, dbgen.LinesBeforeParams{EnvironmentID: environmentID, Epoch: epoch, Sequence: before, Limit: int32(min(max(limit, 1), maxReadLines))})
	if err != nil {
		return nil, fmt.Errorf("read build log lines: %w", err)
	}
	lines := make([]Line, len(rows))
	for i, r := range rows {
		lines[len(rows)-1-i] = Line{Sequence: r.Sequence, System: r.Kind == kindSystem, Text: r.Line}
	}
	return lines, nil
}

// Tail returns the last limit lines of an environment's current attempt when
// that attempt belongs to the given request generation and revision. It
// returns nothing when there is no stream, a different attempt owns it or its
// lines were pruned, so a PR page never quotes another attempt's output.
func (s *Store) Tail(ctx context.Context, environmentID, generation, revision int64, limit int) ([]string, error) {
	stream, err := s.Stream(ctx, environmentID)
	if errors.Is(err, ErrNoStream) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stream.Generation != generation || stream.Revision != revision || !stream.PrunedAt.IsZero() {
		return nil, nil
	}
	lines, err := s.LinesBefore(ctx, environmentID, stream.Epoch, stream.Lines, limit)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	return texts, nil
}

// maxReadLines bounds one read.
const maxReadLines = 1000

// PruneReport counts what one Prune removed.
type PruneReport struct{ Streams, Lines int64 }

// Prune deletes lines of attempts that finished (or started, if they never
// finished) before cutoff, in bounded batches, and marks those streams pruned
// so pages can say the logs expired. It stops early when ctx ends.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (PruneReport, error) {
	var report PruneReport
	q := dbgen.New(s.pool)
	for ctx.Err() == nil {
		streams, err := q.ExpiredStreams(ctx, dbgen.ExpiredStreamsParams{Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true}, BatchSize: pruneStreams})
		if err != nil {
			return report, fmt.Errorf("find expired build logs: %w", err)
		}
		if len(streams) == 0 {
			return report, nil
		}
		for _, stream := range streams {
			for {
				deleted, err := q.DeleteStreamLines(ctx, dbgen.DeleteStreamLinesParams{EnvironmentID: stream.EnvironmentID, Epoch: stream.Epoch, BatchSize: pruneLines})
				if err != nil {
					return report, fmt.Errorf("delete expired build logs: %w", err)
				}
				report.Lines += deleted
				if deleted < pruneLines {
					break
				}
			}
			marked, err := q.MarkStreamPruned(ctx, dbgen.MarkStreamPrunedParams(stream))
			if err != nil {
				return report, fmt.Errorf("mark build logs expired: %w", err)
			}
			report.Streams += marked
		}
	}
	return report, ctx.Err()
}

const (
	pruneStreams = 50
	pruneLines   = 5000
)
