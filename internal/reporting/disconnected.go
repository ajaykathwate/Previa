package reporting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/reporting/dbgen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Disconnector gives an explicit local disconnection its final projection.
// It shares the normal reporting lease, updates only an App-owned living
// comment, and stops after three attempts or ten minutes. Cleanup is independent.
type Disconnector struct {
	pool   *pgxpool.Pool
	github GitHub
}

func NewDisconnector(pool *pgxpool.Pool, github GitHub) *Disconnector {
	return &Disconnector{pool: pool, github: github}
}

func (d *Disconnector) Disconnected(ctx context.Context, repo int64) (bool, error) {
	if _, err := d.pool.Exec(ctx, `UPDATE disconnect_comments c SET state='unavailable' FROM lifecycle_operations o
		WHERE c.operation_id=o.id AND c.state='pending' AND o.created_at<clock_timestamp()-interval '10 minutes'
		AND c.pull_request_id IN (SELECT id FROM pull_requests WHERE repo_id=$1)`, repo); err != nil {
		return false, fmt.Errorf("expire final comments: %w", err)
	}
	rows, err := d.pool.Query(ctx, `SELECT c.pull_request_id FROM disconnect_comments c JOIN pull_requests p ON p.id=c.pull_request_id
		WHERE p.repo_id=$1 AND c.state='pending' AND c.available_at<=clock_timestamp() ORDER BY c.pull_request_id LIMIT 10`, repo)
	if err != nil {
		return false, fmt.Errorf("list final comments: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, fmt.Errorf("read final comments: %w", err)
	}
	for _, id := range ids {
		if err := d.one(ctx, id); err != nil {
			return false, err
		}
	}
	var pending bool
	if err = d.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM disconnect_comments c JOIN pull_requests p ON p.id=c.pull_request_id WHERE p.repo_id=$1 AND c.state='pending')`, repo).Scan(&pending); err != nil {
		return false, fmt.Errorf("check final comment progress: %w", err)
	}
	return !pending, nil
}

func (d *Disconnector) one(ctx context.Context, pr int64) (result error) {
	q := dbgen.New(d.pool)
	owner := "disconnect:" + uuid.NewString()
	_, err := q.AcquireReportLease(ctx, dbgen.AcquireReportLeaseParams{Owner: text(owner), Lease: pgtype.Interval{Microseconds: (2 * time.Minute).Microseconds(), Valid: true}, PullRequestID: pr})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lease final comment: %w", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := q.ReleaseReportLease(c, dbgen.ReleaseReportLeaseParams{PullRequestID: pr, Owner: text(owner)}); err != nil {
			result = errors.Join(result, fmt.Errorf("release final comment lease: %w", err))
		}
	}()
	var installation, githubRepo, commentID int64
	var attempts int
	err = d.pool.QueryRow(ctx, `UPDATE disconnect_comments c SET attempts=attempts+1,available_at=clock_timestamp()+interval '30 seconds'
		FROM pull_requests p,repos r,installations i WHERE c.pull_request_id=$1 AND p.id=c.pull_request_id AND r.id=p.repo_id AND i.id=r.installation_id
		AND c.state='pending' AND c.attempts<3 RETURNING i.github_installation_id,r.github_repo_id,p.github_comment_id,c.attempts`, pr).
		Scan(&installation, &githubRepo, &commentID, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim final comment attempt: %w", err)
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	repository, callErr := d.github.Repository(c, installation, githubRepo)
	if callErr == nil {
		var comment Comment
		comment, callErr = d.github.GetComment(c, installation, repository, commentID)
		if callErr == nil && (!comment.AppOwned || !strings.Contains(comment.Body, Marker)) {
			callErr = ErrRejected
		}
		if callErr == nil {
			callErr = d.github.UpdateComment(c, installation, repository, commentID, Marker+"\n### Previa preview: Disconnected\n\nThis repository was disconnected in Previa. Its previews are being removed. Disconnecting does not uninstall the GitHub App.")
		}
	}
	state := "pending"
	if callErr == nil {
		state = "sent"
	} else if attempts >= 3 || errors.Is(callErr, ErrNotFound) || errors.Is(callErr, ErrPermissionDenied) || errors.Is(callErr, ErrRejected) {
		state = "unavailable"
	}
	if _, err = d.pool.Exec(ctx, `UPDATE disconnect_comments SET state=$2 WHERE pull_request_id=$1`, pr, state); err != nil {
		return fmt.Errorf("record final comment result: %w", err)
	}
	return nil
}
