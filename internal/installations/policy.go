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
	err = r.transaction(ctx, func(_ pgx.Tx, q *dbgen.Queries) error {
		installationID, err := q.FindPolicyInstallation(ctx, repoID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPolicyUnavailable
		}
		if err != nil {
			return fmt.Errorf("find policy installation: %w", err)
		}
		if _, err := q.LockPolicyInstallation(ctx, dbgen.LockPolicyInstallationParams{InstallationID: installationID, UserID: pgtype.Int8{Int64: userID, Valid: true}}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPolicyUnavailable
			}
			return fmt.Errorf("lock policy installation: %w", err)
		}
		if _, err := q.LockPolicyRepository(ctx, dbgen.LockPolicyRepositoryParams{RepoID: repoID, InstallationID: installationID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPolicyUnavailable
			}
			return fmt.Errorf("lock policy repository: %w", err)
		}
		if err := q.SetRepositoryBuildMode(ctx, dbgen.SetRepositoryBuildModeParams{ID: repoID, BuildMode: string(mode)}); err != nil {
			return fmt.Errorf("set build mode: %w", err)
		}
		result = BuildPolicy{RepositoryID: repoID, Mode: mode}
		if mode == ManualBuilds {
			count, err := q.CancelUnstartedAutomaticIntents(ctx, repoID)
			if err != nil {
				return fmt.Errorf("cancel automatic intent: %w", err)
			}
			result.CancelledAutomatic = count
		}
		return nil
	})
	if err != nil {
		return BuildPolicy{}, err
	}
	return result, nil
}
