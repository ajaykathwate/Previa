package environments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// failing is a classified runtime failure.
type failing struct {
	code      string
	permanent bool
}

func (f failing) Error() string       { return f.code }
func (f failing) FailureCode() string { return f.code }
func (f failing) Permanent() bool     { return f.permanent }

// runBuildJob works one build job for request through a real River client and
// returns the job's state and attempt once River has recorded the outcome.
func runBuildJob(t *testing.T, f *fixture, s *env.Service, request env.Request, maxAttempts ...int) (string, int) {
	t.Helper()
	workers := river.NewWorkers()
	env.RegisterWorkers(workers, s, f.broker)
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers,
		Queues: map[string]river.QueueConfig{env.BuildQueue: {MaxWorkers: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var opts *river.InsertOpts
	if len(maxAttempts) > 0 {
		opts = &river.InsertOpts{MaxAttempts: maxAttempts[0]}
	}
	inserted, err := client.Insert(t.Context(), env.BuildArgs(request), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(ctx)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		var attempt int
		if err := f.pool.QueryRow(t.Context(), "SELECT state, attempt FROM river_job WHERE id=$1", inserted.Job.ID).Scan(&state, &attempt); err != nil {
			t.Fatal(err)
		}
		if state != "available" && state != "running" {
			return state, attempt
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the build job did not finish")
	return "", 0
}

func TestPermanentFailuresEndAfterOneAttempt(t *testing.T) {
	f := newFixture(t)
	a := f.prEvent(t, 51, "sha-a")
	r := newGatedRuntime()
	r.refuse[a.PullRequestID] = failing{code: "build_failed", permanent: true}
	state, attempt := runBuildJob(t, f, newCapacityService(t, f, r), a)
	if state != "discarded" || attempt != 1 {
		t.Fatalf("a permanent failure left the job %s after %d attempts", state, attempt)
	}
	if intent, _ := f.intentState(t, a.PullRequestID); intent != "failed" {
		t.Fatalf("request is %s after a permanent failure", intent)
	}
	var code string
	if err := f.pool.QueryRow(t.Context(), "SELECT last_error_code FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&code); err != nil || code != "build_failed" {
		t.Fatalf("failure category %q %v", code, err)
	}

	b := f.prEvent(t, 52, "sha-b")
	r.refuse[b.PullRequestID] = failing{code: "runtime_unavailable"}
	state, attempt = runBuildJob(t, f, newCapacityService(t, f, r), b)
	if state != "retryable" || attempt >= 5 {
		t.Fatalf("a retryable failure left the job %s after %d attempts", state, attempt)
	}
	if intent, _ := f.intentState(t, b.PullRequestID); intent != "running" {
		t.Fatalf("request is %s while its retry is pending", intent)
	}
}

type scriptedVerifier struct {
	snapshot func(env.Request) env.UpstreamSnapshot
	err      error
	calls    int
}

func (v *scriptedVerifier) VerifyUpstream(_ context.Context, id int64) (env.UpstreamSnapshot, error) {
	v.calls++
	return v.snapshot(env.Request{PullRequestID: id}), v.err
}

func TestAdmissionReadsThePRFreshFromGitHub(t *testing.T) {
	f := newFixture(t)
	a := f.prEvent(t, 61, "sha-a")
	matching := func(env.Request) env.UpstreamSnapshot {
		return env.UpstreamSnapshot{PullRequestID: a.PullRequestID, Generation: a.Generation, HeadSHA: a.HeadSHA}
	}
	r := newGatedRuntime()
	s := newCapacityService(t, f, r)
	for name, tc := range map[string]struct {
		verifier  *scriptedVerifier
		permanent bool
		retried   bool
	}{
		"closed upstream": {verifier: &scriptedVerifier{snapshot: matching, err: env.ErrUpstreamClosed}},
		"newer commit": {verifier: &scriptedVerifier{snapshot: func(env.Request) env.UpstreamSnapshot {
			return env.UpstreamSnapshot{PullRequestID: a.PullRequestID, Generation: a.Generation + 1, HeadSHA: "sha-new"}
		}}},
		"access removed":     {verifier: &scriptedVerifier{snapshot: matching, err: env.ErrUpstreamInaccessible}, permanent: true},
		"GitHub unavailable": {verifier: &scriptedVerifier{snapshot: matching, err: env.ErrUpstreamUnavailable}, retried: true},
	} {
		t.Run(name, func(t *testing.T) {
			s.WithVerifier(tc.verifier)
			err := s.Build(t.Context(), a)
			switch {
			case tc.permanent:
				var p interface{ Permanent() bool }
				if !errors.As(err, &p) || !p.Permanent() {
					t.Fatalf("lost access is not final: %v", err)
				}
			case tc.retried:
				if !errors.Is(err, env.ErrUpstreamUnavailable) {
					t.Fatalf("an outage is not retried: %v", err)
				}
			case err != nil:
				t.Fatalf("a superseded request failed: %v", err)
			}
			if tc.verifier.calls != 1 {
				t.Fatal("GitHub was not consulted")
			}
			select {
			case <-r.started:
				t.Fatal("a request GitHub no longer confirms reached the runtime")
			default:
			}
		})
	}
	s.WithVerifier(&scriptedVerifier{snapshot: matching})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if got := <-r.started; got != a.PullRequestID {
		t.Fatal("a confirmed request did not build")
	}
}

// TestPausedBuildsWaitWithoutTouchingGitHubOrDocker: the kill switch defers
// every build before any external call, records why, and lets the same
// request run once builds resume.
func TestPausedBuildsWaitWithoutTouchingGitHubOrDocker(t *testing.T) {
	f := newFixture(t)
	f.repo.WithReports()
	a := f.prEvent(t, 81, "sha-a")
	verifier := &scriptedVerifier{snapshot: func(env.Request) env.UpstreamSnapshot {
		return env.UpstreamSnapshot{PullRequestID: a.PullRequestID, Generation: a.Generation, HeadSHA: a.HeadSHA}
	}}
	r := newGatedRuntime()
	paused := newCapacityService(t, f, r).WithVerifier(verifier).Paused()
	var waiting *env.WaitingError
	if err := paused.Build(t.Context(), a); !errors.As(err, &waiting) || waiting.Reason != env.WaitPaused {
		t.Fatalf("a paused build: %v", err)
	}
	if verifier.calls != 0 {
		t.Fatal("a paused build read GitHub")
	}
	select {
	case <-r.started:
		t.Fatal("a paused build reached the runtime")
	default:
	}
	if state, reason := f.intentState(t, a.PullRequestID); state == "running" || reason != env.WaitPaused {
		t.Fatalf("paused request is %s/%q", state, reason)
	}
	// Resuming is a worker start without the switch: the same request builds.
	resumed := newCapacityService(t, f, r).WithVerifier(verifier)
	if err := resumed.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if got := <-r.started; got != a.PullRequestID {
		t.Fatal("the resumed request did not build")
	}
	if state, reason := f.intentState(t, a.PullRequestID); state != "running" || reason != "" {
		t.Fatalf("resumed request is %s/%q", state, reason)
	}
}
