package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
)

type recordingVerifier struct {
	mu      sync.Mutex
	calls   []int64
	answers map[int64]error
}

func (v *recordingVerifier) VerifyUpstream(_ context.Context, id int64) (environments.UpstreamSnapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, id)
	return environments.UpstreamSnapshot{PullRequestID: id}, v.answers[id]
}

func TestPullRequestsConfirmOnlyLiveWorkWithinTheInterval(t *testing.T) {
	f := newFixture(t)
	live := f.open(t, 1, "sha-live")
	f.publish(t, live, time.Hour)
	pending := f.open(t, 2, "sha-pending") // Automatic: a pending build request
	idle := f.open(t, 3, "sha-idle")
	f.exec(t, "UPDATE build_intents SET state='superseded', completed_at=now() WHERE pull_request_id=$1", idle.PullRequestID)

	verifier := &recordingVerifier{answers: map[int64]error{pending.PullRequestID: environments.ErrUpstreamChanged}}
	settings := f.settings
	settings.PullRequestBatch = 10
	p, err := NewPullRequests(f.pool, verifier, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	p.now = func() time.Time { return clock }
	pass, err := p.RunOnce(t.Context())
	if err != nil || len(verifier.calls) != 2 || pass.Outcomes["current"] != 1 || pass.Outcomes["changed"] != 1 {
		t.Fatalf("first pass %+v %v calls %v", pass, err, verifier.calls)
	}
	for _, id := range verifier.calls {
		if id == idle.PullRequestID {
			t.Fatal("a PR without live work was read from GitHub")
		}
	}
	// Within the interval nothing is read again.
	if _, err := p.RunOnce(t.Context()); err != nil || len(verifier.calls) != 2 {
		t.Fatalf("PRs read again within the interval: %v", verifier.calls)
	}
	clock = clock.Add(PullRequestInterval + time.Second)
	// GitHub's rate limit stops the pass; that PR is first in line next time.
	verifier.answers[live.PullRequestID] = &environments.UpstreamRetryAfter{Delay: time.Minute, Err: environments.ErrUpstreamUnavailable}
	pass, err = p.RunOnce(t.Context())
	if err != nil || pass.RetryAfter != time.Minute || len(verifier.calls) != 3 {
		t.Fatalf("rate-limited pass %+v %v calls %v", pass, err, verifier.calls)
	}
	var outcome string
	f.scan(t, "SELECT outcome FROM pr_reconciliations WHERE pull_request_id=$1", []any{&outcome}, live.PullRequestID)
	if outcome != "current" {
		t.Fatalf("a rate-limited read was recorded as %q", outcome)
	}
}

func TestOutcomesAreSafeCategories(t *testing.T) {
	for err, want := range map[error]string{
		nil:                                  "current",
		environments.ErrUpstreamClosed:       "closed",
		environments.ErrUpstreamInaccessible: "inaccessible",
		environments.ErrUpstreamUnavailable:  "unavailable",
		environments.ErrPRConflicts:          "blocked",
		errors.New("token=secret"):           "error",
	} {
		if got, _ := outcomeOf(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// closedUpstream reports every PR closed by GitHub, a minute after the
// recorded state.
type closedUpstream struct{ state string }

func (closedUpstream) Page(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
	return nil, 0, errors.New("not used")
}

func (s closedUpstream) Inspect(_ context.Context, _, _ int64, number int32) (pr.Upstream, error) {
	state := s.state
	if state == "" {
		state = "closed"
	}
	return pr.Upstream{Owner: "octo", Repository: "app", Observation: pr.Observation{
		Number: number, SHA: "sha-live", State: state, Title: "t", Author: "a", Branch: "b",
		HeadRepoID: 100, UpdatedAt: time.Now().Add(time.Minute),
	}}, nil
}

func TestMissedMergeAfterRebuildStillGetsConfirmed(t *testing.T) {
	f := newFixture(t)
	live := f.open(t, 1, "sha-live")
	f.publish(t, live, time.Hour)
	// Reproduce existing rows: a rebuilt preview retains a completed local teardown marker.
	f.exec(t, `UPDATE pull_requests SET teardown_requested_generation=generation,
		teardown_completed_generation=generation, teardown_reason='requested' WHERE id=$1`, live.PullRequestID)
	producer, err := queue.NewProducer(f.pool, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	sync := pr.NewSyncService(f.pool, producer, closedUpstream{state: "merged"}, f.broker)
	p, err := NewPullRequests(f.pool, sync, f.logger, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := p.RunOnce(t.Context())
	if err != nil || pass.Outcomes["closed"] != 1 {
		t.Fatalf("merged rebuilt preview was not confirmed: %+v %v", pass, err)
	}
	var state string
	f.scan(t, "SELECT state FROM pull_requests WHERE id=$1", []any{&state}, live.PullRequestID)
	if state != "merged" {
		t.Fatalf("missed merge not recorded: %s", state)
	}
	if _, err := f.recovery(t).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	f.scan(t, "SELECT args FROM river_job WHERE kind='environment_wake' AND (args->>'pull_request_id')::bigint=$1 ORDER BY id DESC LIMIT 1", []any{&raw}, live.PullRequestID)
	var wake environments.WakeArgs
	if err := json.Unmarshal(raw, &wake); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Dispatch(t.Context(), wake); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM river_job WHERE kind='teardown' AND (args->>'pull_request_id')::bigint=$1", live.PullRequestID); n != 1 {
		t.Fatalf("%d teardown jobs for the missed merge", n)
	}
}

func TestMissedCloseFoundByReconciliationTearsDown(t *testing.T) {
	f := newFixture(t)
	live := f.open(t, 1, "sha-live")
	f.publish(t, live, time.Hour)
	producer, err := queue.NewProducer(f.pool, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	sync := pr.NewSyncService(f.pool, producer, closedUpstream{}, f.broker)
	p, err := NewPullRequests(f.pool, sync, f.logger, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := p.RunOnce(t.Context())
	if err != nil || pass.Outcomes["closed"] != 1 {
		t.Fatalf("reconciliation %+v %v", pass, err)
	}
	var state string
	f.scan(t, "SELECT state FROM pull_requests WHERE id=$1", []any{&state}, live.PullRequestID)
	if state != "closed" {
		t.Fatalf("missed close not recorded: %s", state)
	}
	// Recovery turns the recorded close into a teardown.
	f.exec(t, "DELETE FROM river_job")
	if _, err := f.recovery(t).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	f.scan(t, "SELECT args FROM river_job WHERE kind='environment_wake' AND (args->>'pull_request_id')::bigint = $1", []any{&raw}, live.PullRequestID)
	var wake environments.WakeArgs
	if err := json.Unmarshal(raw, &wake); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Dispatch(t.Context(), wake); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM river_job WHERE kind='teardown' AND (args->>'pull_request_id')::bigint = $1", live.PullRequestID); n != 1 {
		t.Fatalf("%d teardown jobs for the missed close", n)
	}
}
