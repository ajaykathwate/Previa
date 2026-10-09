package environments_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type intentFixture struct {
	pool                 *pgxpool.Pool
	repo                 *env.IntentRepository
	service              *env.IntentService
	policy               *installations.BuildPolicyService
	owner, other, repoID int64
	prID                 int64
}

func newIntentFixture(t *testing.T) *intentFixture {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for build intent persistence checks")
	}
	admin, err := pgxpool.New(t.Context(), connection)
	if err != nil {
		t.Fatal("initialize test database")
	}
	schema := fmt.Sprintf("intent_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal("invalid test connection")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_init.sql", "0003_pr_generation.sql", "0004_durable_operations.sql", "0005_installation_linking.sql"} {
		applyIntentMigration(t, pool, name)
	}
	f := &intentFixture{pool: pool}
	ctx := t.Context()
	if err := pool.QueryRow(ctx, "INSERT INTO users(github_id,username) VALUES(100,'owner') RETURNING id").Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO users(github_id,username) VALUES(101,'other') RETURNING id").Scan(&f.other); err != nil {
		t.Fatal(err)
	}
	var installationID int64
	if err := pool.QueryRow(ctx, "INSERT INTO installations(github_installation_id,owner_user_id) VALUES(101,$1) RETURNING id", f.owner).Scan(&installationID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO repos(installation_id,full_name,github_repo_id) VALUES($1,'owner/repo',501) RETURNING id", installationID).Scan(&f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO pull_requests(repo_id,number,head_sha,generation) VALUES($1,1,$2,1) RETURNING id", f.repoID, strings.Repeat("a", 40)).Scan(&f.prID); err != nil {
		t.Fatal(err)
	}
	// Existing repositories must acquire the Manual default, without data loss.
	applyIntentMigration(t, pool, "0006_build_intents.sql")
	applyIntentMigration(t, pool, "0007_intent_dispatch.sql")
	applyIntentMigration(t, pool, "0008_pr_workspace.sql")
	applyIntentMigration(t, pool, "0010_pr_mergeability.sql")
	applyIntentMigration(t, pool, "0016_housekeeping.sql") // build requests that wait for capacity
	if _, err := pool.Exec(ctx, "UPDATE pull_requests SET mergeable=true, mergeable_state='clean', build_status='open'"); err != nil {
		t.Fatal(err)
	}
	f.repo = env.NewIntentRepository(pool)
	f.service, err = env.NewIntentService(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.policy, err = installations.NewBuildPolicyService(installations.NewRepository(pool))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func applyIntentMigration(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	data, err := os.ReadFile("../../db/migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(data), "-- +goose Down")
	if _, err := pool.Exec(t.Context(), up); err != nil {
		t.Fatal(err)
	}
}

func (f *intentFixture) command(key string, revision int64, action env.IntentAction) env.IntentCommand {
	return env.IntentCommand{UserID: f.owner, PullRequestID: f.prID, ExpectedRevision: revision, IdempotencyKey: key, Action: action}
}

func (f *intentFixture) request(t *testing.T, key string, revision int64, action env.IntentAction) env.IntentOutcome {
	t.Helper()
	result, err := f.service.Request(t.Context(), f.command(key, revision, action))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *intentFixture) advance(t *testing.T, intent env.BuildIntent, state env.IntentState) env.BuildIntent {
	t.Helper()
	result, err := f.service.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: intent.PullRequestID, Revision: intent.Revision, Generation: intent.Generation, Next: state})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *intentFixture) observe(t *testing.T, deadline time.Time) env.ObservationOutcome {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	result, err := f.service.ObserveTx(t.Context(), tx, f.owner, f.prID, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIntentDefaultsOwnershipAndConstraints(t *testing.T) {
	f := newIntentFixture(t)
	policy, err := f.policy.Get(t.Context(), f.owner, f.repoID)
	if err != nil || policy.Mode != installations.ManualBuilds {
		t.Fatalf("existing repository default: %v %v", policy, err)
	}
	view, err := f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil || view.Current != nil || view.Generation != 1 {
		t.Fatal("observed PR unexpectedly has an intent")
	}
	for _, mode := range []installations.BuildMode{"", "auto", "invalid"} {
		if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, mode); !errors.Is(err, installations.ErrInvalidBuildMode) {
			t.Fatal("accepted invalid build mode")
		}
	}
	cmd := f.command("first-request-key", 0, env.BuildLatest)
	cmd.UserID = f.other
	if _, err := f.service.Request(t.Context(), cmd); !errors.Is(err, env.ErrIntentUnavailable) {
		t.Fatal("foreign request authorized")
	}
	if _, err := f.service.Get(t.Context(), f.other, f.prID); !errors.Is(err, env.ErrIntentUnavailable) {
		t.Fatal("foreign intent visible")
	}
	if _, err := f.policy.Set(t.Context(), f.other, f.repoID, installations.AutomaticBuilds); !errors.Is(err, installations.ErrPolicyUnavailable) {
		t.Fatal("foreign mode change authorized")
	}
	f.request(t, "first-request-key", 0, env.BuildLatest)
	for _, statement := range []string{
		"UPDATE repos SET build_mode='invalid'",
		"UPDATE build_intents SET revision=0",
		"UPDATE build_intents SET generation=-1",
		"UPDATE build_intents SET source='automatic'",
		"UPDATE build_intents SET state='failed'",
	} {
		if _, err := f.pool.Exec(t.Context(), statement); err == nil {
			t.Fatal("schema accepted invalid mode/intent")
		}
	}
	var environments, operations int
	if err := f.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM environments),(SELECT count(*) FROM environment_operations)").Scan(&environments, &operations); err != nil || environments != 0 || operations != 0 {
		t.Fatal("request created deployment/publication state")
	}
}

func TestIntentManualIdempotencyExplicitRetryAndRebuild(t *testing.T) {
	f := newIntentFixture(t)
	first := f.request(t, "first-request-key", 0, env.BuildLatest)
	if first.Coalesced || first.Intent.Revision != 1 || first.Intent.Generation != 1 || first.Intent.Source != "manual" {
		t.Fatal("incorrect initial request")
	}
	replay := f.request(t, "first-request-key", 0, env.BuildLatest)
	if !replay.Coalesced || replay.Intent.Revision != 1 {
		t.Fatal("HTTP replay created another intent")
	}
	coalesced := f.request(t, "second-click-key", 1, env.BuildLatest)
	if !coalesced.Coalesced || coalesced.Intent.Revision != 1 {
		t.Fatal("duplicate click did not coalesce")
	}
	if _, err := f.service.Request(t.Context(), f.command("first-request-key", 0, env.Rebuild)); !errors.Is(err, env.ErrIntentKeyConflict) {
		t.Fatal("same key changed action")
	}
	if _, err := f.service.Request(t.Context(), f.command("too-early-retry-key", 1, env.RetryBuild)); !errors.Is(err, env.ErrIntentAction) {
		t.Fatal("retry while pending accepted")
	}
	queued := f.advance(t, first.Intent, env.IntentQueued)
	running := f.advance(t, queued, env.IntentRunning)
	f.advance(t, running, env.IntentFailed)
	if !f.request(t, "failed-build-key", 1, env.BuildLatest).Coalesced {
		t.Fatal("Build implicitly retried failure")
	}
	retry := f.request(t, "explicit-retry-key", 1, env.RetryBuild)
	if retry.Intent.Revision != 2 || retry.Intent.Generation != first.Intent.Generation {
		t.Fatal("retry did not use separate request revision")
	}
	if !f.request(t, "explicit-retry-key", 1, env.RetryBuild).Coalesced {
		t.Fatal("retry replay not idempotent")
	}
	if _, err := f.service.Request(t.Context(), f.command("first-request-key", 0, env.BuildLatest)); !errors.Is(err, env.ErrIntentConflict) {
		t.Fatal("older HTTP replay created new work")
	}
	running = f.advance(t, f.advance(t, retry.Intent, env.IntentQueued), env.IntentRunning)
	f.advance(t, running, env.IntentSucceeded)
	if !f.request(t, "successful-build-key", 2, env.BuildLatest).Coalesced {
		t.Fatal("Build implicitly rebuilt success")
	}
	rebuild := f.request(t, "explicit-rebuild-key", 2, env.Rebuild)
	if rebuild.Intent.Revision != 3 || rebuild.Intent.Generation != 1 {
		t.Fatal("rebuild changed observed generation")
	}
	_, err := f.service.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: f.prID, Revision: 2, Generation: 1, Next: env.IntentFailed})
	if !errors.Is(err, env.ErrIntentConflict) {
		t.Fatal("late same-generation result overwrote newer revision")
	}
	var rows int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM build_intents").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("historical build rows accumulated")
	}
}

func TestIntentObservationManualAutomaticAndOverride(t *testing.T) {
	f := newIntentFixture(t)
	if result := f.observe(t, time.Time{}); result.Intent != nil || result.TeardownRequired {
		t.Fatal("Manual observation authorized work")
	}
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil || view.Current != nil {
		t.Fatal("enabling Automatic bulk-created an intent")
	}
	deadline := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Microsecond)
	automatic := f.observe(t, deadline).Intent
	if automatic == nil || automatic.Source != "automatic" || automatic.RequestedBy != 0 || !automatic.AvailableAt.Equal(deadline) {
		t.Fatal("automatic deadline/source missing")
	}
	replayed := f.observe(t, deadline.Add(time.Minute)).Intent
	if replayed.Revision != automatic.Revision || !replayed.AvailableAt.Equal(deadline) {
		t.Fatal("duplicate observation reset deadline")
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET generation=generation+1,head_sha=$1 WHERE id=$2", strings.Repeat("b", 40), f.prID); err != nil {
		t.Fatal(err)
	}
	latest := f.observe(t, deadline.Add(time.Minute)).Intent
	if latest.Revision != 2 || latest.Generation != 2 || !latest.AvailableAt.Equal(deadline.Add(time.Minute)) {
		t.Fatal("new observed generation not captured")
	}
	queued := f.advance(t, *latest, env.IntentQueued)
	if _, err := f.service.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: f.prID, Revision: queued.Revision, Generation: queued.Generation, Next: env.IntentRunning}); !errors.Is(err, env.ErrIntentNotDue) {
		t.Fatal("future deadline started early")
	}
	manual := f.request(t, "manual-override-key", 2, env.BuildLatest)
	if manual.Intent.Revision != 3 || manual.Intent.Source != "manual" || manual.Intent.AvailableAt.After(time.Now()) {
		t.Fatal("manual override not immediate/new revision")
	}
}

func TestIntentModeChangeCancelsQueuedButPreservesRunningAndManual(t *testing.T) {
	for _, state := range []env.IntentState{env.IntentPending, env.IntentQueued, env.IntentRunning} {
		t.Run(string(state), func(t *testing.T) {
			f := newIntentFixture(t)
			if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
				t.Fatal(err)
			}
			current := *f.observe(t, time.Now().Add(-time.Second)).Intent
			if state != env.IntentPending {
				current = f.advance(t, current, env.IntentQueued)
			}
			if state == env.IntentRunning {
				current = f.advance(t, current, env.IntentRunning)
			}
			policy, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds)
			if err != nil {
				t.Fatal(err)
			}
			view, err := f.service.Get(t.Context(), f.owner, f.prID)
			if err != nil {
				t.Fatal(err)
			}
			if state == env.IntentRunning {
				if policy.CancelledAutomatic != 0 || view.Current.State != env.IntentRunning {
					t.Fatal("mode switch cancelled running work")
				}
				f.advance(t, current, env.IntentSucceeded)
			} else if policy.CancelledAutomatic != 1 || view.Current.State != env.IntentCancelled || view.Current.CompletedAt.IsZero() {
				t.Fatal("unstarted automatic work not cancelled")
			}
		})
	}
	f := newIntentFixture(t)
	manual := f.request(t, "manual-request-key", 0, env.BuildLatest)
	policy, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds)
	if err != nil || policy.CancelledAutomatic != 0 {
		t.Fatal("manual work cancelled by mode change")
	}
	f.advance(t, manual.Intent, env.IntentQueued)
}

func TestIntentCloseAndNewCommitFenceRunningWork(t *testing.T) {
	for _, state := range []string{"open", "closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			f := newIntentFixture(t)
			first := f.request(t, "first-request-key", 0, env.BuildLatest)
			running := f.advance(t, f.advance(t, first.Intent, env.IntentQueued), env.IntentRunning)
			if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET generation=generation+1,head_sha=$1,state=$2 WHERE id=$3", strings.Repeat("b", 40), state, f.prID); err != nil {
				t.Fatal(err)
			}
			observed := f.observe(t, time.Time{})
			if observed.TeardownRequired != (state != "open") {
				t.Fatal("close teardown authorization missing")
			}
			_, err := f.service.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: f.prID, Revision: running.Revision, Generation: running.Generation, Next: env.IntentSucceeded})
			if !errors.Is(err, env.ErrIntentConflict) {
				t.Fatal("obsolete running result accepted")
			}
			view, err := f.service.Get(t.Context(), f.owner, f.prID)
			if err != nil || view.Current.State != env.IntentSuperseded || view.Current.Revision != 1 {
				t.Fatal("observation implicitly authorized successor")
			}
			if state == "open" {
				newer := f.request(t, "new-generation-key", 1, env.BuildLatest)
				if newer.Intent.Generation != 2 || newer.Intent.Revision != 2 {
					t.Fatal("manual did not capture newest state")
				}
			} else if _, err := f.service.Request(t.Context(), f.command("closed-request-key", 1, env.BuildLatest)); !errors.Is(err, env.ErrPRClosed) {
				t.Fatal("closed/merged PR accepted build")
			}
		})
	}
}

func TestIntentAccessRevocationAndPolicyPreservationOnRelink(t *testing.T) {
	f := newIntentFixture(t)
	repository := installations.NewRepository(f.pool)
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	info := []installations.RepositoryInfo{{ID: 501, FullName: "owner/renamed", Accessible: true}}
	revision, err := repository.Prepare(t.Context(), 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Link(t.Context(), 101, f.owner, revision, "owner", info); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"suspended", "inactive"} {
		if err := repository.ApplyEvent(t.Context(), 101, status, "owner", nil, nil, false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.policy.Get(t.Context(), f.owner, f.repoID); !errors.Is(err, installations.ErrPolicyUnavailable) {
			t.Fatal("unavailable policy accessible")
		}
		if _, err := f.service.Request(t.Context(), f.command("revoked-request-key", 0, env.BuildLatest)); !errors.Is(err, env.ErrIntentUnavailable) {
			t.Fatal("revoked installation authorized request")
		}
	}
	// New verified installation for the same owner retains stable repo identity/mode.
	if err := repository.Link(t.Context(), 102, f.owner, 0, "owner", info); err != nil {
		t.Fatal(err)
	}
	policy, err := f.policy.Get(t.Context(), f.owner, f.repoID)
	if err != nil || policy.Mode != installations.AutomaticBuilds {
		t.Fatal("relink/rename lost policy")
	}
	if err := repository.ApplyEvent(t.Context(), 102, "", "owner", nil, info, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds); !errors.Is(err, installations.ErrPolicyUnavailable) {
		t.Fatal("removed repository access accepted policy change")
	}
}

func TestIntentConcurrentRequestsAndSameSHARetries(t *testing.T) {
	f := newIntentFixture(t)
	var wg sync.WaitGroup
	errorsOut := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			result, err := f.service.Request(t.Context(), f.command("concurrent-request-key", 0, env.BuildLatest))
			if err == nil && result.Intent.Revision != 1 {
				err = errors.New("concurrent duplicate created a revision")
			}
			errorsOut <- err
		})
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	view, err := f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil {
		t.Fatal(err)
	}
	running := f.advance(t, f.advance(t, *view.Current, env.IntentQueued), env.IntentRunning)
	f.advance(t, running, env.IntentFailed)
	results := make(chan error, 2)
	for _, key := range []string{"parallel-retry-key-a", "parallel-retry-key-b"} {
		wg.Go(func() {
			_, err := f.service.Request(t.Context(), f.command(key, 1, env.RetryBuild))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, env.ErrIntentConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("concurrent distinct retries were not CAS-serialized")
	}
}

func TestIntentAtomicObservationAndModeRollback(t *testing.T) {
	f := newIntentFixture(t)
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A failure below must not leave the transaction holding locks that
	// block the schema's cleanup.
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := f.service.ObserveTx(t.Context(), tx, f.owner, f.prID, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil || view.Current != nil {
		t.Fatal("rolled-back intent persisted")
	}
	f.observe(t, time.Now().Add(time.Minute))
	if _, err := f.pool.Exec(t.Context(), `CREATE FUNCTION reject_intent_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture failure'; END $$;
CREATE TRIGGER reject_intent_change BEFORE UPDATE ON build_intents FOR EACH ROW EXECUTE FUNCTION reject_intent_change();`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds); err == nil {
		t.Fatal("mode cancellation failure ignored")
	}
	policy, err := f.policy.Get(t.Context(), f.owner, f.repoID)
	if err != nil || policy.Mode != installations.AutomaticBuilds {
		t.Fatal("mode committed without intent cancellation")
	}
	view, err = f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil || view.Current.State != env.IntentPending {
		t.Fatal("failed cancellation partially committed")
	}
}

func TestIntentCommandValidation(t *testing.T) {
	if _, err := env.NewIntentService(nil); err == nil {
		t.Fatal("accepted nil intent repository")
	}
	if _, err := installations.NewBuildPolicyService(nil); err == nil {
		t.Fatal("accepted nil policy repository")
	}
	service, err := env.NewIntentService(env.NewIntentRepository(nil))
	if err != nil {
		t.Fatal(err)
	}
	valid := env.IntentCommand{UserID: 1, PullRequestID: 1, ExpectedRevision: 0, IdempotencyKey: "valid-request-key", Action: env.BuildLatest}
	for _, change := range []func(*env.IntentCommand){
		func(c *env.IntentCommand) { c.UserID = 0 },
		func(c *env.IntentCommand) { c.PullRequestID = 0 },
		func(c *env.IntentCommand) { c.ExpectedRevision = -1 },
		func(c *env.IntentCommand) { c.IdempotencyKey = "short" },
		func(c *env.IntentCommand) { c.IdempotencyKey = strings.Repeat("a", 129) },
		func(c *env.IntentCommand) { c.Action = "invalid" },
	} {
		command := valid
		change(&command)
		if _, err := service.Request(t.Context(), command); err == nil {
			t.Fatal("invalid command reached persistence")
		}
	}
	if _, err := service.Get(t.Context(), 0, 1); err == nil {
		t.Fatal("invalid owner read accepted")
	}
	if _, err := service.Advance(t.Context(), env.IntentTransition{}); err == nil {
		t.Fatal("invalid transition accepted")
	}
	if _, err := service.ObserveTx(t.Context(), nil, 1, 1, time.Time{}); err == nil {
		t.Fatal("observation without transaction accepted")
	}
}

func TestIntentConcurrentModeChangeAndAuthorization(t *testing.T) {
	for range 4 {
		f := newIntentFixture(t)
		if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			_, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds)
			results <- err
		})
		wg.Go(func() {
			<-start
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				results <- err
				return
			}
			_, err = f.service.ObserveTx(t.Context(), tx, f.owner, f.prID, time.Now().Add(time.Minute))
			if err == nil {
				err = tx.Commit(t.Context())
			} else if rollbackErr := tx.Rollback(t.Context()); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
			results <- err
		})
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		view, err := f.service.Get(t.Context(), f.owner, f.prID)
		if err != nil || view.BuildMode != "manual" || (view.Current != nil && view.Current.State != env.IntentCancelled) {
			t.Fatal("mode/observation race left live automatic intent")
		}
	}
}

func TestIntentCurrentRequestTransitionRetryAndGenerationZero(t *testing.T) {
	f := newIntentFixture(t)
	if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET generation=0 WHERE id=$1", f.prID); err != nil {
		t.Fatal(err)
	}
	first := f.request(t, "zero-generation-key", 0, env.BuildLatest)
	if first.Intent.Generation != 0 {
		t.Fatal("manual request fabricated an observed event")
	}
	queued := f.advance(t, first.Intent, env.IntentQueued)
	if f.advance(t, queued, env.IntentQueued).Revision != first.Intent.Revision {
		t.Fatal("transition replay changed revision")
	}
	if _, err := f.service.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: f.prID, Revision: 1, Generation: 0, Next: env.IntentSucceeded}); !errors.Is(err, env.ErrIntentAction) {
		t.Fatal("queued intent skipped execution")
	}
}

func TestIntentMigrationRollbackAndReapplyInIsolatedSchema(t *testing.T) {
	f := newIntentFixture(t)
	data, err := os.ReadFile("../../db/migrations/0006_build_intents.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("migration rollback absent")
	}
	if _, err := f.pool.Exec(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	applyIntentMigration(t, f.pool, "0006_build_intents.sql")
	view, err := f.service.Get(t.Context(), f.owner, f.prID)
	if err != nil || view.HeadSHA != strings.Repeat("a", 40) || view.BuildMode != "manual" || view.Current != nil {
		t.Fatal("rollback/reapply damaged existing repository/PR")
	}
}
