package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Service) DeleteWorkspace(ctx context.Context, actor, workspace, revision int64, confirm string) error {
	_, err := s.StartWorkspaceDeletion(ctx, actor, workspace, revision, confirm)
	return err
}

func (s *Service) StartWorkspaceDeletion(ctx context.Context, actor, workspace, revision int64, confirm string) (Operation, error) {
	var o Operation
	if strings.TrimSpace(confirm) != "DELETE" {
		return o, ErrConfirmation
	}
	if revision <= 0 {
		return o, ErrConflict
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspace(ctx, tx, workspace, actor, true); err != nil {
			return err
		}
		var public string
		var current int64
		if err := tx.QueryRow(ctx, `SELECT public_id::text,revision FROM workspaces WHERE id=$1`, workspace).Scan(&public, &current); err != nil {
			return fmt.Errorf("read workspace deletion identity: %w", err)
		}
		if revision != current {
			return ErrConflict
		}
		var err error
		o, err = s.operation(ctx, tx, "workspace", workspace, workspace, actor, public, nil)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deletion_journal(action,target,workspace,operation_id) VALUES('workspace',$1,$1,$2)`, public, o.ID); err != nil {
			return fmt.Errorf("journal workspace deletion: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE workspaces SET state='deleting',revision=revision+1 WHERE id=$1`, workspace); err != nil {
			return fmt.Errorf("fence deleting workspace: %w", err)
		}
		if err := s.disconnectWorkspaceInstallationsTx(ctx, tx, workspace, actor, public, o.ID); err != nil {
			return err
		}
		for _, query := range []string{
			`UPDATE workspace_invitations SET state='revoked' WHERE workspace_id=$1 AND state='pending'`,
			`DELETE FROM workspace_transfers WHERE workspace_id=$1`,
			`DELETE FROM repository_memberships WHERE workspace_id=$1`,
			`UPDATE workspace_memberships SET state='removed',removed_at=clock_timestamp(),revision=revision+1 WHERE workspace_id=$1 AND state='active'`,
			`UPDATE workspace_preferences SET workspace_id=NULL WHERE workspace_id=$1`,
			`UPDATE workspace_entitlements SET compute_restricted=true WHERE workspace_id=$1`,
		} {
			if _, err = tx.Exec(ctx, query, workspace); err != nil {
				return fmt.Errorf("revoke deleting workspace access: %w", err)
			}
		}
		return nil
	})
	return o, err
}

// StartAccountDeletion removes sign-in authority immediately. Personal fields
// are erased after the independently stored journal acknowledges the request.
// A one-week receipt lets this browser see progress after its session is revoked.
func (s *Service) StartAccountDeletion(ctx context.Context, actor int64, confirm string) (Operation, string, error) {
	var o Operation
	if strings.TrimSpace(confirm) != "DELETE" {
		return o, "", ErrConfirmation
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return o, "", fmt.Errorf("create deletion receipt: %w", err)
	}
	receipt := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(receipt))
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var public string
		var github int64
		if err := tx.QueryRow(ctx, `SELECT public_id::text,github_id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, actor).Scan(&public, &github); err != nil {
			return unavailableOrError("lock deleting account", err)
		}
		var owns bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE owner_user_id=$1 AND state<>'deleted')`, actor).Scan(&owns); err != nil {
			return fmt.Errorf("check account ownership: %w", err)
		}
		if owns {
			return ErrOwnership
		}
		var err error
		o, err = s.operation(ctx, tx, "account", actor, 0, actor, public, nil)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE lifecycle_operations SET receipt_hash=$2,receipt_expires_at=clock_timestamp()+interval '7 days' WHERE id=$1`, o.ID, hash[:]); err != nil {
			return fmt.Errorf("save account deletion receipt: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deletion_journal(action,target,github_user_id,operation_id) VALUES('account',$1,$2,$3)`, public, github, o.ID); err != nil {
			return fmt.Errorf("journal account deletion: %w", err)
		}
		for _, query := range []string{
			`UPDATE users SET deleted_at=clock_timestamp(),session_epoch=session_epoch+1 WHERE id=$1`,
			`DELETE FROM repository_memberships WHERE user_id=$1`,
			`UPDATE workspace_memberships SET state='removed',removed_at=clock_timestamp(),revision=revision+1 WHERE user_id=$1 AND state='active'`,
			`UPDATE workspace_invitations SET state='revoked' WHERE github_id=(SELECT github_id FROM users WHERE id=$1) AND state='pending'`,
			`DELETE FROM workspace_preferences WHERE user_id=$1`,
		} {
			if _, err = tx.Exec(ctx, query, actor); err != nil {
				return fmt.Errorf("revoke deleted account access: %w", err)
			}
		}
		for _, fence := range s.accountFences {
			if err := fence.FenceAccountTx(ctx, tx, actor); err != nil {
				return err
			}
		}
		return nil
	})
	return o, receipt, err
}

func (s *Service) ReadReceipt(ctx context.Context, receipt string) (Operation, error) {
	if len(receipt) != 43 {
		return Operation{}, ErrUnavailable
	}
	hash := sha256.Sum256([]byte(receipt))
	var o Operation
	err := s.pool.QueryRow(ctx, `SELECT id::text,kind,stage,last_error,created_at,available_at,completed_at,journal_ack_at FROM lifecycle_operations
		WHERE receipt_hash=$1 AND receipt_expires_at>$2 AND kind='account'`, hash[:], time.Now()).Scan(&o.ID, &o.Kind, &o.Stage, &o.Error, &o.Created, &o.Available, &o.Completed, &o.JournalAcknowledged)
	if err != nil {
		return o, unavailableOrError("read account deletion receipt", err)
	}
	return o, nil
}

func (s *Service) disconnectWorkspaceInstallationsTx(ctx context.Context, tx pgx.Tx, workspace, actor int64, public, parent string) error {
	rows, err := tx.Query(ctx, `SELECT id,github_installation_id,public_id::text FROM installations WHERE workspace_id=$1 ORDER BY id FOR UPDATE`, workspace)
	if err != nil {
		return fmt.Errorf("list deleting workspace installations: %w", err)
	}
	type installation struct {
		ID, GitHub int64
		Public     string
	}
	installations, err := pgx.CollectRows(rows, pgx.RowToStructByPos[installation])
	if err != nil {
		return fmt.Errorf("read deleting workspace installations: %w", err)
	}
	for _, i := range installations {
		child, err := s.operation(ctx, tx, "installation", i.ID, workspace, actor, i.Public, &parent)
		if err != nil {
			return err
		}
		if err = s.installationTx(ctx, tx, i.ID, workspace, actor, child, i.GitHub, public); err != nil {
			return err
		}
	}

	return nil
}
