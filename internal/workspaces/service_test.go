package workspaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func isolatedPool(t *testing.T) *pgxpool.Pool { return isolatedPoolBefore(t, false) }

func isolatedPoolBefore(t *testing.T, before bool) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("workspaces_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if before && strings.HasPrefix(filepath.Base(file), "0030_") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return pool
}

type testGitHub struct {
	permission string
	fail       bool
}

func (g *testGitHub) LookupGitHubUser(_ context.Context, login string) (int64, string, error) {
	if g.fail {
		return 0, "", errors.New("GitHub unavailable")
	}
	switch login {
	case "admin":
		return 102, login, nil
	case "dev":
		return 103, login, nil
	case "reviewer":
		return 104, login, nil
	case "newbie":
		return 106, login, nil
	default:
		return 999, login, nil
	}
}
func (g *testGitHub) RepositoryUserPermission(context.Context, int64, string, string, int64) (string, error) {
	if g.fail {
		return "", errors.New("GitHub unavailable")
	}
	return g.permission, nil
}

type fixture struct {
	pool                                                  *pgxpool.Pool
	service                                               *Service
	github                                                *testGitHub
	owner, admin, dev, reviewer, wid, repo, otherRepo, pr int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := isolatedPool(t)
	ctx := t.Context()
	g := &testGitHub{permission: "write"}
	f := fixture{pool: pool, github: g, service: NewService(NewRepository(pool), g, g, nil)}
	add := func(dst *int64, query string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, query, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	add(&f.owner, "INSERT INTO users(github_id,username) VALUES(101,'owner') RETURNING id")
	add(&f.admin, "INSERT INTO users(github_id,username) VALUES(102,'admin') RETURNING id")
	add(&f.dev, "INSERT INTO users(github_id,username) VALUES(103,'dev') RETURNING id")
	add(&f.reviewer, "INSERT INTO users(github_id,username) VALUES(104,'reviewer') RETURNING id")
	var inst int64
	add(&inst, "INSERT INTO installations(github_installation_id,owner_user_id) VALUES(201,$1) RETURNING id", f.owner)
	add(&f.wid, "SELECT workspace_id FROM installations WHERE id=$1", inst)
	add(&f.repo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,301,'acme/api') RETURNING id", inst)
	add(&f.otherRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,302,'acme/private') RETURNING id", inst)
	add(&f.pr, "INSERT INTO pull_requests(repo_id,number,head_sha,head_repo_id,generation) VALUES($1,1,'sha',301,1) RETURNING id", f.repo)
	return f
}
func (f fixture) invite(t *testing.T, login, role string, user, githubID int64, repos []int64) string {
	t.Helper()
	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.service.Invite(t.Context(), InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: login, Role: role, Repositories: repos})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RespondInvitation(t.Context(), user, githubID, token, true); err != nil {
		t.Fatal(err)
	}
	return token
}
func TestWorkspaceRolesAndExplicitRepositoryScope(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	for _, tt := range []struct {
		user, repo int64
		ability    string
		allowed    bool
	}{
		{f.owner, f.otherRepo, "manage", true}, {f.admin, f.otherRepo, "manage", true},
		{f.dev, f.repo, "operate", true}, {f.dev, f.repo, "manage", false}, {f.dev, f.otherRepo, "read", false},
		{f.reviewer, f.repo, "preview", true}, {f.reviewer, f.repo, "operate", false}, {f.reviewer, f.otherRepo, "preview", false},
	} {
		_, err := f.service.Authorize(t.Context(), tt.user, tt.repo, tt.ability, false)
		if (err == nil) != tt.allowed {
			t.Errorf("user %d ability %s allowed=%v: %v", tt.user, tt.ability, tt.allowed, err)
		}
	}
	f.github.permission = "none"
	if _, err := f.service.Authorize(t.Context(), f.dev, f.repo, "operate", true); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	f.github.fail = true
	if _, err := f.service.Authorize(t.Context(), f.admin, f.repo, "manage", true); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal(err)
	}
	if _, err := f.service.Authorize(t.Context(), f.reviewer, f.repo, "preview", false); err != nil {
		t.Fatal(err)
	}
}
func TestInvitationIdentityRegenerationExpiryAndReplay(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w, _ := f.service.Read(ctx, f.owner, f.wid)
	token, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: "dev", Role: "developer", Repositories: []int64{f.repo}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Invitation(ctx, 104, token); !errors.Is(err, ErrInvitation) {
		t.Fatal("wrong account accepted", err)
	}
	v, err := f.service.Invitation(ctx, 103, token)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := f.service.ChangeInvitation(ctx, f.owner, f.wid, w.Revision, v.ID, v.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RespondInvitation(ctx, f.dev, 103, token, true); !errors.Is(err, ErrInvitation) {
		t.Fatal("old token accepted", err)
	}
	if _, err := f.service.RespondInvitation(ctx, f.reviewer, 103, newer, true); !errors.Is(err, ErrInvitation) {
		t.Fatal("forged user accepted", err)
	}
	for range 2 {
		if _, err := f.service.RespondInvitation(ctx, f.dev, 103, newer, true); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", f.wid, f.dev).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate membership: %d %v", count, err)
	}
	w, _ = f.service.Read(ctx, f.owner, f.wid)
	expired, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: "reviewer", Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	f.service.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if _, err := f.service.RespondInvitation(ctx, f.reviewer, 104, expired, true); !errors.Is(err, ErrInvitation) {
		t.Fatal("expired accepted", err)
	}
}
func TestRemovalFencesManualRequestsAndPreservesWorkspace(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	ctx := t.Context()
	_, err := f.pool.Exec(ctx, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,started_at)
 VALUES($1,1,0,1,'sha','manual','build',$2,'workspace-fixture-1','running',now(),now()-interval '1 minute')`, f.pr, f.dev)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := f.service.Read(ctx, f.owner, f.wid)
	m, err := dbgen.New(f.pool).LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: f.wid, UserID: f.dev})
	if err != nil {
		t.Fatal(err)
	}
	change := MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.dev, Revision: m.Revision, Remove: true}
	if err := f.service.ChangeMember(ctx, change); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Read(ctx, f.dev, f.wid); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	var state string
	var generation, seconds int64
	if err := f.pool.QueryRow(ctx, "SELECT b.state,p.generation FROM build_intents b JOIN pull_requests p ON p.id=b.pull_request_id WHERE p.id=$1", f.pr).Scan(&state, &generation); err != nil || state != "cancelled" || generation != 2 {
		t.Fatalf("unfenced request: %s %d %v", state, generation, err)
	}
	if err := f.pool.QueryRow(ctx, "SELECT build_seconds FROM workspace_build_usage WHERE workspace_id=$1", f.wid).Scan(&seconds); err != nil || seconds < 60 {
		t.Fatalf("cancelled usage lost: %d %v", seconds, err)
	}
	if err := f.service.ChangeMember(ctx, change); !errors.Is(err, ErrConflict) {
		t.Fatal("stale form accepted", err)
	}
	if allowed, err := f.service.AuthorizeBuild(ctx, f.pr); err != nil || allowed {
		t.Fatalf("removed actor authorized: %v %v", allowed, err)
	}
}
func TestOwnershipTransferKeepsConnectionAndRevokesDepartedInstaller(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	ctx := t.Context()
	w, _ := f.service.Read(ctx, f.owner, f.wid)
	if err := f.service.ChangeMember(ctx, MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.owner, Revision: w.MembershipRevision, Remove: true}); !errors.Is(err, ErrOwner) {
		t.Fatal(err)
	}
	if err := f.service.NominateOwner(ctx, f.owner, f.wid, w.Revision, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.service.AcceptOwnership(ctx, f.admin, f.wid, w.Revision+1, w.Revision+1); err != nil {
		t.Fatal(err)
	}
	if err := f.service.AcceptOwnership(ctx, f.admin, f.wid, w.Revision+1, w.Revision+1); err == nil {
		t.Fatal("replayed transfer")
	}
	w, err := f.service.Read(ctx, f.admin, f.wid)
	if err != nil || w.Role != "owner" {
		t.Fatal(w, err)
	}
	old, _ := f.service.Read(ctx, f.owner, f.wid)
	if err := f.service.ChangeMember(ctx, MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.owner, Revision: old.MembershipRevision, Remove: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Authorize(ctx, f.owner, f.repo, "manage", false); !errors.Is(err, ErrUnavailable) {
		t.Fatal("installer retained access", err)
	}
	if _, err := f.service.Authorize(ctx, f.admin, f.repo, "manage", false); err != nil {
		t.Fatal("new owner lost connection", err)
	}
}
func TestCrossWorkspaceGrantRejectedAtomically(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other, err := f.service.Create(ctx, f.admin, "Other team")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := f.service.Read(ctx, f.admin, other)
	if _, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.admin, WorkspaceID: other, Revision: w.Revision, Login: "dev", Role: "developer", Repositories: []int64{f.repo}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1", other).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial invitation saved: %d %v", count, err)
	}
}

func TestConcurrentOwnershipAcceptanceLeavesExactlyOneOwner(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	ctx := t.Context()
	w, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.NominateOwner(ctx, f.owner, f.wid, w.Revision, f.admin); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() { <-start; done <- f.service.AcceptOwnership(ctx, f.admin, f.wid, w.Revision+1, w.Revision+1) }()
	}
	close(start)
	success := 0
	for range 2 {
		if err := <-done; err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	var owners int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM workspace_memberships WHERE workspace_id=$1 AND role='owner' AND state='active'", f.wid).Scan(&owners); err != nil || owners != 1 || success != 1 {
		t.Fatalf("handover race: %d owners, %d successful accepts: %v", owners, success, err)
	}
}
func TestAdminCannotEscalateRolesAndRemovedInviterCannotGrantAccess(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	ctx := t.Context()
	w, _ := f.service.Read(ctx, f.admin, f.wid)
	m, err := dbgen.New(f.pool).LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: f.wid, UserID: f.dev})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ChangeMember(ctx, MemberChange{ActorID: f.admin, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.dev, Revision: m.Revision, Role: "admin"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("admin promoted an admin", err)
	}
	token, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.admin, WorkspaceID: f.wid, Revision: w.Revision, Login: "reviewer", Role: "reviewer", Repositories: []int64{f.repo}})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := f.service.Read(ctx, f.admin, f.wid)
	if err := f.service.ChangeMember(ctx, MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.admin, Revision: admin.MembershipRevision, Remove: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RespondInvitation(ctx, f.reviewer, 104, token, true); !errors.Is(err, ErrInvitation) {
		t.Fatal("removed inviter granted access", err)
	}
}
