package usage

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOverviewEnforcesWorkspaceAndRepositoryPermissions(t *testing.T) {
	f := newFixture(t)
	f.now = time.Now().UTC()
	dev := f.member(t, "developer", true)
	reviewer := f.member(t, "reviewer", true)
	admin := f.member(t, "admin", false)
	key := f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(15 * time.Second)
	f.finish(t, key, "failed")
	visibleRepo := f.repo
	f.repo = f.id(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) SELECT installation_id,200,'acme/private' FROM repos WHERE id=$1 RETURNING id`, visibleRepo)
	key = f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(30 * time.Second)
	f.finish(t, key, "succeeded")
	owner, err := f.store.Overview(t.Context(), f.owner, f.workspace)
	if err != nil || !owner.Manager || owner.Consumed != 45000000 || len(owner.Repositories) != 2 {
		t.Fatalf("owner snapshot: %+v %v", owner, err)
	}
	a, err := f.store.Overview(t.Context(), admin, f.workspace)
	if err != nil || a.Consumed != owner.Consumed {
		t.Fatal(a, err)
	}
	d, err := f.store.Overview(t.Context(), dev, f.workspace)
	if err != nil || d.Manager || d.Consumed != 0 || d.Reserved != 0 || len(d.Repositories) != 1 || d.Repositories[0].ID != visibleRepo || d.Repositories[0].Consumed != 15000000 {
		t.Fatalf("developer must see granted repository usage only: %+v %v", d, err)
	}
	for _, actor := range []int64{reviewer, 0} {
		if _, err = f.store.Overview(t.Context(), actor, f.workspace); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unauthorized usage access", actor, err)
		}
	}
	if _, err = f.store.Overview(t.Context(), f.owner, f.workspace+1000); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cross-workspace access", err)
	}
	f.exec(t, `UPDATE workspace_memberships SET state='removed',removed_at=now() WHERE workspace_id=$1 AND user_id=$2`, f.workspace, dev)
	if _, err = f.store.Overview(t.Context(), dev, f.workspace); !errors.Is(err, ErrUnavailable) {
		t.Fatal("removed member kept usage access", err)
	}
}

func TestLegacyUsageIsNotImportedAndBlockedWorkHasReset(t *testing.T) {
	f := newFixture(t)
	f.now = time.Now().UTC()
	pr := f.pr(t, 1)
	f.exec(t, `INSERT INTO workspace_build_usage(workspace_id,day,build_seconds,builds) VALUES($1,current_date,999999,99)`, f.workspace)
	key := f.reserve(t, pr)
	f.finish(t, key, "failed")
	if f.consumed(t) != 0 {
		t.Fatal("legacy elapsed time was imported")
	}
	until := f.now.Add(time.Hour)
	f.exec(t, `UPDATE build_intents SET waiting_revision=revision,waiting_reason='workspace_minutes',usage_wait_until=$2,waiting_since=now(),waiting_checked_at=now() WHERE pull_request_id=$1`, pr, until)
	o, err := f.store.Overview(t.Context(), f.owner, f.workspace)
	if err != nil || len(o.Blocked) != 1 || o.Blocked[0].Reason != WaitMinutes || !o.Blocked[0].Until.Equal(until.Truncate(time.Microsecond)) {
		t.Fatal(o.Blocked, err)
	}
}

func TestRepositoryRebindingCannotMoveCapturedWorkspaceUsage(t *testing.T) {
	f := newFixture(t)
	f.now = time.Now().UTC()
	key := f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(5 * time.Second)
	f.finish(t, key, "succeeded")
	newOwner := f.id(t, `INSERT INTO users(github_id,username) VALUES(999,'another-owner') RETURNING id`)
	newWorkspace := f.id(t, `SELECT workspace_default($1)`, newOwner)
	f.exec(t, `UPDATE installations SET workspace_id=$2,owner_user_id=$3 WHERE id=(SELECT installation_id FROM repos WHERE id=$1)`, f.repo, newWorkspace, newOwner)
	current, err := f.store.Overview(t.Context(), newOwner, newWorkspace)
	if err != nil || current.Consumed != 0 || len(current.Repositories) != 1 || current.Repositories[0].Consumed != 0 || current.Repositories[0].Attempts != 0 {
		t.Fatal("new workspace received another workspace's historical usage", current, err)
	}
	previous, err := f.store.Overview(t.Context(), f.owner, f.workspace)
	if err != nil || previous.Consumed != 5000000 || len(previous.Repositories) != 0 {
		t.Fatal("captured usage moved with repository binding", previous, err)
	}
}

func TestMigrationUpDownUpPreservesLegacyHistory(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `INSERT INTO workspace_build_usage(workspace_id,day,build_seconds,builds) VALUES($1,current_date,30,1)`, f.workspace)
	migration, err := os.ReadFile("../../db/migrations/0042_workspace_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("missing down migration")
	}
	f.exec(t, down)
	f.exec(t, up)
	if got := f.id(t, `SELECT build_seconds FROM workspace_build_usage WHERE workspace_id=$1`, f.workspace); got != 30 {
		t.Fatal("migration erased legacy technical data", got)
	}
	// Entitlements use finite constraints, never an unlimited sentinel.
	if seconds := f.id(t, `SELECT included_seconds FROM workspace_entitlements WHERE workspace_id=$1`, f.workspace); seconds != 18000 {
		t.Fatal("cutover must grant 300 fresh minutes", seconds)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET included_seconds=0 WHERE workspace_id=$1`, f.workspace); err == nil {
		t.Fatal("unlimited entitlement allowed")
	}
}

func TestExpiredAdmissionNeverExecutesAndNewAttemptCanProceed(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(t, 1)
	key := f.reserve(t, pr)
	f.now = f.now.Add(61 * time.Second)
	if _, err := f.store.start(t.Context(), key); err == nil {
		t.Fatal("expired reservation executed")
	}
	if reason, _ := f.admit(t, pr, uuid.NewString()); reason != "" {
		t.Fatal("expired reservation blocked its retry", reason)
	}
	if f.consumed(t) != 0 {
		t.Fatal("admission delay consumed usage")
	}
}

func TestWorkerRestartsKeepUsageAndNewWorkspacesReceiveFinitePlan(t *testing.T) {
	f := newFixture(t)
	key := f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(5 * time.Second)
	f.finish(t, key, "failed")
	if err := f.store.Configure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.consumed(t) != 5000000 {
		t.Fatal("worker restart reset existing consumption")
	}
	user := f.id(t, `INSERT INTO users(github_id,username) VALUES(999,'new-owner') RETURNING id`)
	wid := f.id(t, `SELECT workspace_default($1)`, user)
	if seconds := f.id(t, `SELECT included_seconds FROM workspace_entitlements WHERE workspace_id=$1`, wid); seconds != 600 {
		t.Fatal("new workspace missed its entitlement", seconds)
	}
}
