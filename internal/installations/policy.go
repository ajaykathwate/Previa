package installations

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/installations/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type BuildMode string

const (
	ManualBuilds    BuildMode = "manual"
	AutomaticBuilds BuildMode = "automatic"
)

var ErrPolicyUnavailable = errors.New("repository build policy is unavailable or access is denied")
var ErrInvalidBuildMode = errors.New("build mode must be manual or automatic")

type BuildPolicy struct {
	RepositoryID       int64
	Mode               BuildMode
	CancelledAutomatic int64
}

type buildPolicyStore interface {
	BuildPolicy(context.Context, int64, int64) (BuildPolicy, error)
	SetBuildMode(context.Context, int64, int64, BuildMode) (BuildPolicy, error)
}

// BuildPolicyService is independent of GitHub redirect/authentication dependencies.
type BuildPolicyService struct{ store buildPolicyStore }

func NewBuildPolicyService(store buildPolicyStore) (*BuildPolicyService, error) {
	if store == nil {
		return nil, errors.New("build policy requires a repository")
	}
	return &BuildPolicyService{store: store}, nil
}

func (s *BuildPolicyService) Get(ctx context.Context, userID, repoID int64) (BuildPolicy, error) {
	if userID <= 0 || repoID <= 0 {
		return BuildPolicy{}, ErrPolicyUnavailable
	}
	return s.store.BuildPolicy(ctx, userID, repoID)
}

func (s *BuildPolicyService) Set(ctx context.Context, userID, repoID int64, mode BuildMode) (BuildPolicy, error) {
	if userID <= 0 || repoID <= 0 {
		return BuildPolicy{}, ErrPolicyUnavailable
	}
	if mode != ManualBuilds && mode != AutomaticBuilds {
		return BuildPolicy{}, ErrInvalidBuildMode
	}
	return s.store.SetBuildMode(ctx, userID, repoID, mode)
}

func (r *Repository) BuildPolicy(ctx context.Context, userID, repoID int64) (BuildPolicy, error) {
	row, err := dbgen.New(r.db).ReadOwnedBuildPolicy(ctx, dbgen.ReadOwnedBuildPolicyParams{RepoID: repoID, UserID: pgtype.Int8{Int64: userID, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildPolicy{}, ErrPolicyUnavailable
	}
	if err != nil {
		return BuildPolicy{}, fmt.Errorf("read owned build policy: %w", err)
	}
	return BuildPolicy{RepositoryID: row.ID, Mode: BuildMode(row.BuildMode)}, nil
}

// lockPolicyScope locks the repository's installation, which the user must
// own, then the repository itself.
func lockPolicyScope(ctx context.Context, q *dbgen.Queries, userID, repoID int64) error {
	installationID, err := q.FindPolicyInstallation(ctx, repoID)
	if err != nil {
		return policyAccessError("find policy installation", err)
	}
	if _, err := q.LockPolicyInstallation(ctx, dbgen.LockPolicyInstallationParams{InstallationID: installationID, UserID: pgtype.Int8{Int64: userID, Valid: true}}); err != nil {
		return policyAccessError("lock policy installation", err)
	}
	if _, err := q.LockPolicyRepository(ctx, dbgen.LockPolicyRepositoryParams{RepoID: repoID, InstallationID: installationID, UserID: userID}); err != nil {
		return policyAccessError("lock policy repository", err)
	}
	return nil
}

// cancelAutomaticTx cancels the repository's Automatic requests that have not
// started, reporting each affected PR, and returns how many it cancelled.
func (r *Repository) cancelAutomaticTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, repoID int64) (int64, error) {
	cancelled, err := q.CancelUnstartedAutomaticIntents(ctx, repoID)
	if err != nil {
		return 0, fmt.Errorf("cancel automatic intent: %w", err)
	}
	if r.reports != nil {
		for _, pullRequestID := range cancelled {
			if err := r.reports.RecordReportTx(ctx, tx, pullRequestID); err != nil {
				return 0, fmt.Errorf("report cancelled request: %w", err)
			}
		}
	}
	return int64(len(cancelled)), nil
}

// policyAccessError reports a missing or foreign row as ErrPolicyUnavailable.
func policyAccessError(step string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPolicyUnavailable
	}
	return fmt.Errorf("%s: %w", step, err)
}

// SetBuildMode locks installation → repository → PRs → intents, matching the
// future coordinator. Mode change and cancellation commit together. Running work
// and deployed environments are untouched. Relinking upserts preserve this field.
func (r *Repository) SetBuildMode(ctx context.Context, userID, repoID int64, mode BuildMode) (result BuildPolicy, err error) {
	if userID <= 0 || repoID <= 0 {
		return result, ErrPolicyUnavailable
	}
	if mode != ManualBuilds && mode != AutomaticBuilds {
		return result, ErrInvalidBuildMode
	}
	err = r.transaction(ctx, func(tx pgx.Tx, q *dbgen.Queries) error {
		if err := lockPolicyScope(ctx, q, userID, repoID); err != nil {
			return err
		}
		if err := q.SetRepositoryBuildMode(ctx, dbgen.SetRepositoryBuildModeParams{ID: repoID, BuildMode: string(mode)}); err != nil {
			return fmt.Errorf("set build mode: %w", err)
		}
		result = BuildPolicy{RepositoryID: repoID, Mode: mode}
		if mode != ManualBuilds {
			return nil
		}
		cancelled, err := r.cancelAutomaticTx(ctx, tx, q, repoID)
		result.CancelledAutomatic = cancelled
		return err
	})
	if err != nil {
		return BuildPolicy{}, err
	}
	return result, nil
}
