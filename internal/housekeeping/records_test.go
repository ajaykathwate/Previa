package housekeeping

import (
	"fmt"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
)

// retire makes PR id closed, last updated on GitHub age ago, with its
// teardown finished and its operations and report done.
func (f *fixture) retire(t *testing.T, id int64, age time.Duration) {
	t.Helper()
	f.exec(t, `UPDATE environment_operations SET state='succeeded', completed_at=now() WHERE pull_request_id=$1 AND state IN ('pending','running','retryable')`, id)
	f.exec(t, `UPDATE pull_request_reports SET reported_version=wanted_version WHERE pull_request_id=$1`, id)
	f.exec(t, `UPDATE build_intents SET state='superseded', completed_at=now() WHERE pull_request_id=$1`, id)
	f.exec(t, `UPDATE pull_requests SET state='closed', source_updated_at=now()-$2::bigint*interval '1 second',
		generation=generation+1, teardown_completed_generation=generation+1 WHERE id=$1`, id, int64(age/time.Second))
	f.exec(t, `UPDATE environments SET status='torn_down', container_id=NULL WHERE pull_request_id=$1`, id)
}

// operation records a finished operation of PR id, age old.
func (f *fixture) operation(t *testing.T, id int64, kind, state string, age time.Duration) {
	t.Helper()
	f.exec(t, `INSERT INTO environment_operations(pull_request_id, generation, kind, idempotency_key, payload, state, completed_at, updated_at)
		SELECT id, greatest(generation, 1), $2, $2||'-'||$3||'-'||id::text||'-'||$4::bigint::text, '{}'::jsonb, $3, now()-$4::bigint*interval '1 second', now()
		FROM pull_requests WHERE id=$1`, id, kind, state, int64(age/time.Second))
}

// Finished history goes when its retention ends; anything still in use, or
// newer, stays, and a dry run only counts.
func TestRecordsDeleteOnlyFinishedHistoryPastItsRetention(t *testing.T) {
	f := newFixture(t)
	const day = 24 * time.Hour
	old, unfinished, recent, open := f.open(t, 1, "sha-old"), f.open(t, 2, "sha-unfinished"), f.open(t, 3, "sha-recent"), f.open(t, 4, "sha-open")
	for _, r := range []environments.Request{old, unfinished, recent, open} {
		f.publish(t, r, time.Hour)
	}
	f.retire(t, old.PullRequestID, 100*day)
	f.retire(t, unfinished.PullRequestID, 100*day)
	f.exec(t, "UPDATE pull_requests SET teardown_completed_generation=generation-1 WHERE id=$1", unfinished.PullRequestID)
	f.retire(t, recent.PullRequestID, 10*day)
	// Everything recorded about the old PR (it also has a report).
	f.exec(t, `INSERT INTO build_log_streams(environment_id, epoch, generation, revision, head_sha, state)
		SELECT id, 1, 1, 1, 'sha-old', 'open' FROM environments WHERE pull_request_id=$1`, old.PullRequestID)
	f.exec(t, `INSERT INTO build_logs(environment_id, epoch, sequence, kind, line)
		SELECT id, 1, 0, 'output', 'hello' FROM environments WHERE pull_request_id=$1`, old.PullRequestID)
	f.exec(t, `INSERT INTO pr_reconciliations(pull_request_id, reconciled_at, outcome) VALUES ($1, now(), 'closed')`, old.PullRequestID)
	f.exec(t, `INSERT INTO preview_components(pull_request_id, generation, revision) VALUES ($1, 1, 1)`, old.PullRequestID)
	f.exec(t, `INSERT INTO build_failure_dismissals(user_id, pull_request_id, revision) SELECT id, $1, 1 FROM users LIMIT 1`, old.PullRequestID)
	f.operation(t, old.PullRequestID, "remove_route", "succeeded", 95*day)
	// Operations of a live PR: an old report goes; its current route
	// operation and a failed cleanup stay; a recent one stays.
	f.operation(t, open.PullRequestID, "report_status", "succeeded", 31*day)
	f.operation(t, open.PullRequestID, "activate_route", "succeeded", 31*day)
	f.operation(t, open.PullRequestID, "cleanup_artifact", "failed", 31*day)
	f.operation(t, open.PullRequestID, "report_status", "succeeded", 2*day)
	f.exec(t, `INSERT INTO webhook_deliveries(delivery_id, event_type, payload_hash, received_at) VALUES
		('old', 'pull_request', 'h', now()-interval '31 days'), ('new', 'pull_request', 'h', now()-interval '1 day')`)
	f.exec(t, `INSERT INTO account_build_usage(user_id, day, build_seconds, builds)
		SELECT id, (now()-interval '100 days')::date, 60, 1 FROM users UNION ALL SELECT id, now()::date, 60, 1 FROM users`)

	snapshot := func() string {
		return fmt.Sprint(
			f.count(t, "SELECT count(*) FROM pull_requests"), f.count(t, "SELECT count(*) FROM environments"),
			f.count(t, "SELECT count(*) FROM environment_operations"), f.count(t, "SELECT count(*) FROM webhook_deliveries"),
			f.count(t, "SELECT count(*) FROM account_build_usage"), f.count(t, "SELECT count(*) FROM build_logs"))
	}
	before := snapshot()
	settings := f.settings
	settings.DryRun = true
	dry, err := NewRecords(f.pool, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := dry.RunOnce(t.Context())
	if err != nil || pass != (RecordsPass{Deliveries: 1, Operations: 2, Usage: 1, PullRequests: 1}) || snapshot() != before {
		t.Fatalf("a dry run counts and keeps: %+v %v (%s → %s)", pass, err, before, snapshot())
	}

	records, err := NewRecords(f.pool, f.logger, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	if pass, err = records.RunOnce(t.Context()); err != nil || pass != (RecordsPass{Deliveries: 1, Operations: 2, Usage: 1, PullRequests: 1}) {
		t.Fatalf("pass: %+v %v", pass, err)
	}
	if n := f.count(t, "SELECT count(*) FROM pull_requests WHERE id=$1", old.PullRequestID); n != 0 {
		t.Fatal("the retired PR was kept")
	}
	for _, table := range []string{"environments", "environment_operations", "build_intents", "pull_request_reports", "pr_reconciliations", "preview_components", "build_failure_dismissals"} {
		if n := f.count(t, "SELECT count(*) FROM "+table+" WHERE pull_request_id=$1", old.PullRequestID); n != 0 {
			t.Errorf("%s of the retired PR was kept", table)
		}
	}
	if n := f.count(t, "SELECT count(*) FROM build_logs") + f.count(t, "SELECT count(*) FROM build_log_streams"); n != 0 {
		t.Error("the retired PR's logs were kept")
	}
	if n := f.count(t, "SELECT count(*) FROM pull_requests"); n != 3 {
		t.Fatalf("an unfinished teardown, a recent close and an open PR stay: %d PRs", n)
	}
	if got := f.count(t, "SELECT count(*) FROM environment_operations WHERE pull_request_id=$1 AND completed_at < now()-interval '1 day'", open.PullRequestID); got != 3 {
		t.Fatalf("the live PR keeps its route and failed cleanup operations and its recent one: %d", got)
	}
	if f.count(t, "SELECT count(*) FROM webhook_deliveries WHERE delivery_id='new'") != 1 || f.count(t, "SELECT count(*) FROM webhook_deliveries") != 1 {
		t.Fatal("only old deliveries go")
	}
	if f.count(t, "SELECT count(*) FROM account_build_usage") != 1 {
		t.Fatal("only old usage goes")
	}
	if pass, err = records.RunOnce(t.Context()); err != nil || pass != (RecordsPass{}) {
		t.Fatalf("a second pass has nothing to do: %+v %v", pass, err)
	}
}
