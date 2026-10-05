package environments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/webhooks"
)

// prEvent opens PR number at sha with an eligible Automatic request and a
// provisioned environment.
func (f *fixture) prEvent(t *testing.T, number int32, sha string) env.Request {
	t.Helper()
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: number, HeadSHA: sha, Action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET available_at=now()-interval '1 second' WHERE pull_request_id=$1", r.PullRequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO environments(pull_request_id,subdomain) VALUES($1,$2)", r.PullRequestID, fmt.Sprintf("preview-%d", number)); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) intentState(t *testing.T, pr int64) (state, reason string) {
	t.Helper()
	var waiting *string
	if err := f.pool.QueryRow(t.Context(), "SELECT state, waiting_reason FROM build_intents WHERE pull_request_id=$1", pr).Scan(&state, &waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != nil {
		reason = *waiting
	}
	return state, reason
}

// gatedRuntime blocks each PR's build until released and can refuse with a
// classified failure.
type gatedRuntime struct {
	*fakeRuntime
	mu      sync.Mutex
	gates   map[int64]chan struct{}
	started chan int64
	refuse  map[int64]error
}

func newGatedRuntime() *gatedRuntime {
	return &gatedRuntime{fakeRuntime: newRuntime(), gates: map[int64]chan struct{}{}, started: make(chan int64, 20), refuse: map[int64]error{}}
}

func (r *gatedRuntime) Build(ctx context.Context, request env.Request, attempt env.Attempt) (env.Artifact, error) {
	r.mu.Lock()
	gate, refusal := r.gates[request.PullRequestID], r.refuse[request.PullRequestID]
	r.mu.Unlock()
	r.started <- request.PullRequestID
	if refusal != nil {
		return env.Artifact{}, refusal
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return env.Artifact{}, ctx.Err()
		}
	}
	return env.Artifact{ContainerID: fmt.Sprintf("container-%d-%d", request.PullRequestID, request.Revision), ImageTag: fmt.Sprintf("image-%d-%d", request.PullRequestID, request.Revision)}, nil
}

type classified string

func (c classified) Error() string       { return string(c) }
func (c classified) FailureCode() string { return string(c) }

func newCapacityService(t *testing.T, f *fixture, r env.Runtime) *env.Service {
	t.Helper()
	s, err := env.NewService(f.repo, r, f.logger, queue.DefaultBuildTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildSlotsKeepLaterRequestsQueuedAndWaiting(t *testing.T) {
	f := newFixture(t)
	f.repo.WithBuildSlots(1).WithReports()
	a, b := f.prEvent(t, 11, "sha-a"), f.prEvent(t, 12, "sha-b")
	r := newGatedRuntime()
	r.gates[a.PullRequestID] = make(chan struct{})
	s := newCapacityService(t, f, r)
	done := make(chan error, 1)
	go func() { done <- s.Build(t.Context(), a) }()
	<-r.started

	var waiting *env.WaitingError
	if err := s.Build(t.Context(), b); !errors.As(err, &waiting) || waiting.Reason != env.WaitCapacity {
		t.Fatalf("second build with one slot: %v", err)
	}
	if state, reason := f.intentState(t, b.PullRequestID); state != "queued" && state != "pending" || reason != env.WaitCapacity {
		t.Fatalf("waiting request is %s/%q", state, reason)
	}
	var reports int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND pull_request_id=$1", b.PullRequestID).Scan(&reports); err != nil || reports == 0 {
		t.Fatalf("the wait was not reported: %d %v", reports, err)
	}
	// Waiting again changes nothing and reports nothing new.
	if err := s.Build(t.Context(), b); !errors.As(err, &waiting) {
		t.Fatal(err)
	}
	var again int
	_ = f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND pull_request_id=$1", b.PullRequestID).Scan(&again)
	if again != reports {
		t.Fatalf("a repeated wait reported again: %d -> %d", reports, again)
	}

	close(r.gates[a.PullRequestID])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A's final attempt ends (the job handoff marks it succeeded): its slot frees.
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET state='succeeded', completed_at=now() WHERE pull_request_id=$1", a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if state, reason := f.intentState(t, b.PullRequestID); state != "running" || reason != "" {
		t.Fatalf("admitted request is %s/%q", state, reason)
	}
}

func TestConcurrentAdmissionsNeverExceedTheSlots(t *testing.T) {
	f := newFixture(t)
	f.repo.WithBuildSlots(2)
	r := newGatedRuntime()
	gate := make(chan struct{})
	var requests []env.Request
	for i := range 6 {
		req := f.prEvent(t, int32(20+i), fmt.Sprintf("sha-%d", i))
		r.gates[req.PullRequestID] = gate
		requests = append(requests, req)
	}
	s := newCapacityService(t, f, r)
	results := make(chan error, len(requests))
	for _, req := range requests {
		go func() { results <- s.Build(t.Context(), req) }()
	}
	waited := 0
	for range len(requests) - 2 {
		var waiting *env.WaitingError
		if err := <-results; !errors.As(err, &waiting) {
			t.Fatalf("expected a wait, got %v", err)
		}
		waited++
	}
	var running int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM build_intents WHERE state='running'").Scan(&running); err != nil || running != 2 {
		t.Fatalf("%d builds running with two slots (%v)", running, err)
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if waited != 4 {
		t.Fatalf("%d requests waited", waited)
	}
}

func TestRuntimeCapacityRefusalWaitsInsteadOfFailing(t *testing.T) {
	f := newFixture(t)
	f.repo.WithReports()
	a := f.prEvent(t, 31, "sha-a")
	r := newGatedRuntime()
	r.refuse[a.PullRequestID] = classified("capacity")
	s := newCapacityService(t, f, r)
	var waiting *env.WaitingError
	if err := s.Build(t.Context(), a); !errors.As(err, &waiting) || waiting.Reason != env.WaitCapacity {
		t.Fatalf("capacity refusal: %v", err)
	}
	if state, reason := f.intentState(t, a.PullRequestID); state != "running" || reason != env.WaitCapacity {
		t.Fatalf("admitted request is %s/%q", state, reason)
	}
	var status string
	var code *string
	if err := f.pool.QueryRow(t.Context(), "SELECT status, last_error_code FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&status, &code); err != nil || status == "failed" || code != nil {
		t.Fatalf("a wait was recorded as a failure: %s %v %v", status, code, err)
	}
	// Disk pressure is a wait too.
	r.refuse[a.PullRequestID] = classified("disk_low")
	if err := s.Build(t.Context(), a); !errors.As(err, &waiting) || waiting.Reason != env.WaitDisk {
		t.Fatalf("disk refusal: %v", err)
	}
	// Once capacity frees up the same admitted request goes through.
	delete(r.refuse, a.PullRequestID)
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if state, reason := f.intentState(t, a.PullRequestID); state != "running" || reason != "" {
		t.Fatalf("after the result: %s/%q", state, reason)
	}
}

type refusingHeadroom struct{ reason string }

func (h refusingHeadroom) Admit(context.Context, env.Request) error {
	return &env.WaitingError{Reason: h.reason}
}

func TestHeadroomRefusalKeepsTheRequestQueued(t *testing.T) {
	f := newFixture(t)
	a := f.prEvent(t, 41, "sha-a")
	r := newGatedRuntime()
	s := newCapacityService(t, f, r).WithHeadroom(refusingHeadroom{env.WaitDisk})
	var waiting *env.WaitingError
	if err := s.Build(t.Context(), a); !errors.As(err, &waiting) || waiting.Reason != env.WaitDisk {
		t.Fatalf("headroom refusal: %v", err)
	}
	if state, reason := f.intentState(t, a.PullRequestID); state == "running" || reason != env.WaitDisk {
		t.Fatalf("request is %s/%q", state, reason)
	}
	select {
	case <-r.started:
		t.Fatal("a refused build reached the runtime")
	default:
	}
}

func TestAWorkerShutdownMidBuildIsNoFailure(t *testing.T) {
	f := newFixture(t)
	a := f.prEvent(t, 71, "sha-a")
	r := newGatedRuntime()
	r.gates[a.PullRequestID] = make(chan struct{})
	s := newCapacityService(t, f, r)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Build(ctx, a) }()
	<-r.started
	cancel() // the worker stops while the build runs
	if err := <-done; !errors.Is(err, env.ErrInterrupted) {
		t.Fatalf("an interrupted build returned %v", err)
	}
	if state, _ := f.intentState(t, a.PullRequestID); state != "running" {
		t.Fatalf("an interrupted request is %s; it must run again", state)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), "SELECT status FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&status); err != nil || status == "failed" {
		t.Fatalf("an interruption was recorded as a failure: %s %v", status, err)
	}
}
