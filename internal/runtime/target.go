package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/runtime/dbgen"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	WorkspaceID            int64 // the stable workspace ID, for shared limits; 0 if unknown
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
type BuildAuthorizer interface {
	AuthorizeBuild(context.Context, int64) (bool, error)
}

type TargetStore struct {
	pool          *pgxpool.Pool
	authorization BuildAuthorizer
}

func (s *TargetStore) WithAuthorization(a BuildAuthorizer) *TargetStore {
	s.authorization = a
	return s
}

func (s *TargetStore) authorize(ctx context.Context, pr int64) error {
	if s.authorization == nil {
		return nil
	}
	allowed, err := s.authorization.AuthorizeBuild(ctx, pr)
	if err != nil {
		return failure(CodeUnavailable, "GitHub source permission could not be verified. Try again shortly.", true, err)
	}
	if !allowed {
		return failure(CodeNotCurrent, "This build's requesting member no longer has access.", false, nil)
	}
	return nil
}

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
	target, err := currentTarget(row, request)
	if err != nil {
		return Target{}, err
	}
	if err := s.authorize(ctx, request.PullRequestID); err != nil {
		return Target{}, err
	}
	return target, nil
}

func (s *TargetStore) AuthorizeRequest(ctx context.Context, pr int64) error {
	return s.authorize(ctx, pr)
}

// BaseRepository returns the GitHub ID of the PR's base repository.
func (s *TargetStore) AdmissionScope(ctx context.Context, pullRequestID int64) (Target, error) {
	row, err := dbgen.New(s.pool).AdmissionScope(ctx, pullRequestID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !row.GithubRepoID.Valid {
		return Target{}, failure(CodeNotCurrent, "The pull request no longer exists.", false, err)
	}
	if err != nil {
		return Target{}, failure(CodeUnavailable, "The build target could not be read.", true, fmt.Errorf("read base repository: %w", err))
	}
	return Target{PullRequestID: pullRequestID, BaseRepoID: row.GithubRepoID.Int64, WorkspaceID: row.WorkspaceID.Int64}, nil
}

func currentTarget(row dbgen.ExecutionTargetRow, request environments.Request) (Target, error) {
	switch {
	case !row.WorkspaceAuthorized || !row.WorkspaceID.Valid || row.WorkspaceID.Int64 <= 0 || row.State != "open" || row.Generation != request.Generation || row.HeadSha != request.HeadSHA:
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
		WorkspaceID: row.WorkspaceID.Int64,
	}, nil
}

// Components is what a ready candidate runs next to its app, for the
// dashboard and reports. It never holds credentials.
type Components struct {
	PullRequestID, Generation, Revision int64
	Services                            []ServiceComponent
	Setup                               bool
	Processes                           []string
	DocsPath                            string
	Outbound                            int  // approved destinations given to it
	OutboundWithheld                    bool // approvals exist but its source is untrusted
	// ConfigHash is the effective configuration's hash, which the candidate's
	// publication records (docs/30-reviews.md).
	ConfigHash string
}

// ServiceComponent is one managed service of a candidate.
type ServiceComponent struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// RecordComponents stores what a ready candidate runs, dropping rows of
// earlier requests the PR no longer serves.
func (s *TargetStore) RecordComponents(ctx context.Context, c Components) error {
	services, err := json.Marshal(append([]ServiceComponent{}, c.Services...))
	if err != nil {
		return err
	}
	processes, err := json.Marshal(append([]string{}, c.Processes...))
	if err != nil {
		return err
	}
	if err := dbgen.New(s.pool).RecordComponents(ctx, dbgen.RecordComponentsParams{
		PullRequestID: c.PullRequestID, Generation: c.Generation, Revision: c.Revision, Services: services, Setup: c.Setup,
		Processes: processes, DocsPath: pgtype.Text{String: c.DocsPath, Valid: c.DocsPath != ""},
		Outbound: int32(c.Outbound), OutboundWithheld: c.OutboundWithheld,
		ConfigHash: pgtype.Text{String: c.ConfigHash, Valid: c.ConfigHash != ""},
	}); err != nil {
		return fmt.Errorf("record preview components: %w", err)
	}
	return nil
}

// OutboundPreview is a pull request whose preview network may carry outbound
// connections now.
type OutboundPreview struct {
	PullRequestID, RepoID int64
	Trusted               bool // a same-repository pull request
}

// OutboundPreviews lists them.
func (s *TargetStore) OutboundPreviews(ctx context.Context) ([]OutboundPreview, error) {
	rows, err := dbgen.New(s.pool).ListOutboundPreviews(ctx)
	if err != nil {
		return nil, fmt.Errorf("list previews with outbound connections: %w", err)
	}
	previews := make([]OutboundPreview, len(rows))
	for i, row := range rows {
		previews[i] = OutboundPreview{PullRequestID: row.PullRequestID, RepoID: row.RepoID, Trusted: row.Trusted}
	}
	return previews, nil
}
