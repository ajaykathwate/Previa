package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

func queuePool(t *testing.T) (*pgxpool.Pool, *slog.Logger) {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL to test River in a temporary schema")
	}
	admin, err := pgxpool.New(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("queue_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop queue test schema: %v", err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	return pool, slog.New(slog.NewTextHandler(io.Discard, nil))
}

func cleanKeys(t *testing.T, pool *pgxpool.Pool, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DELETE FROM river_job WHERE args->>'pr_key' = ANY($1::text[])", keys); err != nil {
			t.Errorf("cleanup queue test jobs: %v", err)
		}
	})
}

type otherKindArgs struct {
	PRKey string `json:"pr_key" river:"unique"`
	Note  string `json:"note"`
}

func (otherKindArgs) Kind() string { return "queue_test_other_kind" }
func (otherKindArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "other_test_queue", UniqueOpts: ActivePRUniqueOpts()}
}

func TestTransactionAndCrossKindUniquenessIntegration(t *testing.T) {
	pool, logger := queuePool(t)
	client, err := NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("queue-test:%d", time.Now().UnixNano())
	cleanKeys(t, pool, key, key+":rollback")
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback test insert: %v", err)
		}
	}()
	first, err := client.InsertTx(ctx, tx, PingArgs{PRKey: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE id = $1", first.Job.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("uncommitted job visible outside transaction: %d %v", count, err)
	}
	duplicate, err := client.InsertTx(ctx, tx, otherKindArgs{PRKey: key, Note: "ignored for uniqueness"}, nil)
	if err != nil || !duplicate.UniqueSkippedAsDuplicate || duplicate.Job.ID != first.Job.ID {
		t.Fatalf("same key across kinds/queues was not deduplicated: %+v %v", duplicate, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService(pool, client)
	id, skipped, err := service.EnqueuePing(ctx, key)
	if err != nil || !skipped || id != first.Job.ID {
		t.Fatalf("same active key inserted twice: %d %v %v", id, skipped, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	newID, skipped, err := service.EnqueuePing(ctx, key)
	if err != nil || skipped || newID == id {
		t.Fatalf("completed key was not released: %d %v %v", newID, skipped, err)
	}
	rolledBack, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job, insertErr := client.InsertTx(ctx, rolledBack, PingArgs{PRKey: key + ":rollback"}, nil)
	if err := rolledBack.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if insertErr != nil {
		t.Fatal(insertErr)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE id = $1", job.Job.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back job persisted: %d %v", count, err)
	}
}

func startWorker(t *testing.T, client *river.Client[pgx.Tx]) {
	t.Helper()
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Stop(ctx); err != nil {
			t.Errorf("stop test worker: %v", err)
		}
	})
}

func waitCompletion(t *testing.T, events <-chan *river.Event, id int64) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && event.Job.ID == id {
				return
			}
		case <-timer.C:
			t.Fatalf("job %d did not complete", id)
		}
	}
}

func TestPingRoundTripIntegration(t *testing.T) {
	pool, logger := queuePool(t)
	producer, err := NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("queue-roundtrip:%d", time.Now().UnixNano())
	cleanKeys(t, pool, key)
	id, _, err := NewService(pool, producer).EnqueuePing(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	before, err := producer.JobGet(t.Context(), id)
	if err != nil || before.State != rivertype.JobStateAvailable {
		t.Fatalf("job did not wait for worker startup: %+v %v", before, err)
	}
	worker, err := NewWorker(pool, config.WorkerConfig{MaxConcurrentBuilds: 2}, logger)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := worker.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	startWorker(t, worker)
	waitCompletion(t, events, id)
	row, err := producer.JobGet(t.Context(), id)
	if err != nil || row.State != rivertype.JobStateCompleted || row.FinalizedAt == nil || row.Attempt != 1 {
		t.Fatalf("completion was not persisted: %+v %v", row, err)
	}
}

type gatedWorker struct {
	river.WorkerDefaults[PingArgs]
	started chan int64
	release chan struct{}
}

func (w *gatedWorker) Work(ctx context.Context, job *river.Job[PingArgs]) error {
	w.started <- job.ID
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestQueueConcurrencyIntegration(t *testing.T) {
	pool, logger := queuePool(t)
	producer, err := NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := range 3 {
		keys = append(keys, fmt.Sprintf("queue-limit:%d:%d", time.Now().UnixNano(), i))
		if _, _, err := NewService(pool, producer).EnqueuePing(t.Context(), keys[i]); err != nil {
			t.Fatal(err)
		}
	}
	cleanKeys(t, pool, keys...)
	gated := &gatedWorker{started: make(chan int64, 10), release: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, gated)
	worker, err := river.NewClient(riverpgxv5.New(pool), workerConfig(config.WorkerConfig{MaxConcurrentBuilds: 2}, logger, workers))
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := worker.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	startWorker(t, worker)
	for range 2 {
		select {
		case <-gated.started:
		case <-time.After(5 * time.Second):
			t.Fatal("queue did not fill available worker slots")
		}
	}
	select {
	case <-gated.started:
		t.Fatal("queue exceeded configured concurrency")
	case <-time.After(150 * time.Millisecond):
	}
	var running int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE state = 'running' AND args->>'pr_key' = ANY($1::text[])", keys).Scan(&running); err != nil || running != 2 {
		t.Fatalf("expected two running jobs, got %d: %v", running, err)
	}
	close(gated.release)
	for range 3 {
		select {
		case <-events:
		case <-time.After(10 * time.Second):
			t.Fatal("queue did not complete jobs after releasing worker slots")
		}
	}
}

func TestInvalidPingIsPermanent(t *testing.T) {
	w := &PingWorker{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := w.Work(t.Context(), &river.Job[PingArgs]{JobRow: &rivertype.JobRow{}, Args: PingArgs{PRKey: ""}})
	if _, ok := err.(*river.JobCancelError); !ok {
		t.Fatalf("invalid input should cancel without retry, got %v", err)
	}
}
