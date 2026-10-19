// Package support stores customer requests and the operator's public incidents.
package support

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInput = errors.New("choose a category and enter a message within the displayed size limit; repository and PR references must be accessible public IDs")
var ErrLimit = errors.New("daily support request limit reached; use the support email for urgent issues")
var ErrUnavailable = errors.New("support item unavailable or changed; refresh and try again")

type Settings struct {
	Email, Operator                                          string
	Operators                                                []int64
	MaxBytes, PerDay, RetentionDays, BackupDays, JournalDays int
}
type Service struct {
	pool     *pgxpool.Pool
	settings Settings
}

func NewService(p *pgxpool.Pool, s Settings) *Service {
	if s.MaxBytes == 0 {
		s.MaxBytes = 16384
	}
	if s.PerDay == 0 {
		s.PerDay = 5
	}
	if s.RetentionDays == 0 {
		s.RetentionDays = 90
	}
	if s.BackupDays == 0 {
		s.BackupDays = 30
	}
	if s.JournalDays == 0 {
		s.JournalDays = 90
	}
	if s.Operator == "" {
		s.Operator = "Previa operator"
	}
	return &Service{p, s}
}
func (s *Service) Operator(githubID int64) bool {
	for _, id := range s.settings.Operators {
		if id == githubID && id > 0 {
			return true
		}
	}
	return false
}

type Request struct {
	ID, Category, Message, Diagnostic, Repository, PR, State string
	Revision                                                 int64
	Created                                                  time.Time
}

func (s *Service) Create(ctx context.Context, user, workspace int64, category, message, repository, pr string) (string, error) {
	message = strings.TrimSpace(message)
	if !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') || len(message) == 0 || len(message) > s.settings.MaxBytes || !oneOf(category, "build", "access", "data", "abuse", "other") {
		return "", ErrInput
	}
	for _, ref := range []string{repository, pr} {
		if ref != "" {
			if _, err := uuid.Parse(ref); err != nil {
				return "", ErrInput
			}
		}
	}
	var id string
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.allowSubmissionTx(ctx, tx, user); err != nil {
			return err
		}
		if err := checkReferencesTx(ctx, tx, user, workspace, repository, pr); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO support_requests(user_id,workspace_id,category,message,diagnostic_reference,repository_reference,pull_request_reference) VALUES($1,nullif($2,0),$3,$4,gen_random_uuid(),nullif($5,'')::uuid,nullif($6,'')::uuid) RETURNING id::text`, user, workspace, category, message, repository, pr).Scan(&id); err != nil {
			return fmt.Errorf("save support request: %w", err)
		}
		return nil
	})
	return id, err
}
func (s *Service) Requests(ctx context.Context, user, github int64, operator bool) ([]Request, error) {
	if operator && !s.Operator(github) {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,category,CASE WHEN $2 THEN '' ELSE message END,diagnostic_reference::text,coalesce(repository_reference::text,''),coalesce(pull_request_reference::text,''),state,revision,created_at FROM support_requests WHERE $2 OR user_id=$1 ORDER BY created_at DESC,id LIMIT 100`, user, operator)
	if err != nil {
		return nil, fmt.Errorf("list support requests: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Request])
	if err != nil {
		return nil, fmt.Errorf("read support requests: %w", err)
	}
	return list, nil
}
func (s *Service) Read(ctx context.Context, user, github int64, id string, operator bool) (Request, error) {
	var out Request
	if _, err := uuid.Parse(id); err != nil || operator && !s.Operator(github) {
		return out, ErrUnavailable
	}
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text,category,message,diagnostic_reference::text,coalesce(repository_reference::text,''),coalesce(pull_request_reference::text,''),state,revision,created_at FROM support_requests WHERE id=$1 AND ($2 OR user_id=$3)`, id, operator, user).Scan(&out.ID, &out.Category, &out.Message, &out.Diagnostic, &out.Repository, &out.PR, &out.State, &out.Revision, &out.Created)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return fmt.Errorf("read support request: %w", err)
		}
		if operator {
			if _, err := tx.Exec(ctx, `INSERT INTO operator_activity(github_id,action,reference) VALUES($1,'support_read',$2)`, github, id); err != nil {
				return fmt.Errorf("audit support access: %w", err)
			}
		}
		return nil
	})
	return out, err
}
func (s *Service) Resolve(ctx context.Context, github int64, id string, revision int64) error {
	if !s.Operator(github) {
		return ErrUnavailable
	}
	if _, err := uuid.Parse(id); err != nil {
		return ErrUnavailable
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE support_requests SET state='resolved',resolved_at=clock_timestamp(),revision=revision+1 WHERE id=$1 AND revision=$2 AND state='open'`, id, revision)
		if err != nil {
			return fmt.Errorf("resolve support request: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operator_activity(github_id,action,reference) VALUES($1,'support_resolve',$2)`, github, id); err != nil {
			return fmt.Errorf("audit support resolution: %w", err)
		}
		return nil
	})
}
func (s *Service) EraseAccountTx(ctx context.Context, tx pgx.Tx, user int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM support_daily_usage WHERE user_id=$1`, user); err != nil {
		return fmt.Errorf("erase personal support allowance: %w", err)
	}
	_, err := tx.Exec(ctx, `DELETE FROM support_requests WHERE user_id=$1`, user)
	if err != nil {
		return fmt.Errorf("erase personal support requests: %w", err)
	}
	return nil
}
func (s *Service) EraseWorkspaceTx(ctx context.Context, tx pgx.Tx, workspace int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM support_requests WHERE workspace_id=$1`, workspace)
	if err != nil {
		return fmt.Errorf("erase workspace support requests: %w", err)
	}
	return nil
}
func (s *Service) Prune(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM support_daily_usage WHERE day<(clock_timestamp() AT TIME ZONE 'UTC')::date-2`); err != nil {
		return fmt.Errorf("prune support allowance history: %w", err)
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM support_requests WHERE state='resolved' AND resolved_at<clock_timestamp()-make_interval(days=>$1)`, s.settings.RetentionDays)
	if err != nil {
		return fmt.Errorf("prune resolved support requests: %w", err)
	}
	return nil
}
func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

// Cleanup exposes operational references without customer source or secrets.
type Cleanup struct{ ID, Kind, Stage, Reason, Created string }

func (s *Service) PendingCleanup(ctx context.Context, github int64) ([]Cleanup, error) {
	if !s.Operator(github) {
		return nil, ErrUnavailable
	}
	var out []Cleanup
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text,kind,stage,last_error,created_at::text FROM lifecycle_operations WHERE stage<>'complete' ORDER BY created_at,id LIMIT 100`)
		if err != nil {
			return fmt.Errorf("list pending cleanup references: %w", err)
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Cleanup])
		if err != nil {
			return fmt.Errorf("read pending cleanup references: %w", err)
		}
		for _, v := range out {
			if _, err := tx.Exec(ctx, `INSERT INTO operator_activity(github_id,action,reference) VALUES($1,'cleanup_read',$2)`, github, v.ID); err != nil {
				return fmt.Errorf("audit cleanup reference read: %w", err)
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) allowSubmissionTx(ctx context.Context, tx pgx.Tx, user int64) error {
	// Serialize the finite per-person allowance, including parallel submissions.
	var liveUser int64
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, user).Scan(&liveUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		return fmt.Errorf("lock support submitter: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `INSERT INTO support_daily_usage(user_id,day,requests) VALUES($1,(clock_timestamp() AT TIME ZONE 'UTC')::date,1)
	 ON CONFLICT(user_id,day) DO UPDATE SET requests=support_daily_usage.requests+1 WHERE support_daily_usage.requests<$2 RETURNING requests`, user, s.settings.PerDay).Scan(&count); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLimit
		}
		return fmt.Errorf("count support allowance: %w", err)
	}

	return nil
}

func checkReferencesTx(ctx context.Context, tx pgx.Tx, user, workspace int64, repository, pr string) error {
	if workspace > 0 {
		var selected int64
		if err := tx.QueryRow(ctx, `SELECT w.id FROM workspace_memberships m JOIN workspaces w ON w.id=m.workspace_id WHERE m.workspace_id=$1 AND m.user_id=$2 AND m.state='active' AND w.state='active' FOR SHARE OF w`, workspace, user).Scan(&selected); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnavailable
			}
			return fmt.Errorf("check support workspace: %w", err)
		}
	}
	if repository != "" {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repos WHERE public_id=$1 AND workspace_repository_access(id,$2,'read'))`, repository, user).Scan(&allowed); err != nil {
			return fmt.Errorf("check support repository: %w", err)
		}
		if !allowed {
			return ErrInput
		}
	}
	if pr != "" {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pull_requests p JOIN repos r ON r.id=p.repo_id WHERE p.public_id=$1 AND workspace_repository_access(r.id,$2,'read') AND ($3='' OR r.public_id::text=$3))`, pr, user, repository).Scan(&allowed); err != nil {
			return fmt.Errorf("check support PR: %w", err)
		}
		if !allowed {
			return ErrInput
		}
	}

	return nil
}
