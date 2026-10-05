package housekeeping

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/webhooks"
)

func (f *fixture) recovery(t *testing.T) *Recovery {
	t.Helper()
	r, err := NewRecovery(f.broker, f.repo, f.sweeper, nil, f.logger, 10)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExpiryTearsDownDuePreviewsWithoutRebuildLoops(t *testing.T) {
	f := newFixture(t)
	due := f.open(t, 1, "sha-due")
	f.publish(t, due, -time.Minute)
	fresh := f.open(t, 2, "sha-fresh")
	f.publish(t, fresh, time.Hour)
	busy := f.open(t, 3, "sha-busy")
	f.publish(t, busy, -time.Minute)
	// A new commit is building for PR 3: its publication will set a new expiry.
	f.exec(t, "UPDATE build_intents SET state='running', completed_at=NULL WHERE pull_request_id=$1", busy.PullRequestID)

	expiry, err := NewExpiry(f.pool, f.repo, f.logger, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := expiry.RunOnce(t.Context())
	if err != nil || pass.Due != 1 || pass.Expired != 1 {
		t.Fatalf("expiry pass %+v %v", pass, err)
	}
	var reason, status string
	f.scan(t, `SELECT pr.teardown_reason, e.status FROM pull_requests pr JOIN environments e ON e.pull_request_id = pr.id
		WHERE pr.id = $1 AND pr.teardown_requested_generation = pr.generation`, []any{&reason, &status}, due.PullRequestID)
	if reason != "expired" || status != "tearing_down" {
		t.Fatalf("expired PR: %s/%s", reason, status)
	}
	if n := f.count(t, "SELECT count(*) FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", due.PullRequestID); n != 1 {
		t.Fatalf("%d route removals", n)
	}
	if n := f.count(t, "SELECT count(*) FROM environments WHERE pull_request_id IN ($1, $2) AND status='running'", fresh.PullRequestID, busy.PullRequestID); n != 2 {
		t.Fatal("a preview that is not due, or is being rebuilt, was expired")
	}
	// A second pass, and recovery passes, change nothing: no rebuild loop.
	if pass, err := expiry.RunOnce(t.Context()); err != nil || pass.Due != 0 {
		t.Fatalf("second pass %+v %v", pass, err)
	}
	recovery := f.recovery(t)
	for range 3 {
		if _, err := recovery.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, "SELECT count(*) FROM build_intents WHERE pull_request_id=$1 AND state IN ('pending','queued','running')", due.PullRequestID); n != 0 {
		t.Fatal("an expired preview was rebuilt without a new request")
	}
	if n := f.count(t, "SELECT count(*) FROM river_job WHERE kind='build' AND (args->>'pull_request_id')::bigint = $1", due.PullRequestID); n != 0 {
		t.Fatal("recovery queued a build for an expired preview")
	}
	// A later relevant Automatic event brings it back.
	if _, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{RepoID: f.repoID, Number: 1, HeadSHA: "sha-next", Action: "synchronize", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM build_intents WHERE pull_request_id=$1 AND state='pending'", due.PullRequestID); n != 1 {
		t.Fatal("a new commit did not request a new preview after expiry")
	}
}

func TestExpiryDryRunOnlyLogs(t *testing.T) {
	f := newFixture(t)
	due := f.open(t, 1, "sha-due")
	f.publish(t, due, -time.Minute)
	settings := f.settings
	settings.DryRun = true
	expiry, err := NewExpiry(f.pool, f.repo, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	if pass, err := expiry.RunOnce(t.Context()); err != nil || pass.Due != 1 || pass.Expired != 0 {
		t.Fatalf("dry run %+v %v", pass, err)
	}
	if n := f.count(t, "SELECT count(*) FROM environments WHERE pull_request_id=$1 AND status='running'", due.PullRequestID); n != 1 {
		t.Fatal("a dry run expired a preview")
	}
}

func TestRecoveryResumesLostJobsLeasesAndExhaustedRetries(t *testing.T) {
	f := newFixture(t)
	queued := f.open(t, 1, "sha-queued")
	f.exec(t, "UPDATE build_intents SET available_at=now()-interval '1 minute', state='queued' WHERE pull_request_id=$1", queued.PullRequestID)
	expired := f.open(t, 2, "sha-expired")
	f.publish(t, expired, -time.Minute)
	if _, err := f.repo.RequestTeardown(t.Context(), expired.PullRequestID, "expired"); err != nil {
		t.Fatal(err)
	}
	// Every River job is lost (a dropped queue, or a crash before commit of
	// another system): durable rows alone must bring the work back.
	f.exec(t, "DELETE FROM river_job")
	pass, err := f.recovery(t).RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pass.Wakeups == 0 || f.count(t, "SELECT count(*) FROM river_job WHERE kind='environment_wake' AND (args->>'pull_request_id')::bigint = $1", queued.PullRequestID) != 1 {
		t.Fatalf("lost wake-up not recovered: %+v", pass)
	}
	if pass.Operations == 0 || f.count(t, "SELECT count(*) FROM river_job WHERE kind='environment_operation'") == 0 {
		t.Fatalf("lost operation wake-ups not recovered: %+v", pass)
	}
	// A crashed worker's lease expires: the operation becomes claimable again.
	f.exec(t, "DELETE FROM river_job")
	f.exec(t, `UPDATE environment_operations SET state='running', lease_owner='crashed', lease_expires_at=now()-interval '1 second', attempts=attempts+1
		WHERE kind='remove_route' AND pull_request_id=$1`, expired.PullRequestID)
	if _, err := f.recovery(t).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state string
	f.scan(t, "SELECT state FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", []any{&state}, expired.PullRequestID)
	if state != "retryable" && state != "pending" {
		t.Fatalf("expired lease left the removal %s", state)
	}
	// A running build whose job exhausted its retries without its handoff
	// (the worker died during the last attempt) is dispatched again.
	f.exec(t, "DELETE FROM river_job")
	f.exec(t, "UPDATE build_intents SET state='running', started_at=now() WHERE pull_request_id=$1", queued.PullRequestID)
	f.exec(t, `INSERT INTO river_job(kind, args, queue, state, attempt, max_attempts, finalized_at, priority)
		VALUES ('build', jsonb_build_object('pull_request_id', $1::bigint), 'build', 'discarded', 5, 5, now(), 1)`, queued.PullRequestID)
	if _, err := f.recovery(t).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM river_job WHERE kind='environment_wake' AND (args->>'pull_request_id')::bigint = $1", queued.PullRequestID); n != 1 {
		t.Fatalf("stuck running build not recovered (%d wake-ups)", n)
	}
	var wake environments.WakeArgs
	var raw []byte
	f.scan(t, "SELECT args FROM river_job WHERE kind='environment_wake' AND (args->>'pull_request_id')::bigint = $1", []any{&raw}, queued.PullRequestID)
	if err := json.Unmarshal(raw, &wake); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Dispatch(t.Context(), wake); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM river_job WHERE kind='build' AND state <> 'discarded' AND (args->>'pull_request_id')::bigint = $1", queued.PullRequestID); n != 1 {
		t.Fatalf("the recovered wake-up queued %d builds", n)
	}
}
