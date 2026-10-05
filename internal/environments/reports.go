package environments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/jackc/pgx/v5"
)

var errorCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// safeErrorCode is a stable, lowercase failure category, or "" when code is
// not one: raw provider text never reaches reports or the database.
func safeErrorCode(code string) string {
	code = strings.ToLower(code)
	if !errorCodePattern.MatchString(code) {
		return ""
	}
	return code
}

// WithReports makes reportable changes enqueue durable report operations
// (docs/13-pr-feedback.md). Leave it off until a reporter executes them.
func (r *Repository) WithReports() *Repository {
	r.reports = true
	return r
}

// reportTx records, in the caller's transaction, that the PR's GitHub
// presentation must catch up with what the transaction changed: it bumps the
// PR's wanted report version and inserts a report operation for it. A report
// operation always renders the PR's current state, so a late one can never
// publish an older state. PRs with nothing to report (no living comment, build
// request or environment) are skipped.
func (r *Repository) reportTx(ctx context.Context, tx pgx.Tx, pullRequestID int64) error {
	if !r.reports || r.operations == nil {
		return nil
	}
	q := dbgen.New(tx)
	target, err := q.ReadReportTarget(ctx, pullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read report target: %w", err)
	}
	if !target.Reportable.Bool {
		return nil
	}
	wanted, err := q.BumpReportWanted(ctx, pullRequestID)
	if err != nil {
		return fmt.Errorf("record wanted report: %w", err)
	}
	request := NewRequest(target.ID, target.Generation, target.HeadSha, target.State)
	if target.CurrentIntent && target.State == "open" {
		request.Revision = target.Revision
	}
	if _, err := r.operations.EnqueueTx(ctx, tx, OperationSpec{
		Request: request, Kind: ReportStatus,
		IdempotencyKey: fmt.Sprintf("report:pr:%d:v:%d", pullRequestID, wanted),
		Payload:        OperationPayload{Request: request},
	}); err != nil {
		return fmt.Errorf("persist report operation: %w", err)
	}
	return nil
}
