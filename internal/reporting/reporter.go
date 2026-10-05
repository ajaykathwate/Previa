package reporting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/reporting/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultCheckName = "Previa preview"
	commentPages     = 5
	maxPasses        = 3
)

// Reporter keeps each PR's living comment and current check run in line with
// its persisted state. It implements environments.OperationExecutor for
// report operations: whatever change queued a report, the reporter renders
// the PR as it is now, so a late report can never leave an older state
// behind. One reporter works on a PR at a time (a lease in PostgreSQL).
type Reporter struct {
	pool     *pgxpool.Pool
	github   GitHub
	settings Settings
	logger   *slog.Logger
	// Lease bounds how long a crashed reporter blocks the PR's reports. It
	// must exceed the report operation's timeout.
	Lease time.Duration
	now   func() time.Time
}

func NewReporter(pool *pgxpool.Pool, github GitHub, settings Settings, logger *slog.Logger) (*Reporter, error) {
	if pool == nil || github == nil || logger == nil {
		return nil, errors.New("reporter requires a database, GitHub and a logger")
	}
	if settings.CheckName == "" {
		settings.CheckName = defaultCheckName
	}
	return &Reporter{pool: pool, github: github, settings: settings, logger: logger, Lease: 2 * time.Minute, now: time.Now}, nil
}

// Execute performs one report operation.
func (r *Reporter) Execute(ctx context.Context, op environments.Operation) error {
	if op.Kind != environments.ReportStatus || op.PullRequestID <= 0 {
		return &environments.OperationFailure{Code: "invalid_operation", Permanent: true}
	}
	return r.Sync(ctx, op.PullRequestID, fmt.Sprintf("report:%d:%d", op.ID, r.now().UnixNano()))
}

// Sync converges one PR's GitHub presentation on its current state. It
// returns nil when GitHub shows the latest wanted version (or the PR cannot
// be reported to), and a classified *environments.OperationFailure otherwise.
func (r *Reporter) Sync(ctx context.Context, pullRequestID int64, owner string) error {
	q := dbgen.New(r.pool)
	_, err := q.AcquireReportLease(ctx, dbgen.AcquireReportLeaseParams{
		Owner: text(owner), Lease: pgtype.Interval{Microseconds: r.Lease.Microseconds(), Valid: true}, PullRequestID: pullRequestID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		lease, readErr := q.ReadReportLease(ctx, pullRequestID)
		if errors.Is(readErr, pgx.ErrNoRows) {
			return nil // Nothing was ever reported or wanted for this PR.
		}
		if readErr != nil {
			return &environments.OperationFailure{Code: "report_store_unavailable", Err: readErr}
		}
		wait := time.Second
		if lease.LeaseExpiresAt.Valid {
			wait = max(time.Until(lease.LeaseExpiresAt.Time), time.Second)
		}
		return &environments.OperationFailure{Code: "report_in_progress", RetryAfter: wait}
	}
	if err != nil {
		return &environments.OperationFailure{Code: "report_store_unavailable", Err: err}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := q.ReleaseReportLease(releaseCtx, dbgen.ReleaseReportLeaseParams{PullRequestID: pullRequestID, Owner: text(owner)}); err != nil {
			r.logger.WarnContext(ctx, "release report lease", "pr_id", pullRequestID, "error", err)
		}
	}()
	// Stop well before the lease can expire, so another reporter never starts
	// while this one may still be writing to GitHub.
	ctx, cancel := context.WithTimeout(ctx, r.Lease*3/4)
	defer cancel()
	for range maxPasses {
		done, err := r.pass(ctx, q, pullRequestID, owner)
		if err != nil || done {
			return err
		}
	}
	return nil // Still behind: the newer change has its own report operation.
}

// pass renders and writes the current state once. done is false when the
// PR changed while it was being reported.
func (r *Reporter) pass(ctx context.Context, q *dbgen.Queries, pullRequestID int64, owner string) (bool, error) {
	row, err := q.ReadReportSnapshot(ctx, pullRequestID)
	if err != nil {
		return false, &environments.OperationFailure{Code: "report_store_unavailable", Err: err}
	}
	wanted := row.WantedVersion.Int64
	if wanted <= row.ReportedVersion.Int64 {
		return true, nil
	}
	record := dbgen.RecordReportParams{
		PullRequestID: pullRequestID, Owner: text(owner), Version: wanted,
		CommentDigest: row.CommentDigest, CheckHeadSha: row.CheckHeadSha, CheckRevision: row.CheckRevision,
		CheckRunID: row.CheckRunID, CheckOpen: row.CheckOpen.Bool, CheckDigest: row.CheckDigest,
	}
	if row.InstallationStatus != "active" || !row.Accessible || !row.GithubRepoID.Valid {
		// Without access nothing can be written; the next change after access
		// returns reports again.
		record.LastErrorCode = pgtype.Text{String: "inaccessible", Valid: true}
		return r.finish(ctx, q, record, wanted)
	}
	view := Derive(snapshotOf(row), r.settings, row.GithubCommentID.Valid, r.now())
	repo, err := r.github.Repository(ctx, row.GithubInstallationID, row.GithubRepoID.Int64)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrPermissionDenied):
		record.LastErrorCode = pgtype.Text{String: "inaccessible", Valid: true}
		return r.finish(ctx, q, record, wanted)
	case err != nil:
		return false, failure(err)
	}
	target := reportTarget{row: row, repo: repo, owner: owner, view: view, version: wanted}
	var permanent []string
	commentErr := r.syncComment(ctx, q, target, &record)
	if commentErr != nil && !isPermanent(commentErr) {
		return false, r.partial(ctx, q, record, commentErr)
	}
	if commentErr != nil {
		permanent = append(permanent, "comment_"+codeOf(commentErr))
	}
	checkErr := r.syncCheck(ctx, target, &record)
	if checkErr != nil && !isPermanent(checkErr) {
		return false, r.partial(ctx, q, record, checkErr)
	}
	if checkErr != nil {
		permanent = append(permanent, "check_"+codeOf(checkErr))
	}
	if len(permanent) > 0 {
		record.LastErrorCode = pgtype.Text{String: permanent[0], Valid: true}
	}
	if len(permanent) == 2 {
		// Neither part can be written (permissions removed, for example).
		if _, err := r.finish(ctx, q, record, wanted); err != nil {
			return false, err
		}
		return true, &environments.OperationFailure{Code: permanent[0], Permanent: true}
	}
	return r.finish(ctx, q, record, wanted)
}

// finish records what was written for version and reports whether nothing
// newer is wanted. Losing the lease means a newer reporter may have written
// after this one: retrying repairs GitHub with the latest state.
func (r *Reporter) finish(ctx context.Context, q *dbgen.Queries, record dbgen.RecordReportParams, version int64) (bool, error) {
	affected, err := q.RecordReport(ctx, record)
	if err != nil {
		return false, &environments.OperationFailure{Code: "report_store_unavailable", Err: err}
	}
	if affected != 1 {
		return false, &environments.OperationFailure{Code: "report_lease_lost"}
	}
	row, err := q.ReadReportSnapshot(ctx, record.PullRequestID)
	if err != nil {
		return false, &environments.OperationFailure{Code: "report_store_unavailable", Err: err}
	}
	return row.WantedVersion.Int64 <= version, nil
}

// partial keeps what one part already wrote before retrying the other.
func (r *Reporter) partial(ctx context.Context, q *dbgen.Queries, record dbgen.RecordReportParams, cause error) error {
	record.Version = 0 // nothing newer counts as reported
	if _, err := q.RecordReport(ctx, record); err != nil {
		r.logger.WarnContext(ctx, "record partial report", "pr_id", record.PullRequestID, "error", err)
	}
	return failure(cause)
}

type reportTarget struct {
	row     dbgen.ReadReportSnapshotRow
	repo    Repo
	owner   string
	view    View
	version int64
}

// syncComment creates or updates the living comment. A stored ID that GitHub
// no longer has (deleted by a person) or a lost ID (GitHub accepted a create
// but saving its ID failed) leads to a bounded search for this App's marked
// comment before a new one is created, so a PR never gets two.
func (r *Reporter) syncComment(ctx context.Context, q *dbgen.Queries, t reportTarget, record *dbgen.RecordReportParams) error {
	if !t.view.Comment {
		return nil
	}
	digest := Digest(CommentContent(t.view))
	body := CommentBody(t.view, t.version)
	installation, number := t.row.GithubInstallationID, t.row.Number
	id := t.row.GithubCommentID.Int64
	if id != 0 && digest == t.row.CommentDigest.String {
		return nil // GitHub already shows this content.
	}
	if id != 0 {
		comment, err := r.github.GetComment(ctx, installation, t.repo, id)
		switch {
		case err == nil && comment.AppOwned:
			return r.updateComment(ctx, t, comment, body, digest, record)
		case err == nil, errors.Is(err, ErrNotFound):
			// Deleted, or not ours: find or recreate ours below.
		default:
			return err
		}
	}
	found, ok, err := r.github.FindComment(ctx, installation, t.repo, number, Marker, commentPages)
	if err != nil {
		return err
	}
	if ok {
		if err := r.saveCommentID(ctx, q, t, found.ID); err != nil {
			return err
		}
		return r.updateComment(ctx, t, found, body, digest, record)
	}
	created, err := r.github.CreateComment(ctx, installation, t.repo, number, body)
	if err != nil {
		return err
	}
	if err := r.saveCommentID(ctx, q, t, created); err != nil {
		return err
	}
	record.CommentDigest = pgtype.Text{String: digest, Valid: true}
	return nil
}

// updateComment edits the comment unless a newer report already wrote it.
// GitHub has no compare-and-set, so this check narrows rather than closes the
// window; the per-PR lease and the repair after a lost lease cover the rest.
func (r *Reporter) updateComment(ctx context.Context, t reportTarget, comment Comment, body, digest string, record *dbgen.RecordReportParams) error {
	if CommentVersion(comment.Body) > t.version {
		return nil
	}
	if err := r.github.UpdateComment(ctx, t.row.GithubInstallationID, t.repo, comment.ID, body); err != nil {
		return err
	}
	record.CommentDigest = pgtype.Text{String: digest, Valid: true}
	return nil
}

func (r *Reporter) saveCommentID(ctx context.Context, q *dbgen.Queries, t reportTarget, id int64) error {
	affected, err := q.SaveCommentID(ctx, dbgen.SaveCommentIDParams{CommentID: pgtype.Int8{Int64: id, Valid: true}, PullRequestID: t.row.ID, Owner: text(t.owner)})
	if err != nil {
		return &environments.OperationFailure{Code: "report_store_unavailable", Err: err}
	}
	if affected != 1 {
		return &environments.OperationFailure{Code: "report_lease_lost"}
	}
	return nil
}

// syncCheck keeps one check run per commit and request revision. A run that
// no longer describes the current request and never completed (superseded,
// or the preview was removed mid-build) is completed as cancelled; completed
// runs are left as they are.
func (r *Reporter) syncCheck(ctx context.Context, t reportTarget, record *dbgen.RecordReportParams) error {
	name, installation := r.settings.CheckName, t.row.GithubInstallationID
	stored := t.row
	next := t.view.Check
	sameIdentity := next != nil && stored.CheckHeadSha.String == next.HeadSHA && stored.CheckRevision.Valid && stored.CheckRevision.Int64 == next.Revision
	if !sameIdentity && stored.CheckRunID.Valid && stored.CheckOpen.Bool {
		closing := CheckRun{
			Name: name, HeadSHA: stored.CheckHeadSha.String, Status: "completed", Conclusion: "cancelled",
			ExternalID: ExternalID(stored.ID, &Check{HeadSHA: stored.CheckHeadSha.String, Revision: stored.CheckRevision.Int64}),
			Title:      "Stopped", Summary: "A newer commit or request replaced this one, or the preview was removed.",
		}
		if err := r.github.UpdateCheckRun(ctx, installation, t.repo, stored.CheckRunID.Int64, closing); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		record.CheckOpen = false
	}
	if next == nil {
		return nil
	}
	digest := CheckDigest(next)
	if sameIdentity && stored.CheckRunID.Valid && digest == stored.CheckDigest.String {
		return nil
	}
	run := CheckRun{
		Name: name, HeadSHA: next.HeadSHA, ExternalID: ExternalID(stored.ID, next), Status: next.Status,
		Conclusion: next.Conclusion, Title: next.Title, Summary: next.Summary, Details: next.DetailsURL,
	}
	var id int64
	if sameIdentity && stored.CheckRunID.Valid {
		id = stored.CheckRunID.Int64
	} else {
		found, ok, err := r.github.FindCheckRun(ctx, installation, t.repo, next.HeadSHA, name, run.ExternalID)
		if err != nil {
			return err
		}
		if ok {
			id = found
		}
	}
	if id != 0 {
		err := r.github.UpdateCheckRun(ctx, installation, t.repo, id, run)
		if errors.Is(err, ErrNotFound) {
			id = 0
		} else if err != nil {
			return err
		}
	}
	if id == 0 {
		created, err := r.github.CreateCheckRun(ctx, installation, t.repo, run)
		if err != nil {
			return err
		}
		id = created
	}
	record.CheckHeadSha = pgtype.Text{String: next.HeadSHA, Valid: true}
	record.CheckRevision = pgtype.Int8{Int64: next.Revision, Valid: true}
	record.CheckRunID = pgtype.Int8{Int64: id, Valid: true}
	record.CheckOpen = next.Status != "completed"
	record.CheckDigest = pgtype.Text{String: digest, Valid: true}
	return nil
}

func isPermanent(err error) bool {
	return errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrRejected) || errors.Is(err, ErrNotFound)
}

func codeOf(err error) string {
	switch {
	case errors.Is(err, ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, ErrRejected):
		return "rejected"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	}
	return "failed"
}

// failure classifies a GitHub or store error for the operation worker.
func failure(err error) error {
	var classified *environments.OperationFailure
	if errors.As(err, &classified) {
		return err
	}
	var rate *RateLimited
	if errors.As(err, &rate) {
		return &environments.OperationFailure{Code: "github_rate_limited", RetryAfter: rate.RetryAfter, Err: err}
	}
	switch {
	case errors.Is(err, ErrPermissionDenied):
		return &environments.OperationFailure{Code: "github_permission_denied", Permanent: true, Err: err}
	case errors.Is(err, ErrRejected), errors.Is(err, ErrNotFound):
		return &environments.OperationFailure{Code: "github_rejected", Permanent: true, Err: err}
	}
	return &environments.OperationFailure{Code: "github_unavailable", Err: err}
}

// snapshotOf turns the stored row into the persisted truth Derive reads.
func snapshotOf(row dbgen.ReadReportSnapshotRow) Snapshot {
	s := Snapshot{
		PullRequestID: row.ID, RepoID: row.RepoID, Number: row.Number, HeadSHA: row.HeadSha, State: row.State,
		Generation: row.Generation, BuildStatus: row.BuildStatus, NeedsRefresh: row.NeedsRefresh, Fork: row.IsFork,
		SecretCount: row.SecretCount, TeardownRequested: row.TeardownRequestedGeneration == row.Generation,
		TeardownReason: row.TeardownReason.String,
	}
	if row.IntentRevision.Valid {
		s.Intent = &Intent{
			Generation: row.IntentGeneration.Int64, Revision: row.IntentRevision.Int64, HeadSHA: row.IntentHeadSha.String,
			State: row.IntentState.String, AvailableAt: row.IntentAvailableAt.Time, Waiting: row.IntentWaiting,
		}
	}
	if row.EnvironmentID.Valid {
		s.Environment = &Environment{
			Status: row.EnvironmentStatus.String, Hostname: row.Subdomain.String, DeployedSHA: row.DeployedSha.String,
			AppliedGeneration: row.AppliedGeneration.Int64, AppliedRevision: row.AppliedRevision.Int64,
			LastErrorCode: row.LastErrorCode.String, ExpiresAt: row.ExpiresAt.Time,
		}
	}
	return s
}

func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
