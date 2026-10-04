package environments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
)

// The runtime boundary must see a committed empty log and the admitted attempt's
// open stream before writing sequence 0.
type logCheckingRuntime struct {
	*fakeRuntime
	f     *fixture
	calls int
}

func (r *logCheckingRuntime) Build(ctx context.Context, request env.Request, attempt env.Attempt) (env.Artifact, error) {
	r.calls++
	var count int
	if err := r.f.pool.QueryRow(ctx, "SELECT count(*) FROM build_logs").Scan(&count); err != nil {
		return env.Artifact{}, err
	}
	if count != 0 {
		return env.Artifact{}, errors.New("previous logs reached the runtime")
	}
	var epoch, generation, revision int64
	var state, sha string
	if err := r.f.pool.QueryRow(ctx, "SELECT epoch,generation,revision,state,head_sha FROM build_log_streams WHERE environment_id=$1", attempt.EnvironmentID).Scan(&epoch, &generation, &revision, &state, &sha); err != nil {
		return env.Artifact{}, err
	}
	if epoch != attempt.LogEpoch || generation != request.Generation || revision != request.Revision || sha != request.HeadSHA || state != "open" {
		return env.Artifact{}, fmt.Errorf("stream epoch=%d g=%d r=%d %s %s does not match attempt %+v", epoch, generation, revision, sha, state, attempt)
	}
	if _, err := r.f.pool.Exec(ctx, "INSERT INTO build_logs(environment_id,epoch,sequence,line) VALUES($1,$2,0,'current build')", attempt.EnvironmentID, attempt.LogEpoch); err != nil {
		return env.Artifact{}, err
	}
	return r.fakeRuntime.Build(ctx, request, attempt)
}

// recordingLogs is the BuildLogs boundary; it records which attempts finished.
type recordingLogs struct {
	mu       sync.Mutex
	finished []env.Attempt
}

func (l *recordingLogs) Finish(_ context.Context, environmentID, epoch int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finished = append(l.finished, env.Attempt{EnvironmentID: environmentID, LogEpoch: epoch})
	return nil
}

func seedBuildLog(t *testing.T, f *fixture, r env.Request) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `WITH s AS (
		INSERT INTO build_log_streams(environment_id,epoch,generation,revision,head_sha)
		SELECT id,1,0,1,'previous-sha' FROM environments WHERE pull_request_id=$1 RETURNING environment_id)
		INSERT INTO build_logs(environment_id,epoch,sequence,line) SELECT environment_id,1,0,'previous build' FROM s`, r.PullRequestID); err != nil {
		t.Fatal(err)
	}
}

func streamEpoch(t *testing.T, f *fixture) int64 {
	t.Helper()
	var epoch int64
	if err := f.pool.QueryRow(t.Context(), "SELECT epoch FROM build_log_streams").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestBuildClearsLogsBeforeRuntimeAndReusesEnvironment(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	seedBuildLog(t, f, a)
	var id int64
	var subdomain string
	if err := f.pool.QueryRow(t.Context(), "SELECT id,subdomain FROM environments").Scan(&id, &subdomain); err != nil {
		t.Fatal(err)
	}
	runtime := &logCheckingRuntime{fakeRuntime: newRuntime(), f: f}
	logs := &recordingLogs{}
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.WithBuildLogs(logs)
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	drainOperations(t, f, &fakePublisher{runtime: runtime.fakeRuntime})
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 2 {
		t.Fatalf("runtime calls: %d", runtime.calls)
	}
	drainOperations(t, f, &fakePublisher{runtime: runtime.fakeRuntime})
	closed := f.event(t, "closed", "sha-B")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	drainOperations(t, f, &fakePublisher{runtime: runtime.fakeRuntime})
	reopened := f.event(t, "reopened", "sha-C")
	if err := s.Build(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	drainOperations(t, f, &fakePublisher{runtime: runtime.fakeRuntime})
	var rows int
	var currentID int64
	var currentSubdomain, line string
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*),min(id),min(subdomain) FROM environments").Scan(&rows, &currentID, &currentSubdomain); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || currentID != id || currentSubdomain != subdomain || runtime.calls != 3 {
		t.Fatal("reopen changed the lifetime environment identity")
	}
	// The seeded stream was epoch 1; each of the three admissions opened a new one.
	if epoch := streamEpoch(t, f); epoch != 4 {
		t.Fatalf("stream epoch = %d, want 4", epoch)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT line FROM build_logs WHERE environment_id=$1 AND epoch=4 AND sequence=0", id).Scan(&line); err != nil || line != "current build" {
		t.Fatalf("current log: %q %v", line, err)
	}
	want := []env.Attempt{{EnvironmentID: id, LogEpoch: 2}, {EnvironmentID: id, LogEpoch: 3}, {EnvironmentID: id, LogEpoch: 4}}
	if fmt.Sprint(logs.finished) != fmt.Sprint(want) || fmt.Sprint(runtime.attempts) != fmt.Sprint(want) {
		t.Fatalf("attempts: runtime %v finished %v, want %v", runtime.attempts, logs.finished, want)
	}
}

func TestRejectedBuildPreservesLogs(t *testing.T) {
	for _, change := range []string{"generation", "revision", "manual", "revoked", "early"} {
		t.Run(change, func(t *testing.T) {
			f := newScheduleFixture(t)
			if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
				t.Fatal(err)
			}
			r := f.observe(t, "opened", "sha-A")
			f.provision(t, r)
			seedBuildLog(t, f.fixture, r)
			if change != "early" {
				f.clock.advance(env.QuietPeriod)
			}
			switch change {
			case "generation":
				f.observe(t, "synchronize", "sha-B")
			case "revision":
				if _, err := f.broker.RequestBuild(t.Context(), f.command(r, "manual-override-log-test", r.Revision, env.BuildLatest)); err != nil {
					t.Fatal(err)
				}
			case "manual":
				if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false"); err != nil {
					t.Fatal(err)
				}
			}
			runtime := newRuntime()
			s, err := env.NewService(f.repo, runtime, f.logger, time.Minute, f.clock.now)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Build(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			var line string
			if err := f.pool.QueryRow(t.Context(), "SELECT line FROM build_logs").Scan(&line); err != nil || line != "previous build" || len(runtime.started) != 0 {
				t.Fatalf("rejected build touched logs/runtime: %q %v", line, err)
			}
			if epoch := streamEpoch(t, f.fixture); epoch != 1 {
				t.Fatalf("rejected build opened a new log stream: epoch %d", epoch)
			}
		})
	}
}

func TestBuildLogClearFailureRollsBackAdmission(t *testing.T) {
	f := newFixture(t)
	r := f.event(t, "opened", "sha-A")
	f.provision(t, r)
	seedBuildLog(t, f, r)
	if _, err := f.pool.Exec(t.Context(), `CREATE FUNCTION reject_log_clear() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture failure'; END $$;
		CREATE TRIGGER reject_log_clear BEFORE DELETE ON build_logs FOR EACH ROW EXECUTE FUNCTION reject_log_clear();`); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime()
	s := service(t, f, runtime, &fakePublisher{})
	if err := s.Build(t.Context(), r); err == nil {
		t.Fatal("log clear failure ignored")
	}
	var state string
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT state,(SELECT count(*) FROM build_logs) FROM build_intents").Scan(&state, &count); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || count != 1 || len(runtime.started) != 0 {
		t.Fatalf("partial admission: state=%s logs=%d runtime=%d", state, count, len(runtime.started))
	}
	if epoch := streamEpoch(t, f); epoch != 1 {
		t.Fatalf("rolled-back admission kept a new log stream: epoch %d", epoch)
	}
}

// A River retry re-admits the same revision. It must get a new epoch so output
// from the earlier execution can never land in the retry's logs.
func TestRetriedRevisionOpensNewLogEpoch(t *testing.T) {
	f := newFixture(t)
	r := f.event(t, "opened", "sha-A")
	f.provision(t, r)
	runtime := newRuntime()
	runtime.fail[r.Generation] = true
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Build(t.Context(), r); err == nil {
		t.Fatal("simulated failure was not reported")
	}
	runtime.mu.Lock()
	runtime.fail[r.Generation] = false
	runtime.mu.Unlock()
	if err := s.Build(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if len(runtime.attempts) != 2 || runtime.attempts[0].EnvironmentID == 0 ||
		runtime.attempts[0].EnvironmentID != runtime.attempts[1].EnvironmentID ||
		runtime.attempts[1].LogEpoch <= runtime.attempts[0].LogEpoch {
		t.Fatalf("retry attempts: %v", runtime.attempts)
	}
}

func TestAttemptWithoutEnvironmentHasNoLogStream(t *testing.T) {
	f := newFixture(t)
	r := f.event(t, "opened", "sha-A")
	runtime := newRuntime()
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Build(t.Context(), r) // Without an environment row the result is not applied.
	if len(runtime.attempts) != 1 || runtime.attempts[0] != (env.Attempt{}) {
		t.Fatalf("attempts: %v", runtime.attempts)
	}
	var streams int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM build_log_streams").Scan(&streams); err != nil || streams != 0 {
		t.Fatalf("streams=%d err=%v", streams, err)
	}
}
