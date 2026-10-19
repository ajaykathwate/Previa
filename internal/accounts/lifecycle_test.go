package accounts

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
)

func TestRepositoryDisconnectFencesAndWaitsForIndependentAck(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pr := f.id(t, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,1,'sha') RETURNING id`, f.repo)
	f.exec(t, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at) SELECT id,1,0,generation,head_sha,'automatic','automatic','f4-test-'||id,'queued',now() FROM pull_requests WHERE id=$1`, pr)
	other := f.id(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,302,'acme/other') RETURNING id`, f.installation)
	for _, tt := range []struct {
		actor, rev int64
		word       string
		want       error
	}{{f.member, -1, "DISCONNECT", ErrConflict}, {f.owner, 1, "delete", ErrConfirmation}, {999, 1, "DISCONNECT", ErrUnavailable}} {
		if _, err := f.s.DisconnectRepository(ctx, tt.actor, f.repo, tt.rev, tt.word); !errors.Is(err, tt.want) {
			t.Fatalf("invalid command: %v", err)
		}
	}
	o, err := f.s.DisconnectRepository(ctx, f.owner, f.repo, 0, "DISCONNECT")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT accessible::text FROM repos WHERE id=$1`, f.repo); got != "false" {
		t.Fatal(got)
	}
	if got := f.text(t, `SELECT accessible::text FROM repos WHERE id=$1`, other); got != "true" {
		t.Fatal("unrelated repository fenced")
	}
	if pending := f.id(t, `SELECT generation FROM pull_requests WHERE id=$1`, pr); pending <= 0 {
		t.Fatal("preview teardown not durably requested")
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT stage FROM lifecycle_operations WHERE id=$1`, o.ID); got == "complete" {
		t.Fatal("completed before cleanup")
	}
	f.exec(t, `UPDATE pull_requests SET teardown_completed_generation=teardown_requested_generation WHERE id=$1`, pr)
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT stage FROM lifecycle_operations WHERE id=$1`, o.ID); got == "complete" {
		t.Fatal("completed without independent acknowledgement")
	}
	if err = f.s.AcknowledgeJournal(ctx, f.journal(t)); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT stage FROM lifecycle_operations WHERE id=$1`, o.ID); got != "complete" {
		t.Fatal(got)
	}
	if n := f.id(t, `SELECT count(*) FROM pull_requests WHERE id=$1`, pr); n != 0 {
		t.Fatal("repository-owned PR survived erasure")
	}
	// A signed stale delivery cannot recreate the deleted repository.
	f.exec(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,301,'acme/api') ON CONFLICT(full_name) DO UPDATE SET accessible=true`, f.installation)
	if n := f.id(t, `SELECT count(*) FROM repos WHERE github_repo_id=301`); n != 0 {
		t.Fatal("late event resurrected repository")
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal("completion replay", err)
	}
}
func TestWorkspaceDeletionOrdersChildrenAndErasesSharedData(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.exec(t, `INSERT INTO support_requests(workspace_id,user_id,category,message,diagnostic_reference) VALUES($1,$2,'other','private message',gen_random_uuid())`, f.workspace, f.member)
	if _, err := f.s.StartWorkspaceDeletion(ctx, f.member, f.workspace, 1, "DELETE"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("admin deleted workspace", err)
	}
	o, err := f.s.StartWorkspaceDeletion(ctx, f.owner, f.workspace, 1, "DELETE")
	if err != nil {
		t.Fatal(err)
	}
	if n := f.id(t, `SELECT count(*) FROM workspace_memberships WHERE workspace_id=$1 AND state='active'`, f.workspace); n != 0 {
		t.Fatal("members retained access")
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT state FROM workspaces WHERE id=$1`, f.workspace); got != "deleting" {
		t.Fatal(got)
	}
	if err = f.s.AcknowledgeJournal(ctx, f.journal(t)); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.exec(t, `UPDATE lifecycle_operations SET available_at=clock_timestamp() WHERE stage<>'complete'`)
		if err = f.s.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.text(t, `SELECT state FROM workspaces WHERE id=$1`, f.workspace); got != "deleted" {
		t.Fatal(got)
	}
	if n := f.id(t, `SELECT count(*) FROM installations WHERE workspace_id=$1`, f.workspace); n != 0 {
		t.Fatal("connection survived")
	}
	if n := f.id(t, `SELECT count(*) FROM support_requests WHERE workspace_id=$1`, f.workspace); n != 0 {
		t.Fatal("private support message survived")
	}
}
func TestAccountDeletionOwnershipReceiptAndPseudonymousAttribution(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, _, err := f.s.StartAccountDeletion(ctx, f.owner, "DELETE"); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO support_requests(user_id,category,message,diagnostic_reference) VALUES($1,'other','private message',gen_random_uuid())`, f.member)
	o, receipt, err := f.s.StartAccountDeletion(ctx, f.member, "DELETE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = auth.NewRepository(f.pool).GetUser(ctx, f.member); err == nil {
		t.Fatal("deleted account still authenticates")
	}
	if _, err = f.s.ReadReceipt(ctx, strings.Repeat("a", 43)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("other receipt accepted", err)
	}
	if r, err := f.s.ReadReceipt(ctx, receipt); err != nil || r.ID != o.ID {
		t.Fatal(r, err)
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT email FROM users WHERE id=$1`, f.member); got == "" {
		t.Fatal("erased before journal acknowledgement")
	}
	if err = f.s.AcknowledgeJournal(ctx, f.journal(t)); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.text(t, `SELECT username||email||first_name FROM users WHERE id=$1`, f.member); got != "Deleted member" {
		t.Fatal("profile retained", got)
	}
	if n := f.id(t, `SELECT count(*) FROM support_requests WHERE user_id=$1`, f.member); n != 0 {
		t.Fatal("support data retained")
	}
	if got := f.text(t, `SELECT full_name FROM repos WHERE id=$1`, f.repo); got != "acme/api" {
		t.Fatal("other workspace data erased")
	}
	f.exec(t, `UPDATE lifecycle_operations SET receipt_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, o.ID)
	if _, err = f.s.ReadReceipt(ctx, receipt); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired receipt accepted", err)
	}
}
func TestLifecycleMigrationDownUp(t *testing.T) {
	f := newFixture(t)
	b, err := os.ReadFile("../../db/migrations/0043_account_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("missing rollback")
	}
	f.exec(t, down)
	f.exec(t, up)
	if n := f.id(t, `SELECT session_epoch FROM users WHERE id=$1`, f.owner); n != 0 {
		t.Fatal(n)
	}
}
func TestJournalValidationAndRetention(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	o, _, err := f.s.StartAccountDeletion(ctx, f.member, "DELETE")
	if err != nil {
		t.Fatal(err)
	}
	j := f.journal(t)
	if len(j.Events) < 1 {
		t.Fatal("empty export")
	}
	bad := j
	bad.Instance = "00000000-0000-0000-0000-000000000000"
	if err = f.s.ReplayJournal(ctx, bad); err == nil {
		t.Fatal("foreign instance accepted")
	}
	bad = j
	bad.Generated = time.Now().Add(-time.Hour)
	if err = f.s.ReplayJournal(ctx, bad); err == nil {
		t.Fatal("stale journal accepted")
	}
	for _, raw := range []string{"", `{}`, `{"version":9}`, `{"version":1,"secret":"do not accept"}`} {
		if _, err = DecodeJournal([]byte(raw), 1048576); err == nil {
			t.Fatal("malformed journal accepted", raw)
		}
	}
	f.exec(t, `UPDATE deletion_journal SET created_at=clock_timestamp()-interval '100 days'`)
	if len(f.journal(t).Events) == 0 {
		t.Fatal("unacknowledged event was pruned")
	}
	if err = f.s.AcknowledgeJournal(ctx, j); err != nil {
		t.Fatal(err)
	}
	if len(f.journal(t).Events) == 0 {
		t.Fatal("unfinished operation event was pruned")
	}
	if err = f.s.Process(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(f.journal(t).Events); n != 0 {
		t.Fatal("expired completed events retained", n)
	}
}
