package environments_test

import (
	"context"
	"errors"
	"fmt"
	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"testing"
)

func TestEndedExecutionBecomesFailedInsteadOfRequeued(t *testing.T) {
	for _, state := range []string{"discarded", "cancelled", "completed"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			r := f.event(t, "opened", "sha-a")
			var jobID int64
			if err := f.pool.QueryRow(t.Context(), "SELECT execution_job_id FROM build_intents WHERE pull_request_id=$1", r.PullRequestID).Scan(&jobID); err != nil || jobID <= 0 {
				t.Fatalf("execution link: %d %v", jobID, err)
			}
			if _, err := f.pool.Exec(t.Context(), "UPDATE river_job SET state=$1,finalized_at=now() WHERE id=$2", state, jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET state='running',started_at=now()-interval '1 minute' WHERE pull_request_id=$1", r.PullRequestID); err != nil {
				t.Fatal(err)
			}
			wake := env.WakeArgs{Key: fmt.Sprintf("wake:pr:%d:g:%d:r:%d", r.PullRequestID, r.Generation, r.Revision), PullRequestID: r.PullRequestID, Generation: r.Generation, Revision: r.Revision}
			if err := f.broker.Dispatch(t.Context(), wake); err != nil {
				t.Fatal(err)
			}
			var saved, code string
			if err := f.pool.QueryRow(t.Context(), "SELECT state,execution_error_code FROM build_intents WHERE pull_request_id=$1", r.PullRequestID).Scan(&saved, &code); err != nil {
				t.Fatal(err)
			}
			if saved != "failed" || code != "worker_failed" {
				t.Fatalf("terminal execution left active: %s %s", saved, code)
			}
			if err := f.broker.Dispatch(t.Context(), wake); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind='build'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("requeued terminal revision: %d %v", count, err)
			}
		})
	}
}
func TestFinalizationErrorHasFailureFallbackWithoutRepairingDatabase(t *testing.T) {
	f := newFixture(t)
	r := f.prEvent(t, 71, "sha-fallback")
	// Simulate the screenshot's missing function only in this disposable schema.
	if _, err := f.pool.Exec(t.Context(), "DROP FUNCTION workspace_publication_authorized(bigint)"); err != nil {
		t.Fatal(err)
	}
	runtime := newGatedRuntime()
	runtime.refuse[r.PullRequestID] = failing{code: "source_failed", permanent: true}
	state, _ := runBuildJob(t, f, newCapacityService(t, f, runtime), r, 1)
	if state != "discarded" {
		t.Fatalf("job: %s", state)
	}
	saved, _ := f.intentState(t, r.PullRequestID)
	if saved != "failed" {
		t.Fatalf("finalization error stranded build: %s", saved)
	}
	var code string
	if err := f.pool.QueryRow(t.Context(), "SELECT execution_error_code FROM build_intents WHERE pull_request_id=$1", r.PullRequestID).Scan(&code); err != nil || code != "worker_failed" {
		t.Fatalf("failure notice: %s %v", code, err)
	}
}
func TestOldFinalAttemptCannotFailANewerRequest(t *testing.T) {
	f := newFixture(t)
	old := f.event(t, "opened", "sha-old")
	newer := f.event(t, "synchronize", "sha-new")
	runtime := newGatedRuntime()
	runtime.refuse[old.PullRequestID] = errors.New("old error")
	worker := env.BuildWorker{Service: newCapacityService(t, f, runtime), Broker: f.broker}
	// The stale service returns before execution; a stale completion must leave
	// the newer queued request untouched regardless of its attempt number.
	_ = worker.Work(context.Background(), &river.Job[env.BuildArgs]{Args: env.BuildArgs(old), JobRow: &rivertype.JobRow{ID: -1, Attempt: 5, MaxAttempts: 5}})
	var state string
	var revision int64
	if err := f.pool.QueryRow(t.Context(), "SELECT state,revision FROM build_intents WHERE pull_request_id=$1", newer.PullRequestID).Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	if revision != newer.Revision || (state != "queued" && state != "pending") {
		t.Fatalf("stale failure replaced successor: %d %s", revision, state)
	}
}
