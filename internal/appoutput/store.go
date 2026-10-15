package appoutput

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/appoutput/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Watch states.
const (
	StateRequested = "requested" // a viewer asked; no follower runs yet
	StateFollowing = "following"
	StateStopped   = "stopped" // Note says why
)

// keepLines is how many of the newest lines are kept while watching.
const keepLines = 1000

// ErrUnavailable hides whether a pull request exists from users who do not
// own it.
var ErrUnavailable = errors.New("app output is unavailable")

// Preview is an owner's pull request and its preview.
type Preview struct {
	PullRequestID, RepoID int64
	Number                int32
	Title, Repository     string
	EnvironmentID         int64 // 0 without one
	Live                  bool  // its app container runs
	DeployedSHA           string
}

// Watch is what the follower of one preview is doing.
type Watch struct {
	Epoch        int64
	State, Note  string
	NextSequence int64
	First        int64 // the oldest line still kept
}

// Line is one line of output and when the app printed it.
type Line struct {
	Sequence int64
	At       time.Time
	Text     string
}

// Store keeps app output only while someone watches.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Preview returns the owner's pull request, or ErrUnavailable.
func (s *Store) Preview(ctx context.Context, userID, repoID int64, number int32) (Preview, error) {
	if userID <= 0 || repoID <= 0 || number <= 0 {
		return Preview{}, ErrUnavailable
	}
	row, err := dbgen.New(s.pool).OwnedPreview(ctx, dbgen.OwnedPreviewParams{RepoID: repoID, OwnerUserID: pgtype.Int8{Int64: userID, Valid: true}, Number: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrUnavailable
	}
	if err != nil {
		return Preview{}, fmt.Errorf("read preview: %w", err)
	}
	return Preview{
		PullRequestID: row.PullRequestID, RepoID: row.RepoID, Number: row.Number, Title: row.Title, Repository: row.FullName,
		EnvironmentID: row.EnvironmentID, Live: row.Live, DeployedSHA: row.DeployedSha,
	}, nil
}

// Request notes that a viewer opened the output and reports whether a
// follower must be started.
func (s *Store) Request(ctx context.Context, environmentID int64) (bool, error) {
	state, err := dbgen.New(s.pool).RequestWatch(ctx, environmentID)
	if err != nil {
		return false, fmt.Errorf("request app output: %w", err)
	}
	return state == StateRequested, nil
}

// Touch notes that a viewer is still connected and reports whether a
// follower must be started (the previous one stopped answering).
func (s *Store) Touch(ctx context.Context, environmentID int64) (bool, error) {
	state, err := dbgen.New(s.pool).TouchWatch(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Request(ctx, environmentID)
	}
	if err != nil {
		return false, fmt.Errorf("touch app output: %w", err)
	}
	return state == StateRequested, nil
}

// Watch reads the follower's state; a zero Watch when nobody asked yet.
func (s *Store) Watch(ctx context.Context, environmentID int64) (Watch, error) {
	q := dbgen.New(s.pool)
	row, err := q.ReadWatch(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Watch{}, nil
	}
	if err != nil {
		return Watch{}, fmt.Errorf("read app output: %w", err)
	}
	w := Watch{Epoch: row.Epoch, State: row.State, Note: row.Note, NextSequence: row.NextSequence}
	if w.Epoch > 0 {
		if w.First, err = q.FirstOutputSequence(ctx, dbgen.FirstOutputSequenceParams{EnvironmentID: environmentID, Epoch: w.Epoch}); err != nil {
			return Watch{}, fmt.Errorf("read app output: %w", err)
		}
	}
	return w, nil
}

// Lines returns up to limit lines of epoch from sequence from.
func (s *Store) Lines(ctx context.Context, environmentID, epoch, from int64, limit int) ([]Line, error) {
	rows, err := dbgen.New(s.pool).OutputLines(ctx, dbgen.OutputLinesParams{EnvironmentID: environmentID, Epoch: epoch, Sequence: max(from, 0), Limit: int32(min(max(limit, 1), keepLines))})
	if err != nil {
		return nil, fmt.Errorf("read app output lines: %w", err)
	}
	lines := make([]Line, len(rows))
	for i, r := range rows {
		lines[i] = Line{Sequence: r.Sequence, At: r.At.Time, Text: r.Line}
	}
	return lines, nil
}

// LiveContainer is the container the environment's preview runs in, if it
// runs.
func (s *Store) LiveContainer(ctx context.Context, environmentID int64) (string, bool, error) {
	id, err := dbgen.New(s.pool).LiveContainer(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the live container: %w", err)
	}
	return id, true, nil
}

// Begin takes over a requested watch for a new follower, clearing earlier
// lines; false when there is nothing to do (another follower took it, or
// nobody asks).
func (s *Store) Begin(ctx context.Context, environmentID int64) (int64, bool, error) {
	var epoch int64
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if epoch, err = q.BeginFollowing(ctx, environmentID); err != nil {
			return err
		}
		return q.ClearOutputLines(ctx, environmentID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("begin following app output: %w", err)
	}
	return epoch, true, nil
}

// errFenced means another follower took over.
var errFenced = errors.New("another follower took over")

// Append stores lines and drops those beyond the newest keepLines.
func (s *Store) Append(ctx context.Context, environmentID, epoch int64, lines []Line) error {
	if len(lines) == 0 {
		return nil
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		first, err := q.AllocateOutput(ctx, dbgen.AllocateOutputParams{EnvironmentID: environmentID, Epoch: epoch, Lines: int64(len(lines))})
		if errors.Is(err, pgx.ErrNoRows) {
			return errFenced
		}
		if err != nil {
			return fmt.Errorf("allocate app output lines: %w", err)
		}
		texts, times := make([]string, len(lines)), make([]pgtype.Timestamptz, len(lines))
		for i, l := range lines {
			texts[i], times[i] = l.Text, pgtype.Timestamptz{Time: l.At, Valid: true}
		}
		if err := q.InsertOutputLines(ctx, dbgen.InsertOutputLinesParams{EnvironmentID: environmentID, Epoch: epoch, FirstSequence: first, Texts: texts, Times: times}); err != nil {
			return fmt.Errorf("store app output lines: %w", err)
		}
		next := first + int64(len(lines))
		if next > keepLines {
			if err := q.TrimOutputLines(ctx, dbgen.TrimOutputLinesParams{EnvironmentID: environmentID, Epoch: epoch, Sequence: next - keepLines}); err != nil {
				return fmt.Errorf("trim app output lines: %w", err)
			}
		}
		return nil
	})
}

// Heartbeat keeps the follower's claim and reports whether anyone watched
// within a minute; errFenced when another follower took over.
func (s *Store) Heartbeat(ctx context.Context, environmentID, epoch int64) (bool, error) {
	watched, err := dbgen.New(s.pool).FollowerHeartbeat(ctx, dbgen.FollowerHeartbeatParams{EnvironmentID: environmentID, Epoch: epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errFenced
	}
	if err != nil {
		return false, fmt.Errorf("app output heartbeat: %w", err)
	}
	return watched, nil
}

// Stop ends following with a note for viewers. forget deletes the lines too
// (nobody is watching them).
func (s *Store) Stop(ctx context.Context, environmentID, epoch int64, note string, forget bool) error {
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.StopFollowing(ctx, dbgen.StopFollowingParams{EnvironmentID: environmentID, Epoch: epoch, Note: note}); err != nil {
			return fmt.Errorf("stop following app output: %w", err)
		}
		if forget {
			return q.ClearOutputLines(ctx, environmentID)
		}
		return nil
	})
}
