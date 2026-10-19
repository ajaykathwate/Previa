package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DisconnectRepository revokes access and records cleanup before returning.
// Authorization and the target revision are rechecked under scope locks.
func (s *Service) DisconnectRepository(ctx context.Context, actor, repo, revision int64, confirm string) (Operation, error) {
	var out Operation
	if strings.TrimSpace(confirm) != "DISCONNECT" {
		return out, ErrConfirmation
	}
	if revision < 0 {
		return out, ErrConflict
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var installation, workspace int64
		if err := tx.QueryRow(ctx, `SELECT i.id,coalesce(i.workspace_id,0) FROM repos r JOIN installations i ON i.id=r.installation_id WHERE r.id=$1`, repo).Scan(&installation, &workspace); err != nil {
			return unavailableOrError("read disconnect repository scope", err)
		}
		if err := lockWorkspace(ctx, tx, workspace, actor, false); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM installations WHERE id=$1 FOR UPDATE`, installation); err != nil {
			return fmt.Errorf("lock disconnect installation: %w", err)
		}
		var allowed bool
		var current, currentInstallation, currentWorkspace int64
		if err := tx.QueryRow(ctx, `SELECT workspace_repository_access(r.id,$2,'manage'),i.revision,r.installation_id,i.workspace_id FROM repos r JOIN installations i ON i.id=r.installation_id WHERE r.id=$1 FOR UPDATE OF r`, repo, actor).Scan(&allowed, &current, &currentInstallation, &currentWorkspace); err != nil {
			return unavailableOrError("lock disconnect repository", err)
		}
		if currentInstallation != installation || currentWorkspace != workspace {
			return ErrConflict
		}
		if !allowed {
			return ErrUnavailable
		}
		if revision != current {
			return ErrConflict
		}
		var err error
		out, err = s.disconnectRepoTx(ctx, tx, repo, workspace, actor, nil)
		return err
	})
	return out, err
}

func lockWorkspace(ctx context.Context, tx pgx.Tx, workspace, actor int64, ownerOnly bool) error {
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
		WHERE w.id=$1 AND w.state='active' AND m.user_id=$2 AND m.state='active' FOR UPDATE OF w`, workspace, actor).Scan(&role)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock workspace authority: %w", err)
	}
	if err != nil || (role != "owner" && (ownerOnly || role != "admin")) {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) disconnectRepoTx(ctx context.Context, tx pgx.Tx, repo, workspace, actor int64, parent *string) (Operation, error) {
	var public, workspacePublic string
	var installation, githubRepo int64
	var disconnected bool
	err := tx.QueryRow(ctx, `SELECT r.public_id::text,w.public_id::text,i.github_installation_id,coalesce(r.github_repo_id,0),r.disconnected_at IS NOT NULL FROM repos r
		JOIN installations i ON i.id=r.installation_id JOIN workspaces w ON w.id=i.workspace_id WHERE r.id=$1 FOR UPDATE OF r`, repo).
		Scan(&public, &workspacePublic, &installation, &githubRepo, &disconnected)
	if err != nil {
		return Operation{}, fmt.Errorf("read disconnect identity: %w", err)
	}
	o, err := s.operation(ctx, tx, "repository", repo, workspace, actor, public, parent)
	if err != nil {
		return o, err
	}
	if disconnected {
		return o, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_disconnections(github_installation_id,github_repo_id) SELECT $1::bigint,$2::bigint WHERE $2::bigint>0 ON CONFLICT DO NOTHING`, installation, githubRepo); err != nil {
		return o, fmt.Errorf("retain repository revocation: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO deletion_journal(action,target,workspace,github_installation_id,github_repo_id,operation_id)
		SELECT 'repository',$1,$2,$3,nullif($4,0),$5 WHERE NOT EXISTS(SELECT 1 FROM deletion_journal WHERE operation_id=$5)`, public, workspacePublic, installation, githubRepo, o.ID); err != nil {
		return o, fmt.Errorf("journal repository disconnection: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO disconnect_comments(pull_request_id,operation_id) SELECT p.id,$2 FROM pull_requests p
		WHERE p.repo_id=$1 AND p.github_comment_id IS NOT NULL ON CONFLICT DO NOTHING`, repo, o.ID); err != nil {
		return o, fmt.Errorf("record final disconnect comments: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE repos SET accessible=false,disconnected_at=coalesce(disconnected_at,clock_timestamp()),build_mode='manual' WHERE id=$1`, repo); err != nil {
		return o, fmt.Errorf("disconnect repository: %w", err)
	}
	if s.previews == nil {
		return o, fmt.Errorf("repository lifecycle is not configured")
	}
	if err = s.previews.FenceRepositoryTx(ctx, tx, repo); err != nil {
		return o, err
	}
	return o, nil
}

func (s *Service) DisconnectInstallation(ctx context.Context, actor, installation, revision int64, confirm string) (Operation, error) {
	var o Operation
	if strings.TrimSpace(confirm) != "DISCONNECT" {
		return o, ErrConfirmation
	}
	if revision < 0 {
		return o, ErrConflict
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var workspace, current, githubID int64
		var public, workspacePublic string
		if err := tx.QueryRow(ctx, `SELECT i.workspace_id,i.revision,i.public_id::text,i.github_installation_id,w.public_id::text
			FROM installations i JOIN workspaces w ON w.id=i.workspace_id WHERE i.id=$1`, installation).
			Scan(&workspace, &current, &public, &githubID, &workspacePublic); err != nil {
			return unavailableOrError("read disconnect installation scope", err)
		}
		if err := lockWorkspace(ctx, tx, workspace, actor, false); err != nil {
			return err
		}
		var currentWorkspace int64
		if err := tx.QueryRow(ctx, `SELECT revision,workspace_id FROM installations WHERE id=$1 FOR UPDATE`, installation).Scan(&current, &currentWorkspace); err != nil {
			return unavailableOrError("lock disconnect installation", err)
		}
		if currentWorkspace != workspace {
			return ErrConflict
		}
		if revision != current {
			return ErrConflict
		}
		var err error
		o, err = s.operation(ctx, tx, "installation", installation, workspace, actor, public, nil)
		if err != nil {
			return err
		}
		if err = s.installationTx(ctx, tx, installation, workspace, actor, o, githubID, workspacePublic); err != nil {
			return err
		}
		return nil
	})
	return o, err
}

func (s *Service) installationTx(ctx context.Context, tx pgx.Tx, installation, workspace, actor int64, o Operation, githubID int64, workspacePublic string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO github_disconnections(github_installation_id) VALUES($1) ON CONFLICT DO NOTHING`, githubID); err != nil {
		return fmt.Errorf("retain installation revocation: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deletion_journal(action,target,workspace,github_installation_id,operation_id)
		SELECT 'installation',$1,$2,$3,$4 WHERE NOT EXISTS(SELECT 1 FROM deletion_journal WHERE operation_id=$4)`, o.TargetPublicID, workspacePublic, githubID, o.ID); err != nil {
		return fmt.Errorf("journal installation disconnection: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM repos WHERE installation_id=$1 ORDER BY id`, installation)
	if err != nil {
		return fmt.Errorf("list repositories to disconnect: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("read repositories to disconnect: %w", err)
	}
	for _, repo := range ids {
		if _, err = s.disconnectRepoTx(ctx, tx, repo, workspace, actor, &o.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE installations SET status='inactive',disconnected_at=coalesce(disconnected_at,clock_timestamp()),revision=revision+1 WHERE id=$1`, installation); err != nil {
		return fmt.Errorf("disconnect installation: %w", err)
	}
	return nil
}
