package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/runtime/dbgen"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Target is what may run for one admitted request.
type Target struct {
	PullRequestID, RepoID int64 // Previa IDs
	Number                int32
	InstallationID        int64 // GitHub installation ID
	// GitHub repository IDs. HeadRepoID is 0 when the head repository is
	// unknown or deleted.
	BaseRepoID, HeadRepoID int64
	Fork                   bool
}

// secretSource is the source identity secrets are decided on. Anything GitHub
// marked as a fork is untrusted even if repository IDs happened to match.
func (t Target) secretSource() secrets.Source {
	if t.Fork {
		return secrets.Source{BaseRepoID: t.BaseRepoID}
	}
	return secrets.Source{BaseRepoID: t.BaseRepoID, HeadRepoID: t.HeadRepoID}
}

// TargetStore reads targets from PostgreSQL.
type TargetStore struct{ pool *pgxpool.Pool }

func NewTargetStore(pool *pgxpool.Pool) *TargetStore { return &TargetStore{pool: pool} }

// Current returns the request's target only while the request is still the
// PR's current, running, authorized work and repository access is intact.
func (s *TargetStore) Current(ctx context.Context, request environments.Request) (Target, error) {
	row, err := dbgen.New(s.pool).ExecutionTarget(ctx, request.PullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, failure(CodeNotCurrent, "The pull request no longer exists.", false, err)
	}
	if err != nil {
		return Target{}, failure(CodeUnavailable, "The build target could not be read.", true, fmt.Errorf("read execution target: %w", err))
	}
	return currentTarget(row, request)
}

func currentTarget(row dbgen.ExecutionTargetRow, request environments.Request) (Target, error) {
	switch {
	case row.State != "open" || row.Generation != request.Generation || row.HeadSha != request.HeadSHA:
		return Target{}, failure(CodeNotCurrent, "A newer commit or a close replaced this build.", false, nil)
	case !row.IntentState.Valid || row.IntentState.String != "running" || row.IntentRevision.Int64 != request.Revision ||
		row.IntentGeneration.Int64 != request.Generation || row.IntentHeadSha.String != request.HeadSHA:
		return Target{}, failure(CodeNotCurrent, "This build is no longer the authorized request.", false, nil)
	case !row.Accessible || row.InstallationStatus != "active" || !row.GithubRepoID.Valid || row.GithubInstallationID <= 0:
		return Target{}, failure(CodeInaccessible, "GitHub access to this repository was removed.", false, nil)
	}
	head := int64(0)
	if row.HeadRepoID.Valid {
		head = row.HeadRepoID.Int64
	}
	return Target{
		PullRequestID: row.PullRequestID, RepoID: row.RepoID, Number: row.Number, InstallationID: row.GithubInstallationID,
		BaseRepoID: row.GithubRepoID.Int64, HeadRepoID: head, Fork: row.IsFork || head != row.GithubRepoID.Int64,
	}, nil
}
