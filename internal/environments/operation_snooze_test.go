package environments_test

import (
	"context"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestOperationBackoffSnoozesRiverWithoutSpendingAttempt(t *testing.T) {
	f := newFixture(t)
	r := f.event(t, "opened", "sha-a")
	f.provision(t, r)
	if applied, err := f.repo.Apply(t.Context(), r, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "candidate", ImageTag: "image"}}); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	var id int64
	if err := f.pool.QueryRow(t.Context(), "UPDATE environment_operations SET available_at=now()+interval '1 minute' WHERE kind='activate_route' RETURNING id").Scan(&id); err != nil {
		t.Fatal(err)
	}
	worker, err := env.NewOperationWorker(f.repo, &fakePublisher{}, f.logger, time.Minute, map[env.OperationKind]time.Duration{
		env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	c, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers,
		Queues: map[string]river.QueueConfig{env.RouteOperationQueue: {MaxWorkers: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		var attempt, failures, durableAttempts int
		var delayed bool
		if err := f.pool.QueryRow(t.Context(), `SELECT j.state, j.attempt, COALESCE(cardinality(j.errors), 0),
			j.scheduled_at > now()+interval '40 seconds', o.attempts
			FROM river_job j JOIN environment_operations o ON o.id=$1
			WHERE j.kind='environment_operation' AND j.args->>'operation_id'=$1::text`, id).
			Scan(&state, &attempt, &failures, &delayed, &durableAttempts); err != nil {
			t.Fatal(err)
		}
		if state == "scheduled" && delayed {
			if attempt != 0 || failures != 0 || durableAttempts != 0 {
				t.Fatalf("waiting consumed an attempt: River=%d errors=%d operation=%d", attempt, failures, durableAttempts)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("River did not snooze the early operation")
}
