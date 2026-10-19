package accounts

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// An independently migrated schema represents the older database snapshot.
// Only persisted public identities and instance metadata match the source;
// none of the later revocations or operations exist in it.
func restoreFixture(t *testing.T, source *fixture) *fixture {
	t.Helper()
	dest := newFixture(t)
	for _, v := range []struct {
		table    string
		src, dst int64
	}{{"users", source.owner, dest.owner}, {"users", source.member, dest.member}, {"workspaces", source.workspace, dest.workspace}, {"installations", source.installation, dest.installation}, {"repos", source.repo, dest.repo}} {
		id := source.text(t, "SELECT public_id::text FROM "+v.table+" WHERE id=$1", v.src)
		dest.exec(t, "UPDATE "+v.table+" SET public_id=$1 WHERE id=$2", id, v.dst)
	}
	instance := source.text(t, `SELECT instance::text FROM deletion_journal_metadata`)
	dest.exec(t, `UPDATE deletion_journal_metadata SET instance=$1,snapshot_at=$2`, instance, time.Now().Add(-time.Hour))
	return dest
}
func TestIndependentJournalRestoresWorkspaceDeletionBeforeTraffic(t *testing.T) {
	source := newFixture(t)
	dest := restoreFixture(t, source)
	ctx := t.Context()
	dest.exec(t, `INSERT INTO sessions(token,data,expiry) VALUES('old-session',decode('00','hex'),clock_timestamp()+interval '1 day')`)
	if _, err := source.s.StartWorkspaceDeletion(ctx, source.owner, source.workspace, 1, "DELETE"); err != nil {
		t.Fatal(err)
	}
	j := source.journal(t)
	if err := dest.s.ReplayJournal(ctx, j); err != nil {
		t.Fatal(err)
	}
	if n := dest.id(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatal("restored sessions were resurrected")
	}
	if got := dest.text(t, `SELECT state FROM workspaces WHERE id=$1`, dest.workspace); got != "deleting" {
		t.Fatal("restored deleted workspace active", got)
	}
	if got := dest.text(t, `SELECT accessible::text FROM repos WHERE id=$1`, dest.repo); got != "false" {
		t.Fatal("restored repository regained access")
	}
	if n := dest.id(t, `SELECT count(*) FROM lifecycle_operations WHERE journal_ack_at IS NULL`); n != 0 {
		t.Fatal("replayed operations not acknowledged", n)
	}
	count := dest.id(t, `SELECT count(*) FROM lifecycle_operations`)
	if err := dest.s.ReplayJournal(ctx, j); err != nil {
		t.Fatal("crash replay", err)
	}
	if n := dest.id(t, `SELECT count(*) FROM lifecycle_operations`); n != count {
		t.Fatal("duplicate operations on replay")
	}
	for range 4 {
		dest.exec(t, `UPDATE lifecycle_operations SET available_at=clock_timestamp() WHERE stage<>'complete'`)
		if err := dest.s.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := dest.text(t, `SELECT state FROM workspaces WHERE id=$1`, dest.workspace); got != "deleted" {
		t.Fatal("restored erasure did not complete", got)
	}
}
func TestJournalRestoresOwnershipBeforePersonalDeletion(t *testing.T) {
	source := newFixture(t)
	dest := restoreFixture(t, source)
	ctx := t.Context()
	err := source.s.transaction(ctx, func(tx pgx.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{{`UPDATE workspace_memberships SET role='admin' WHERE workspace_id=$1 AND user_id=$2`, []any{source.workspace, source.owner}}, {`UPDATE workspace_memberships SET role='owner' WHERE workspace_id=$1 AND user_id=$2`, []any{source.workspace, source.member}}, {`UPDATE workspaces SET owner_user_id=$2 WHERE id=$1`, []any{source.workspace, source.member}}} {
			if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = source.s.StartAccountDeletion(ctx, source.owner, "DELETE"); err != nil {
		t.Fatal(err)
	}
	if err = dest.s.ReplayJournal(ctx, source.journal(t)); err != nil {
		t.Fatal(err)
	}
	if owner := dest.id(t, `SELECT owner_user_id FROM workspaces WHERE id=$1`, dest.workspace); owner != dest.member {
		t.Fatal("restored transfer lost", owner)
	}
	if n := dest.id(t, `SELECT count(*) FROM workspace_memberships WHERE user_id=$1 AND state='active'`, dest.owner); n != 0 {
		t.Fatal("deleted former owner regained membership")
	}
	dest.exec(t, `UPDATE lifecycle_operations SET available_at=clock_timestamp()`)
	if err = dest.s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dest.text(t, `SELECT username FROM users WHERE id=$1`, dest.owner); got != "Deleted member" {
		t.Fatal(got)
	}
	if got := dest.text(t, `SELECT full_name FROM repos WHERE id=$1`, dest.repo); got != "acme/api" {
		t.Fatal("shared source removed")
	}
}
func TestJournalDoesNotReplayRevocationsAlreadyInSnapshot(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.exec(t, `UPDATE workspace_memberships SET state='removed',removed_at=clock_timestamp() WHERE workspace_id=$1 AND user_id=$2`, f.workspace, f.member)
	j := f.journal(t)
	// A later explicit invitation re-granted access before this snapshot.
	f.exec(t, `UPDATE workspace_memberships SET state='active',removed_at=NULL WHERE workspace_id=$1 AND user_id=$2`, f.workspace, f.member)
	if err := f.s.ReplayJournal(ctx, j); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT state FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2`, f.workspace, f.member); got != "active" {
		t.Fatal("old journal undid a newer grant", got)
	}
}
