package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
)

// TeardownReason says why a PR that GitHub may still show open is torn down.
type TeardownReason string

const (
	TeardownRevoked   TeardownReason = "revoked"   // installation suspended or deleted, or repository removed
	TeardownRequested TeardownReason = "requested" // the owner asked (dashboard action, Prompt 10)
	TeardownExpired   TeardownReason = "expired"   // housekeeping expiry (Prompt 9)

	TeardownSweepQueue = "environment_teardown"
)

func (reason TeardownReason) valid() bool {
	return reason == TeardownRevoked || reason == TeardownRequested || reason == TeardownExpired
}

// lockTeardownScope locks installation, repository and PR in the usual order
// without requiring the installation to be active or the repository
// accessible: local teardown must work after GitHub access is gone.
func lockTeardownScope(ctx context.Context, q *dbgen.Queries, pullRequestID int64) (dbgen.LockTeardownRequestRow, error) {
	scope, err := q.FindIntentScope(ctx, pullRequestID)
	if err != nil {
		return dbgen.LockTeardownRequestRow{}, fmt.Errorf("find teardown scope: %w", err)
	}
	if _, err := q.LockScopeInstallation(ctx, scope.InstallationID); err != nil {
		return dbgen.LockTeardownRequestRow{}, fmt.Errorf("lock teardown installation: %w", err)
	}
	if _, err := q.LockScopeRepository(ctx, scope.RepoID); err != nil {
		return dbgen.LockTeardownRequestRow{}, fmt.Errorf("lock teardown repository: %w", err)
	}
	pr, err := q.LockTeardownRequest(ctx, pullRequestID)
	if err != nil {
		return dbgen.LockTeardownRequestRow{}, fmt.Errorf("lock teardown PR: %w", err)
	}
	return pr, nil
}

// RequestTeardown records a local teardown of one PR and starts it in the same
// transaction: the generation advances (fencing every earlier build,
// publication and report), the current build request is superseded, and the
// environment's artifacts move to a durable route-removal operation. Nothing
// external happens here, and nothing will rebuild the preview until a new
// owner request or a later relevant Automatic event.
//
// It is idempotent: a teardown already requested and started at the current
// generation, or a PR with nothing to tear down, reports false. Authorization
// is the caller's: revocation needs none; an owner request checks ownership
// before calling this.
func (r *Repository) RequestTeardown(ctx context.Context, pullRequestID int64, reason TeardownReason) (bool, error) {
	if pullRequestID <= 0 || !reason.valid() {
		return false, errors.New("invalid teardown request")
	}
	if r.operations == nil {
		return false, errors.New("environment operation broker is not configured")
	}
	var started bool
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		started = false
		q := dbgen.New(tx)
		pr, err := lockTeardownScope(ctx, q, pullRequestID)
		if err != nil {
			return err
		}
		required := pr.State != "open" || pr.TeardownRequestedGeneration == pr.Generation
		if required && pr.TeardownCompletedGeneration >= pr.Generation {
			return nil // Requested and started already.
		}
		if !required {
			live, err := teardownNeeded(ctx, q, pullRequestID)
			if err != nil || !live {
				return err
			}
			if _, err := q.RequestTeardown(ctx, dbgen.RequestTeardownParams{ID: pullRequestID, Reason: pgtype.Text{String: string(reason), Valid: true}}); err != nil {
				return fmt.Errorf("record teardown request: %w", err)
			}
			if _, err := q.SupersedeIntents(ctx, []int64{pullRequestID}); err != nil {
				return fmt.Errorf("supersede build request: %w", err)
			}
		}
		started, err = r.startTeardownTx(ctx, tx, pullRequestID)
		return err
	})
	if err != nil {
		return false, err
	}
	return started, nil
}

// teardownNeeded reports whether an open PR holds a live preview or an
// unfinished build request that a teardown must stop.
func teardownNeeded(ctx context.Context, q *dbgen.Queries, pullRequestID int64) (bool, error) {
	env, err := q.ReadEnvironment(ctx, pullRequestID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read environment for teardown: %w", err)
	}
	if err == nil && env.Status != "tearing_down" && env.Status != "torn_down" {
		return true, nil
	}
	intent, err := q.ReadBuildIntent(ctx, pullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read build request for teardown: %w", err)
	}
	return intent.State == string(IntentPending) || intent.State == string(IntentQueued) || intent.State == string(IntentRunning), nil
}

// startTeardownTx starts the teardown required at the PR's current generation
// (closed, or locally requested), which the caller has locked: the environment
// becomes tearing_down and hands its artifacts to a route-removal operation,
// and the request is marked started. It reports whether a removal began.
func (r *Repository) startTeardownTx(ctx context.Context, tx pgx.Tx, pullRequestID int64) (bool, error) {
	q := dbgen.New(tx)
	pr, err := q.LockTeardownRequest(ctx, pullRequestID)
	if err != nil {
		return false, fmt.Errorf("lock PR to start teardown: %w", err)
	}
	if pr.State == "open" && pr.TeardownRequestedGeneration != pr.Generation {
		return false, errors.New("teardown is not required at the PR's current generation")
	}
	if pr.TeardownCompletedGeneration >= pr.Generation {
		return false, nil
	}
	request := NewRequest(pr.ID, pr.Generation, pr.HeadSha, pr.State)
	env, err := q.ReadEnvironment(ctx, pr.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read environment to start teardown: %w", err)
	}
	started := false
	inTeardown := err == nil && env.Status == "tearing_down" && env.TeardownGeneration.Valid && env.TeardownGeneration.Int64 == pr.Generation
	if err == nil && env.Status != "torn_down" && !inTeardown {
		previous, err := q.ApplyResult(ctx, dbgen.ApplyResultParams{
			PrID: pr.ID, ExpectedGeneration: pr.Generation, ExpectedRevision: 0, ResultStatus: "torn_down", HeadSha: pr.HeadSha,
		})
		if err != nil {
			return false, fmt.Errorf("start environment teardown: %w", err)
		}
		if err := r.enqueueResultTx(ctx, tx, request, Result{Status: "torn_down"}, previous); err != nil {
			return false, err
		}
		started = true
	}
	if _, err := q.CompleteTeardownRequest(ctx, dbgen.CompleteTeardownRequestParams{ID: pr.ID, Generation: pr.Generation}); err != nil {
		return false, fmt.Errorf("record started teardown: %w", err)
	}
	if err := r.reportTx(ctx, tx, pr.ID); err != nil {
		return false, err
	}
	return started, nil
}

// RevokeTx requests teardown of every affected PR inside the caller's
// installation lifecycle transaction, which already holds the installation's
// row lock. allRepositories covers suspension and deletion; otherwise
// githubRepoIDs lists the removed repositories. A sweep wake-up commits with
// the requests; the sweep starts each teardown without GitHub access.
func (b *Broker) RevokeTx(ctx context.Context, tx pgx.Tx, githubInstallationID int64, allRepositories bool, githubRepoIDs []int64) (int, error) {
	if githubInstallationID <= 0 || (!allRepositories && len(githubRepoIDs) == 0) {
		return 0, nil
	}
	q := dbgen.New(tx)
	ids, err := q.RequestRevocationTeardowns(ctx, dbgen.RequestRevocationTeardownsParams{
		GithubInstallationID: githubInstallationID, AllRepositories: allRepositories, GithubRepoIds: githubRepoIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("request revocation teardowns: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := q.SupersedeIntents(ctx, ids); err != nil {
		return 0, fmt.Errorf("supersede revoked build requests: %w", err)
	}
	if _, err := b.client.InsertTx(ctx, tx, TeardownSweepArgs{}, nil); err != nil {
		return 0, fmt.Errorf("schedule teardown sweep: %w", err)
	}
	return len(ids), nil
}

// TeardownSweepArgs wakes the teardown sweep. Wake-ups are not deduplicated:
// one inserted while a sweep runs must still cause a later sweep, and extra
// sweeps are idempotent and cheap.
type TeardownSweepArgs struct{}

func (TeardownSweepArgs) Kind() string { return "environment_teardown_sweep" }
func (TeardownSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: TeardownSweepQueue, MaxAttempts: 10}
}

// TeardownSweepWorker runs one sweep per wake-up. Registered with the other
// execution workers (Prompt 10).
type TeardownSweepWorker struct {
	river.WorkerDefaults[TeardownSweepArgs]
	Sweeper *TeardownSweeper
}

func (*TeardownSweepWorker) Timeout(*river.Job[TeardownSweepArgs]) time.Duration {
	return 2 * time.Minute
}
func (w *TeardownSweepWorker) Work(ctx context.Context, _ *river.Job[TeardownSweepArgs]) error {
	_, err := w.Sweeper.RunOnce(ctx)
	return err
}

// TeardownSweeper starts recorded teardowns and clears an installation's
// cleanup marker once nothing of its revoked pull requests remains: no
// unstarted teardown, no environment still live or tearing down, no
// unfinished route removal or cleanup, and no Docker resource left.
type TeardownSweeper struct {
	repo      *Repository
	inventory ResourceInventory
	logger    *slog.Logger
	batchSize int32
}

// TeardownSweep counts what one pass did.
type TeardownSweep struct {
	Started int // teardowns started
	Cleared int // installations whose cleanup marker was cleared
}

func NewTeardownSweeper(repo *Repository, inventory ResourceInventory, logger *slog.Logger, batchSize int32) (*TeardownSweeper, error) {
	if repo == nil || repo.operations == nil || inventory == nil || logger == nil {
		return nil, errors.New("teardown sweeper requires a repository with operations, an inventory and a logger")
	}
	if batchSize <= 0 {
		return nil, errors.New("teardown sweeper batch size must be positive")
	}
	return &TeardownSweeper{repo: repo, inventory: inventory, logger: logger, batchSize: batchSize}, nil
}

// RunOnce is bounded by the batch size and safe to repeat or run concurrently:
// every step rechecks under locks and is idempotent.
func (s *TeardownSweeper) RunOnce(ctx context.Context) (TeardownSweep, error) {
	var sweep TeardownSweep
	q := dbgen.New(s.repo.pool)
	ids, err := q.ListUnstartedTeardowns(ctx, dbgen.ListUnstartedTeardownsParams{AfterID: 0, BatchSize: s.batchSize})
	if err != nil {
		return sweep, fmt.Errorf("list unstarted teardowns: %w", err)
	}
	for _, id := range ids {
		var started bool
		err := database.InTransaction(ctx, s.repo.pool, func(tx pgx.Tx) error {
			if _, err := lockTeardownScope(ctx, dbgen.New(tx), id); err != nil {
				return err
			}
			var e error
			started, e = s.repo.startTeardownTx(ctx, tx, id)
			return e
		})
		if err != nil {
			return sweep, fmt.Errorf("start teardown for PR %d: %w", id, err)
		}
		if started {
			sweep.Started++
		}
	}
	installations, err := q.ListCleanupPendingInstallations(ctx, dbgen.ListCleanupPendingInstallationsParams{AfterID: 0, BatchSize: s.batchSize})
	if err != nil {
		return sweep, fmt.Errorf("list installations awaiting cleanup: %w", err)
	}
	for _, installationID := range installations {
		started, cleared, err := s.settle(ctx, installationID)
		if err != nil {
			return sweep, fmt.Errorf("settle installation %d cleanup: %w", installationID, err)
		}
		sweep.Started += started
		if cleared {
			sweep.Cleared++
		}
	}
	if sweep.Started > 0 || sweep.Cleared > 0 {
		s.logger.InfoContext(ctx, "teardown sweep completed", "teardowns_started", sweep.Started, "installations_cleared", sweep.Cleared)
	}
	return sweep, nil
}

// settle requests and starts teardown for anything revoked now (however the
// access was lost), then clears the cleanup marker when nothing remains.
func (s *TeardownSweeper) settle(ctx context.Context, installationID int64) (started int, cleared bool, err error) {
	var revision int64
	err = database.InTransaction(ctx, s.repo.pool, func(tx pgx.Tx) error {
		started = 0
		q := dbgen.New(tx)
		installation, err := q.LockCleanupInstallation(ctx, installationID)
		if err != nil {
			return fmt.Errorf("lock installation: %w", err)
		}
		revision = installation.Revision
		if !installation.CleanupPending {
			return nil
		}
		ids, err := q.RequestInaccessibleTeardowns(ctx, installationID)
		if err != nil {
			return fmt.Errorf("request teardown of revoked PRs: %w", err)
		}
		if len(ids) > 0 {
			if _, err := q.SupersedeIntents(ctx, ids); err != nil {
				return fmt.Errorf("supersede revoked build requests: %w", err)
			}
		}
		for _, id := range ids {
			ok, err := s.repo.startTeardownTx(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("start teardown for PR %d: %w", id, err)
			}
			if ok {
				started++
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	q := dbgen.New(s.repo.pool)
	work, err := q.CountInstallationTeardownWork(ctx, installationID)
	if err != nil {
		return started, false, fmt.Errorf("count remaining teardown work: %w", err)
	}
	if work.Unstarted+work.Live+work.Operations > 0 {
		return started, false, nil
	}
	// Docker is the last word: anything still labelled for a revoked PR is
	// scheduled for removal, and the marker stays until it is gone. Inventory
	// runs outside any transaction.
	prs, err := q.ListRevokedEnvironmentPRs(ctx, dbgen.ListRevokedEnvironmentPRsParams{InstallationID: installationID, BatchSize: s.batchSize})
	if err != nil {
		return started, false, fmt.Errorf("list revoked environments: %w", err)
	}
	if len(prs) == int(s.batchSize) {
		return started, false, nil // More than one pass can check; finish next time.
	}
	remaining := false
	for _, id := range prs {
		resources, err := s.inventory.Resources(ctx, id)
		if err != nil {
			return started, false, fmt.Errorf("inspect resources of PR %d: %w", id, err)
		}
		if len(resources) == 0 {
			continue
		}
		remaining = true
		if err := s.repo.ScheduleResourceCleanup(ctx, Request{PullRequestID: id}, resources); err != nil {
			return started, false, fmt.Errorf("schedule cleanup of PR %d: %w", id, err)
		}
	}
	if remaining {
		return started, false, nil
	}
	affected, err := q.ClearInstallationCleanup(ctx, dbgen.ClearInstallationCleanupParams{ID: installationID, Revision: revision})
	if err != nil {
		return started, false, fmt.Errorf("clear installation cleanup marker: %w", err)
	}
	return started, affected == 1, nil
}
