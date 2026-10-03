package environments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
)

// The runtime boundary must see a committed empty log before writing sequence 0.
type logCheckingRuntime struct {
	*fakeRuntime
	f     *fixture
	calls int
}

func (r *logCheckingRuntime) Build(ctx context.Context, request env.Request) (env.Artifact, error) {
	r.calls++
	var count int
	if err := r.f.pool.QueryRow(ctx, "SELECT count(*) FROM build_logs").Scan(&count); err != nil {
		return env.Artifact{}, err
	}
	if count != 0 {
		return env.Artifact{}, errors.New("previous logs reached the runtime")
	}
	if _, err := r.f.pool.Exec(ctx, `INSERT INTO build_logs(environment_id,sequence,line)
		SELECT id,0,'current build' FROM environments WHERE pull_request_id=$1`, request.PullRequestID); err != nil {
		return env.Artifact{}, err
	}
	return r.fakeRuntime.Build(ctx, request)
}

func seedBuildLog(t *testing.T, f *fixture, r env.Request) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO build_logs(environment_id,sequence,line)
		SELECT id,0,'previous build' FROM environments WHERE pull_request_id=$1`, r.PullRequestID); err != nil {
		t.Fatal(err)
	}
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
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := f.pool.QueryRow(t.Context(), "SELECT line FROM build_logs WHERE environment_id=$1 AND sequence=0", id).Scan(&line); err != nil || line != "current build" {
		t.Fatalf("current log: %q %v", line, err)
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
}
