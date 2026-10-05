// Package pullrequests owns observed PR state, discovery and the repository workspace.
package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrUnavailable = errors.New("repository is unavailable or not owned by this user")
var ErrSnapshotChanged = errors.New("repository changed during discovery; retry the page")

type Observation struct {
	Number                                 int32
	SHA, State, Title, Author, Branch, URL string
	HeadRepoID                             int64
	Fork                                   bool
	UpdatedAt                              time.Time
	Mergeable                              *bool
	MergeableState, BuildStatus            string
}

func FromGitHub(p *github.PullRequest, baseRepoID int64) (Observation, error) {
	if p == nil || p.GetNumber() <= 0 || p.GetNumber() > 2147483647 || p.GetHead().GetSHA() == "" || p.GetUpdatedAt().IsZero() {
		return Observation{}, errors.New("pull request is missing identity, head or source timestamp")
	}
	state := p.GetState()
	if p.GetMerged() || p.MergedAt != nil {
		state = "merged"
	}
	if state != "open" && state != "closed" && state != "merged" {
		return Observation{}, errors.New("invalid pull request state")
	}
	headID := p.GetHead().GetRepo().GetID()
	return Observation{Number: int32(p.GetNumber()), SHA: p.GetHead().GetSHA(), State: state, Title: p.GetTitle(), Author: p.GetUser().GetLogin(), Branch: p.GetHead().GetRef(), URL: p.GetHTMLURL(), Mergeable: p.Mergeable, MergeableState: p.GetMergeableState(), BuildStatus: MergeabilityStatus(p.Mergeable, p.GetMergeableState()), HeadRepoID: headID, Fork: headID == 0 || headID != baseRepoID, UpdatedAt: p.GetUpdatedAt().Time}, nil
}

type Observer interface {
	ObserveTx(context.Context, pgx.Tx, environments.Request) (environments.Request, error)
}

// ReportRecorder records, in the caller's transaction, that a PR's GitHub
// report must catch up with a change. An Observer that implements it (the
// environments broker) also hears about changes recorded outside webhook
// processing, such as discovery, refreshed mergeability or verification.
type ReportRecorder interface {
	RecordReportTx(ctx context.Context, tx pgx.Tx, pullRequestID int64) error
}

func reportChangeTx(ctx context.Context, tx pgx.Tx, observer Observer, pullRequestID int64) error {
	recorder, ok := observer.(ReportRecorder)
	if !ok {
		return nil
	}
	if err := recorder.RecordReportTx(ctx, tx, pullRequestID); err != nil {
		return fmt.Errorf("record PR report: %w", err)
	}
	return nil
}

type scope struct {
	install dbgen.LockInstallationRow
	repo    dbgen.LockRepositoryRow
}

func lockScope(ctx context.Context, tx pgx.Tx, repoID, userID int64) (scope, error) {
	q := dbgen.New(tx)
	id, err := q.FindScope(ctx, repoID)
	if err != nil {
		return scope{}, scopeError(err)
	}
	i, err := q.LockInstallation(ctx, id)
	if err != nil {
		return scope{}, scopeError(err)
	}
	r, err := q.LockRepository(ctx, repoID)
	if err != nil {
		return scope{}, scopeError(err)
	}
	if !r.GithubRepoID.Valid || r.GithubRepoID.Int64 <= 0 || r.InstallationID != i.ID || !r.Accessible || i.Status != "active" || !i.OwnerUserID.Valid || (userID > 0 && i.OwnerUserID.Int64 != userID) {
		return scope{}, ErrUnavailable
	}
	return scope{i, r}, nil
}
func scopeError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	return fmt.Errorf("read repository scope: %w", err)
}

// ObserveTx expects installation/repository locks in that order. Source time is
// distinct from local generation. Snapshot callers have additionally fenced the
// entire API read against repository observation and installation revisions.
// Only webhook observations reach observer.ObserveTx (and so can authorize
// work); other observations that change the PR only record a report.
func ObserveTx(ctx context.Context, tx pgx.Tx, repoID int64, o Observation, webhook bool, observer Observer) (bool, error) {
	q := dbgen.New(tx)
	old, err := q.LockPR(ctx, dbgen.LockPRParams{RepoID: repoID, Number: o.Number})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock observed PR: %w", err)
	}
	if !exists && o.State != "open" {
		return false, nil // A close can only finish a PR we tracked while open.
	}
	o.BuildStatus = MergeabilityStatus(o.Mergeable, o.MergeableState)
	// Rows created before source timestamps existed need an authoritative baseline
	// before a conflicting open observation can replace their known state.
	if exists && !old.SourceUpdatedAt.Valid && webhook &&
		(old.HeadSha != o.SHA || old.State != o.State) && (old.State != "open" || o.State == "open") {
		return false, requireRefreshTx(ctx, tx, q, old, observer)
	}
	if exists && old.SourceUpdatedAt.Valid {
		if o.UpdatedAt.Before(old.SourceUpdatedAt.Time) {
			return false, nil
		}
		conflicting := old.HeadSha != o.SHA || old.State != o.State
		if webhook && o.UpdatedAt.Equal(old.SourceUpdatedAt.Time) && old.NeedsRefresh && !conflicting {
			return false, nil
		}
		if webhook && o.UpdatedAt.Equal(old.SourceUpdatedAt.Time) && conflicting && (old.State != "open" || o.State == "open") {
			return false, requireRefreshTx(ctx, tx, q, old, observer)
		}
	}
	generation := old.Generation
	changed := !exists || old.HeadSha != o.SHA || old.State != o.State || old.BuildStatus != o.BuildStatus || old.NeedsRefresh
	if webhook || (exists && changed) {
		generation++
	}
	id, err := q.SaveObservation(ctx, dbgen.SaveObservationParams{RepoID: repoID, Number: o.Number, HeadSha: o.SHA, State: o.State, Generation: generation, Title: o.Title, Author: o.Author, HeadBranch: o.Branch, HtmlUrl: o.URL, HeadRepoID: pgtype.Int8{Int64: o.HeadRepoID, Valid: o.HeadRepoID > 0}, IsFork: o.Fork, SourceUpdatedAt: pgtype.Timestamptz{Time: o.UpdatedAt, Valid: true}, Mergeable: nullableBool(o.Mergeable), MergeableState: o.MergeableState, BuildStatus: o.BuildStatus})
	if err != nil {
		return false, fmt.Errorf("save PR observation: %w", err)
	}
	if webhook {
		if _, err := observer.ObserveTx(ctx, tx, environments.NewRequest(id, generation, o.SHA, o.State)); err != nil {
			return false, err
		}
	} else if changed {
		if err := q.InvalidateObservedIntent(ctx, dbgen.InvalidateObservedIntentParams{PullRequestID: id, Generation: generation}); err != nil {
			return false, err
		}
		// Close is a durable teardown request in the PR row. No runtime adapter is
		// present; discovery never creates build intent or executable work.
		if err := reportChangeTx(ctx, tx, observer, id); err != nil {
			return false, err
		}
	}
	return true, nil
}

// requireRefreshTx fences a conflicting observation until an authoritative
// read settles it.
func requireRefreshTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, old dbgen.PullRequest, observer Observer) error {
	if err := q.RequireRefresh(ctx, old.ID); err != nil {
		return err
	}
	if err := q.InvalidateObservedIntent(ctx, dbgen.InvalidateObservedIntentParams{PullRequestID: old.ID, Generation: old.Generation + 1}); err != nil {
		return err
	}
	return reportChangeTx(ctx, tx, observer, old.ID)
}

// ProcessWebhookTx resolves immutable GitHub repository identity under the same
// locks as policy changes, lifecycle updates and scheduling.
func ProcessWebhookTx(ctx context.Context, tx pgx.Tx, githubRepoID, installationID int64, o Observation, observer Observer, refreshers ...MergeabilityRefresher) error {
	q := dbgen.New(tx)
	repoID, err := q.FindGitHubRepository(ctx, dbgen.FindGitHubRepositoryParams{GithubRepoID: pgtype.Int8{Int64: githubRepoID, Valid: true}, GithubInstallationID: installationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s, err := lockScope(ctx, tx, repoID, 0)
	if errors.Is(err, ErrUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.install.GithubInstallationID != installationID {
		return nil
	}
	// Even stale/conflicting deliveries fence a concurrent API snapshot.
	if err := q.BumpObservation(ctx, repoID); err != nil {
		return err
	}
	applied, err := ObserveTx(ctx, tx, repoID, o, true, observer)
	if err == nil && applied && o.State == "open" && o.BuildStatus == "checking" {
		for _, refresher := range refreshers {
			if refresher == nil {
				continue
			}
			if err := refresher.RefreshMergeabilityTx(ctx, tx, repoID, o.Number, true); err != nil {
				return err
			}
		}
	}
	return err
}

// MergeabilityStatus fails closed while GitHub computes its asynchronous result.
func MergeabilityStatus(mergeable *bool, state string) string {
	if state == "dirty" || mergeable != nil && !*mergeable {
		return "conflicts"
	}
	if mergeable != nil && *mergeable || state == "clean" {
		return "open"
	}
	return "checking"
}
func nullableBool(value *bool) pgtype.Bool {
	if value == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *value, Valid: true}
}
func DisplayStatus(state, buildStatus string) string {
	if state != "open" {
		return state
	}
	return buildStatus
}
