package buildlogs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/buildlogs/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A repository baseline refresh (docs/32-persistent-test-data.md) has a build
// log too: the same writer, caps and redaction as a pull request attempt's,
// stored per refresh in baseline_logs. The refresh row keeps the counters and
// fences writers: once the refresh finishes or waits, a late writer stores
// nothing.

// OpenBaseline binds a writer to a running refresh's log, continuing what an
// earlier writer of the same refresh stored. It fails with ErrSuperseded when
// the refresh is not running.
func (s *Store) OpenBaseline(ctx context.Context, refreshID int64, redactor *secrets.Redactor) (*Writer, error) {
	if redactor == nil {
		return nil, errors.New("build log writer requires a redactor")
	}
	row, err := dbgen.New(s.pool).OpenBaselineStream(ctx, refreshID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSuperseded
	}
	if err != nil {
		return nil, fmt.Errorf("open baseline log: %w", err)
	}
	return newWriter(baselineAppender{pool: s.pool, refreshID: refreshID}, Binding{}, stored{lines: row.LogLines, bytes: row.LogBytes, truncated: row.LogTruncated},
		redactor, DefaultLimits(), s.now), nil
}

// baselineAppender stores a refresh's batches.
type baselineAppender struct {
	pool      *pgxpool.Pool
	refreshID int64
}

func (a baselineAppender) append(ctx context.Context, _ Binding, b batch) error {
	return database.InTransaction(ctx, a.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		first, err := q.AllocateBaselineLines(ctx, dbgen.AllocateBaselineLinesParams{
			Lines: int64(len(b.texts)), Bytes: b.bytes, Truncated: b.truncated, Omitted: b.omitted, Lost: b.lost, RefreshID: a.refreshID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSuperseded
		}
		if err != nil {
			return fmt.Errorf("allocate baseline log sequences: %w", err)
		}
		if len(b.texts) == 0 {
			return nil
		}
		times := make([]pgtype.Timestamptz, len(b.times))
		for i, t := range b.times {
			times[i] = pgtype.Timestamptz{Time: t, Valid: true}
		}
		if err := q.InsertBaselineLines(ctx, dbgen.InsertBaselineLinesParams{RefreshID: a.refreshID, FirstSequence: first, Kinds: b.kinds, Texts: b.texts, Times: times}); err != nil {
			return fmt.Errorf("store baseline log lines: %w", err)
		}
		return nil
	})
}

// BaselineLinesBefore returns up to limit matching lines before a sequence,
// in order.
func (s *Store) BaselineLinesBefore(ctx context.Context, refreshID, before int64, limit int, filter Filter) ([]Line, error) {
	rows, err := dbgen.New(s.pool).BaselineLinesBefore(ctx, dbgen.BaselineLinesBeforeParams{RefreshID: refreshID, Position: before,
		Kind: filter.kind(), Query: filter.Query, MaxLines: int32(min(max(limit, 1), maxReadLines))})
	if err != nil {
		return nil, fmt.Errorf("read baseline log lines: %w", err)
	}
	lines := make([]Line, len(rows))
	for i, r := range rows {
		lines[len(rows)-1-i] = Line{Sequence: r.Sequence, System: r.Kind == kindSystem, Text: r.Line, At: r.CreatedAt.Time}
	}
	return lines, nil
}

// BaselineLinesFrom returns up to limit lines from a sequence, in order.
func (s *Store) BaselineLinesFrom(ctx context.Context, refreshID, from int64, limit int) ([]Line, error) {
	rows, err := dbgen.New(s.pool).BaselineLinesFrom(ctx, dbgen.BaselineLinesFromParams{RefreshID: refreshID, Sequence: max(from, 0), Limit: int32(min(max(limit, 1), maxReadLines))})
	if err != nil {
		return nil, fmt.Errorf("read baseline log lines: %w", err)
	}
	lines := make([]Line, len(rows))
	for i, r := range rows {
		lines[i] = Line{Sequence: r.Sequence, System: r.Kind == kindSystem, Text: r.Line, At: r.CreatedAt.Time}
	}
	return lines, nil
}

// ParseFilter reads a log page's source and search parameters.
func ParseFilter(source, query string) (Filter, error) {
	return parseFilter(map[string][]string{"source": {source}, "q": {query}})
}
