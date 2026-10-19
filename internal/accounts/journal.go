package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Journal contains identifiers only. Encryption and independent storage belong
// to the host backup channel; acknowledgement happens only after its upload.
type Journal struct {
	Version   int            `json:"version"`
	Instance  string         `json:"instance"`
	Generated time.Time      `json:"generated_at"`
	Events    []JournalEvent `json:"events"`
}
type JournalEvent struct {
	ID              string    `json:"id"`
	Action          string    `json:"action"`
	Target          string    `json:"target"`
	Workspace       *string   `json:"workspace,omitempty"`
	Installation    *int64    `json:"installation,omitempty"`
	Repository      *int64    `json:"repository,omitempty"`
	User            *int64    `json:"user,omitempty"`
	Successor       *string   `json:"successor,omitempty"`
	SuccessorGitHub *int64    `json:"successor_github,omitempty"`
	Created         time.Time `json:"created_at"`
}

func (s *Service) ExportJournal(ctx context.Context, days, maxBytes int) ([]byte, error) {
	if days < 2 || maxBytes < 65536 {
		return nil, errors.New("invalid journal retention or size limit")
	}
	// Never prune an unacknowledged event or an unfinished deletion.
	_, err := s.pool.Exec(ctx, `DELETE FROM deletion_journal j WHERE j.acknowledged_at IS NOT NULL AND j.created_at<clock_timestamp()-make_interval(days=>$1) AND (j.operation_id IS NULL OR EXISTS(SELECT 1 FROM lifecycle_operations o WHERE o.id=j.operation_id AND o.stage='complete'))`, days)
	if err != nil {
		return nil, fmt.Errorf("prune acknowledged deletion journal: %w", err)
	}
	var j Journal
	j.Version = 1
	if err = s.pool.QueryRow(ctx, `SELECT instance::text,clock_timestamp() FROM deletion_journal_metadata WHERE singleton`).Scan(&j.Instance, &j.Generated); err != nil {
		return nil, fmt.Errorf("read journal identity: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT event_id::text,action,target::text,workspace::text,github_installation_id,github_repo_id,github_user_id,successor::text,successor_github_id,created_at FROM deletion_journal ORDER BY sequence LIMIT $1`, maxBytes/100+1)
	if err != nil {
		return nil, fmt.Errorf("export deletion journal: %w", err)
	}
	j.Events, err = pgx.CollectRows(rows, pgx.RowToStructByPos[JournalEvent])
	if err != nil {
		return nil, fmt.Errorf("decode deletion journal: %w", err)
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("encode deletion journal: %w", err)
	}
	if len(b) > maxBytes || len(j.Events) >= maxBytes/100+1 {
		return nil, errors.New("deletion journal exceeds configured bound; increase its bound or resolve pending cleanup before retrying")
	}
	return b, nil
}
func DecodeJournal(raw []byte, maxBytes int) (Journal, error) {
	var j Journal
	if len(raw) > maxBytes || len(raw) == 0 {
		return j, errors.New("deletion journal is empty or exceeds its configured bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return j, fmt.Errorf("decode deletion journal envelope: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return j, errors.New("deletion journal has trailing content")
	}
	if j.Version != 1 || j.Generated.IsZero() || j.Generated.After(time.Now().Add(5*time.Minute)) {
		return j, errors.New("unsupported deletion journal or invalid export time")
	}
	if _, err := uuid.Parse(j.Instance); err != nil {
		return j, errors.New("deletion journal instance is invalid")
	}
	seen := map[string]bool{}
	for _, e := range j.Events {
		if _, err := uuid.Parse(e.ID); err != nil {
			return j, errors.New("invalid journal event ID")
		}
		if seen[e.ID] {
			return j, errors.New("duplicate deletion journal event")
		}
		seen[e.ID] = true
		if err := validateJournalEvent(e, j.Generated); err != nil {
			return j, err
		}
	}
	return j, nil
}
func (s *Service) checkJournal(ctx context.Context, j Journal, restore bool) error {
	var instance string
	var snapshot time.Time
	if err := s.pool.QueryRow(ctx, `SELECT instance::text,snapshot_at FROM deletion_journal_metadata WHERE singleton`).Scan(&instance, &snapshot); err != nil {
		return fmt.Errorf("read restore journal boundary: %w", err)
	}
	if instance != j.Instance {
		return errors.New("deletion journal belongs to another instance")
	}
	if restore && j.Generated.Before(snapshot) {
		return errors.New("deletion journal is older than the backup snapshot; restore remains stopped")
	}
	return nil
}
func (s *Service) AcknowledgeJournal(ctx context.Context, j Journal) error {
	if err := s.checkJournal(ctx, j, false); err != nil {
		return err
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		for _, e := range j.Events {
			_, err := tx.Exec(ctx, `UPDATE deletion_journal SET acknowledged_at=coalesce(acknowledged_at,clock_timestamp()) WHERE event_id=$1 AND action=$2 AND target=$3`, e.ID, e.Action, e.Target)
			if err != nil {
				return fmt.Errorf("acknowledge journal event: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `UPDATE lifecycle_operations o SET journal_ack_at=coalesce(journal_ack_at,clock_timestamp()),available_at=clock_timestamp() WHERE EXISTS(SELECT 1 FROM deletion_journal j WHERE j.operation_id=o.id AND j.acknowledged_at IS NOT NULL)`)
		if err != nil {
			return fmt.Errorf("acknowledge deletion operations: %w", err)
		}
		return nil
	})
}

// ReplayJournal runs with server and workers stopped. Events already present in
// the snapshot are not reapplied: a later legitimate membership grant in that
// snapshot must not be undone by an older revocation. Missing events are applied
// in journal order and checkpointed individually, making crash replay safe.
func (s *Service) ReplayJournal(ctx context.Context, j Journal) error {
	if err := s.checkJournal(ctx, j, true); err != nil {
		return err
	}
	for _, e := range j.Events {
		var present bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deletion_journal WHERE event_id=$1)`, e.ID).Scan(&present); err != nil {
			return fmt.Errorf("check journal replay checkpoint: %w", err)
		}
		if !present {
			if err := s.replayEvent(ctx, e); err != nil {
				return fmt.Errorf("replay %s revocation: %w", e.Action, err)
			}
			_, err := s.pool.Exec(ctx, `INSERT INTO deletion_journal(event_id,action,target,workspace,github_installation_id,github_repo_id,github_user_id,successor,successor_github_id,created_at,acknowledged_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,clock_timestamp()) ON CONFLICT(event_id) DO NOTHING`, e.ID, e.Action, e.Target, e.Workspace, e.Installation, e.Repository, e.User, e.Successor, e.SuccessorGitHub, e.Created)
			if err != nil {
				return fmt.Errorf("checkpoint journal replay: %w", err)
			}
		}
		// A restored pending operation already has the fence. Its exported identity
		// still needs the remote acknowledgement before the worker may finish it.
		if _, err := s.pool.Exec(ctx, `UPDATE lifecycle_operations SET journal_ack_at=coalesce(journal_ack_at,clock_timestamp()),available_at=clock_timestamp() WHERE kind=$1 AND target_public_id=$2`, e.Action, e.Target); err != nil {
			return fmt.Errorf("acknowledge replayed operation: %w", err)
		}
	}
	if err := s.AcknowledgeJournal(ctx, j); err != nil {
		return err
	}
	// Restored opaque tokens may have been logged out or revoked after the
	// snapshot. Never resurrect them; every browser signs in again after restore.
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions`); err != nil {
			return fmt.Errorf("discard restored sessions: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET session_epoch=session_epoch+1`); err != nil {
			return fmt.Errorf("fence restored preview grants: %w", err)
		}
		return nil
	})
}
func (s *Service) replayEvent(ctx context.Context, e JournalEvent) error {
	switch e.Action {
	case "ownership":
		return s.replayOwnership(ctx, e)
	case "membership":
		return s.transaction(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('previa.journal_replay','on',true)`); err != nil {
				return fmt.Errorf("set membership replay guard: %w", err)
			}
			for _, q := range []string{`DELETE FROM repository_memberships WHERE user_id=(SELECT id FROM users WHERE github_id=$1) AND workspace_id=(SELECT id FROM workspaces WHERE public_id=$2)`, `UPDATE workspace_memberships SET state='removed',removed_at=clock_timestamp(),revision=revision+1 WHERE user_id=(SELECT id FROM users WHERE github_id=$1) AND workspace_id=(SELECT id FROM workspaces WHERE public_id=$2) AND state='active'`, `UPDATE workspace_preferences SET workspace_id=NULL WHERE user_id=(SELECT id FROM users WHERE github_id=$1) AND workspace_id=(SELECT id FROM workspaces WHERE public_id=$2)`} {
				if _, err := tx.Exec(ctx, q, e.User, e.Workspace); err != nil {
					return fmt.Errorf("restore membership revocation: %w", err)
				}
			}
			return nil
		})
	case "account":
		var actor int64
		var deleted *time.Time
		err := s.pool.QueryRow(ctx, `SELECT id,deleted_at FROM users WHERE github_id=$1`, e.User).Scan(&actor, &deleted)
		if errors.Is(err, pgx.ErrNoRows) { // Retain an identity-only tombstone if the account postdates this snapshot.
			_, err = s.pool.Exec(ctx, `INSERT INTO users(public_id,github_id,username,deleted_at,session_epoch) VALUES($1,$2,'Deleted member',clock_timestamp(),1)`, e.Target, e.User)
			if err != nil {
				return fmt.Errorf("restore absent account tombstone: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("find restored account: %w", err)
		}
		if deleted != nil {
			return nil
		}
		_, _, err = s.StartAccountDeletion(ctx, actor, "DELETE")
		return err
	case "repository", "installation", "workspace":
		return s.replayScope(ctx, e)
	}
	return errors.New("unsupported journal action")
}
func (s *Service) replayScope(ctx context.Context, e JournalEvent) error {
	// Permanent GitHub identity fences are required even if this target was
	// created after the snapshot and its local rows do not exist there.
	if e.Installation != nil && (e.Action != "repository" || e.Repository != nil) {
		repo := int64(0)
		if e.Action == "repository" {
			repo = *e.Repository
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO github_disconnections(github_installation_id,github_repo_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, e.Installation, repo); err != nil {
			return fmt.Errorf("restore GitHub revocation fence: %w", err)
		}
	}
	var id, actor, workspace, rev int64
	var state string
	var err error
	switch e.Action {
	case "workspace":
		err = s.pool.QueryRow(ctx, `SELECT id,owner_user_id,id,revision,state FROM workspaces WHERE public_id=$1`, e.Target).Scan(&id, &actor, &workspace, &rev, &state)
	case "installation":
		err = s.pool.QueryRow(ctx, `SELECT i.id,w.owner_user_id,w.id,i.revision,w.state FROM installations i JOIN workspaces w ON w.id=i.workspace_id WHERE i.public_id=$1`, e.Target).Scan(&id, &actor, &workspace, &rev, &state)
	case "repository":
		err = s.pool.QueryRow(ctx, `SELECT r.id,w.owner_user_id,w.id,i.revision,w.state FROM repos r JOIN installations i ON i.id=r.installation_id JOIN workspaces w ON w.id=i.workspace_id WHERE r.public_id=$1`, e.Target).Scan(&id, &actor, &workspace, &rev, &state)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find journal target: %w", err)
	}
	var exists bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lifecycle_operations WHERE kind=$1 AND target_public_id=$2)`, e.Action, e.Target).Scan(&exists); err != nil {
		return fmt.Errorf("find restored deletion: %w", err)
	}
	if exists || state == "deleted" {
		return nil
	}
	if e.Action == "workspace" {
		_, err = s.StartWorkspaceDeletion(ctx, actor, id, rev, "DELETE")
		return err
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		// A parent workspace may already be deleting. Its authority is irrelevant
		// here: the independent journal, not a restored member, authorizes the fence.
		if e.Action == "repository" {
			_, err = s.disconnectRepoTx(ctx, tx, id, workspace, actor, nil)
			return err
		}
		var wp string
		if err = tx.QueryRow(ctx, `SELECT public_id::text FROM workspaces WHERE id=$1`, workspace).Scan(&wp); err != nil {
			return fmt.Errorf("read restored workspace identity: %w", err)
		}
		o, err := s.operation(ctx, tx, "installation", id, workspace, actor, e.Target, nil)
		if err != nil {
			return err
		}
		return s.installationTx(ctx, tx, id, workspace, actor, o, *e.Installation, wp)
	})
}
func (s *Service) replayOwnership(ctx context.Context, e JournalEvent) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('previa.journal_replay','on',true)`); err != nil {
			return fmt.Errorf("set ownership replay guard: %w", err)
		}
		var workspace int64
		err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE public_id=$1 AND state='active' FOR UPDATE`, e.Target).Scan(&workspace)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find restored ownership scope: %w", err)
		}
		var owner int64
		if err = tx.QueryRow(ctx, `INSERT INTO users(public_id,github_id,username) VALUES($1,$2,'Restored member') ON CONFLICT(github_id) DO UPDATE SET github_id=EXCLUDED.github_id RETURNING id`, e.Successor, e.SuccessorGitHub).Scan(&owner); err != nil {
			return fmt.Errorf("restore successor identity: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE workspace_memberships SET role='admin',revision=revision+1 WHERE workspace_id=$1 AND role='owner' AND user_id<>$2`, workspace, owner); err != nil {
			return fmt.Errorf("restore former owner membership: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workspace_memberships(workspace_id,user_id,role,state) VALUES($1,$2,'owner','active') ON CONFLICT(workspace_id,user_id) DO UPDATE SET role='owner',state='active',removed_at=NULL,revision=workspace_memberships.revision+1`, workspace, owner); err != nil {
			return fmt.Errorf("restore successor membership: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE workspaces SET owner_user_id=$2,revision=revision+1 WHERE id=$1`, workspace, owner); err != nil {
			return fmt.Errorf("restore workspace ownership: %w", err)
		}
		return nil
	})
}

func validateJournalEvent(e JournalEvent, generated time.Time) error {
	if _, err := uuid.Parse(e.Target); err != nil {
		return errors.New("invalid journal target")
	}
	for _, id := range []*string{e.Workspace, e.Successor} {
		if id != nil {
			if _, err := uuid.Parse(*id); err != nil {
				return errors.New("invalid journal scope")
			}
		}
	}
	for _, id := range []*int64{e.Installation, e.Repository, e.User, e.SuccessorGitHub} {
		if id != nil && *id <= 0 {
			return errors.New("invalid journal GitHub identifier")
		}
	}
	if e.Created.IsZero() || e.Created.After(generated.Add(time.Minute)) {
		return errors.New("invalid journal event time")
	}
	valid := false
	switch e.Action {
	case "repository":
		valid = e.Installation != nil && e.Workspace != nil
	case "installation":
		valid = e.Installation != nil && e.Workspace != nil
	case "account":
		valid = e.User != nil
	case "workspace":
		valid = e.Workspace != nil && *e.Workspace == e.Target
	case "membership":
		valid = e.User != nil && e.Workspace != nil
	case "ownership":
		valid = e.Successor != nil && e.SuccessorGitHub != nil
	}
	if !valid {
		return errors.New("invalid deletion journal action or missing identifiers")
	}
	return nil
}
