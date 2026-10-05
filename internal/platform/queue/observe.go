package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/logging"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// observeJobs correlates and measures every worked job: its logs carry the
// job and, when its arguments name them, the PR, commit and request version;
// metrics record queue wait, duration and outcome by kind.
type observeJobs struct{ river.MiddlewareDefaults }

// correlation is the subset of job arguments worth putting on every log line.
type correlation struct {
	PullRequestID int64  `json:"pull_request_id"`
	Generation    *int64 `json:"generation"`
	Revision      *int64 `json:"revision"`
	HeadSHA       string `json:"head_sha"`
	OperationID   int64  `json:"operation_id"`
	RepoID        int64  `json:"repo_id"`
}

func (*observeJobs) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if job.AttemptedAt != nil {
		metrics.JobStarted(job.Queue, job.AttemptedAt.Sub(job.ScheduledAt))
	}
	attrs := []slog.Attr{slog.Int64("job_id", job.ID), slog.String("job_kind", job.Kind), slog.Int("job_attempt", job.Attempt)}
	var c correlation
	if json.Unmarshal(job.EncodedArgs, &c) == nil {
		if c.PullRequestID > 0 {
			attrs = append(attrs, slog.Int64("pr_id", c.PullRequestID))
		}
		if c.Generation != nil {
			attrs = append(attrs, slog.Int64("generation", *c.Generation))
		}
		if c.Revision != nil {
			attrs = append(attrs, slog.Int64("revision", *c.Revision))
		}
		if len(c.HeadSHA) >= 7 && len(c.HeadSHA) <= 64 {
			attrs = append(attrs, slog.String("sha", c.HeadSHA))
		}
		if c.OperationID > 0 {
			attrs = append(attrs, slog.Int64("operation_id", c.OperationID))
		}
		if c.RepoID > 0 {
			attrs = append(attrs, slog.Int64("repo_id", c.RepoID))
		}
	}
	start := time.Now()
	err := doInner(logging.With(ctx, attrs...))
	metrics.JobFinished(job.Kind, jobOutcome(err), time.Since(start))
	return err
}

func jobOutcome(err error) string {
	var snooze *rivertype.JobSnoozeError
	var cancel *rivertype.JobCancelError
	switch {
	case err == nil:
		return "succeeded"
	case errors.As(err, &snooze):
		return "snoozed"
	case errors.As(err, &cancel):
		return "cancelled"
	default:
		return "failed"
	}
}

// QueueStat is the number of jobs of one queue in one state, and for the
// available state how long the oldest has been due.
type QueueStat struct {
	Queue, State string
	Jobs         int64
	OldestDue    time.Duration
}

// Stats reads queue depth from River's table: unfinished jobs per queue and
// state. It reads River's schema directly and only counts rows.
func Stats(ctx context.Context, pool *pgxpool.Pool) ([]QueueStat, error) {
	rows, err := pool.Query(ctx, `SELECT queue, state::text, count(*),
		COALESCE(EXTRACT(EPOCH FROM now() - min(scheduled_at) FILTER (WHERE state = 'available')), 0)::float8
		FROM river_job WHERE state IN ('available', 'scheduled', 'retryable', 'running')
		GROUP BY queue, state`)
	if err != nil {
		return nil, fmt.Errorf("read queue depth: %w", err)
	}
	defer rows.Close()
	var stats []QueueStat
	for rows.Next() {
		var s QueueStat
		var oldest float64
		if err := rows.Scan(&s.Queue, &s.State, &s.Jobs, &oldest); err != nil {
			return nil, fmt.Errorf("read queue depth: %w", err)
		}
		s.OldestDue = time.Duration(oldest * float64(time.Second))
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queue depth: %w", err)
	}
	return stats, nil
}
