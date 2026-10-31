package accounts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/earlyaccess"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/internal/usage"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/google/uuid"
)

func TestEarlyAccessApprovalGatesConnectionsAndPhysicalCompute(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	gate := earlyaccess.NewService(f.pool, true, []int64{101})
	repo := installations.NewRepository(f.pool)
	installations.NewService(repo, nil, nil, nil).WithAdmission(gate)
	if allowed, err := gate.Allowed(ctx, f.workspace); err != nil || allowed {
		t.Fatalf("new workspace bypassed approval: %v", err)
	}
	linkCtx := scope.With(ctx, f.workspace, "owner")
	if err := repo.Link(linkCtx, 203, f.owner, 0, "acme", nil); !errors.Is(err, installations.ErrApprovalRequired) {
		t.Fatalf("pending connection accepted: %v", err)
	}
	pr := f.id(t, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,1,'sha') RETURNING id`, f.repo)
	f.exec(t, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at) SELECT id,1,0,generation,head_sha,'automatic','automatic','gate-test','queued',now() FROM pull_requests WHERE id=$1`, pr)
	store := usage.NewStore(f.pool, 0, 0).WithAdmission(gate)
	admit := func(key string) string {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		reason, _, err := store.AdmitPRTx(ctx, tx, pr, key, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return reason
	}
	if reason := admit(uuid.NewString()); reason != usage.WaitRestricted {
		t.Fatal(reason)
	}
	if f.id(t, `SELECT count(*) FROM workspace_usage_attempts WHERE workspace_id=$1`, f.workspace) != 0 {
		t.Fatal("pending approval consumed allowance")
	}
	refresh := f.id(t, `INSERT INTO baseline_refreshes(repo_id,sequence,trigger) VALUES($1,1,'enabled') RETURNING id`, f.repo)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reason, _, err := store.AdmitBaselineTx(ctx, tx, f.workspace, refresh, uuid.NewString(), time.Now())
	if err != nil || reason != usage.WaitRestricted {
		_ = tx.Rollback(ctx)
		t.Fatalf("pending baseline admitted: %s %v", reason, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	public := f.text(t, `SELECT public_id::text FROM workspaces WHERE id=$1`, f.workspace)
	revision := f.id(t, `SELECT revision FROM workspaces WHERE id=$1`, f.workspace)
	if err := gate.Approve(ctx, 102, public, revision); !errors.Is(err, earlyaccess.ErrUnavailable) {
		t.Fatal("customer admin approved itself")
	}
	if err := gate.Approve(ctx, 101, public, revision+1); !errors.Is(err, earlyaccess.ErrUnavailable) {
		t.Fatal("stale approval accepted")
	}
	if err := gate.Approve(ctx, 101, public, revision); err != nil {
		t.Fatal(err)
	}
	if err := gate.Approve(ctx, 101, public, revision); err != nil {
		t.Fatal("duplicate approval not idempotent", err)
	}
	if f.id(t, `SELECT count(*) FROM workspace_approvals WHERE workspace_id=$1`, f.workspace) != 1 {
		t.Fatal("approval audit missing or duplicated")
	}
	if f.id(t, `SELECT count(*) FROM workspace_usage_attempts WHERE workspace_id=$1`, f.workspace) != 0 {
		t.Fatal("approval started compute")
	}
	approvedKey := uuid.NewString()
	if reason := admit(approvedKey); reason != "" {
		t.Fatal(reason)
	}
	if err := repo.Link(linkCtx, 203, f.owner, 0, "acme", nil); err != nil {
		t.Fatal("approved connection failed", err)
	}
	// A restored/changed pending scope also cannot execute a prior reservation.
	f.exec(t, `UPDATE workspaces SET early_access_state='pending' WHERE id=$1`, f.workspace)
	called := false
	err = store.Run(ctx, approvedKey, func(_ context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("pending workspace executed an old reservation")
	}
}

func TestAdmissionMigrationPreservesExistingWorkspaces(t *testing.T) {
	f := newFixture(t)
	b, err := os.ReadFile("../../db/migrations/0045_early_access.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("missing down migration")
	}
	if _, err = f.pool.Exec(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	gate := earlyaccess.NewService(f.pool, true, nil)
	allowed, err := gate.Allowed(t.Context(), f.workspace)
	if err != nil || !allowed {
		t.Fatalf("existing workspace not preserved: %v", err)
	}
	newID, err := workspaces.NewService(workspaces.NewRepository(f.pool), nil, nil, nil).Create(t.Context(), f.owner, "New pending")
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err = gate.Allowed(t.Context(), newID); err != nil || allowed {
		t.Fatal("new workspace did not await approval")
	}
	if err = gate.Approve(t.Context(), 101, f.text(t, `SELECT public_id::text FROM workspaces WHERE id=$1`, newID), 1); !errors.Is(err, earlyaccess.ErrUnavailable) {
		t.Fatal("empty operators granted approval")
	}
}

type pendingOwner struct{ id int64 }

func (o pendingOwner) UserIDs(context.Context) (int64, int64, bool) { return o.id, 101, true }
func (pendingOwner) Username(context.Context) (string, bool)        { return "owner", true }
func (pendingOwner) CSRFToken(context.Context) string               { return "csrf" }

// A pending workspace's Owner who uses Install GitHub or Build as a plain form
// lands on the early-access status page, not on a bare text response; an
// in-page request gets the short reason it shows inline. Nothing reaches the
// connection or the build until the workspace is approved.
func TestPendingWorkspaceRefusalsExplainThemselves(t *testing.T) {
	f := newFixture(t)
	h := earlyaccess.NewHandler(earlyaccess.NewService(f.pool, true, nil), pendingOwner{f.owner}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reached := false
	guarded := h.Guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	send := func(method, path string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r = r.WithContext(scope.With(r.Context(), f.workspace, "owner"))
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		guarded.ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct {
		name, method, path string
		header             map[string]string
		status             int
	}{
		{"Install GitHub form", http.MethodPost, "/installations/start", nil, http.StatusSeeOther},
		{"Build form", http.MethodPost, "/repositories/x/pull-requests/1/build", nil, http.StatusSeeOther},
		{"setup page", http.MethodGet, "/workspaces/x/setup", nil, http.StatusSeeOther},
		{"in-page build", http.MethodPost, "/repositories/x/pull-requests/1/build", map[string]string{"HX-Request": "true"}, http.StatusForbidden},
		{"in-place save", http.MethodPost, "/installations/start", map[string]string{"Accept": "application/json"}, http.StatusForbidden},
	} {
		w := send(c.method, c.path, c.header)
		switch {
		case w.Code != c.status:
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.status)
		case c.status == http.StatusSeeOther && w.Header().Get("Location") != "/early-access":
			t.Errorf("%s went to %q, not the status page", c.name, w.Header().Get("Location"))
		case c.status == http.StatusForbidden && !strings.Contains(w.Body.String(), "early-access approval"):
			t.Errorf("%s: unexplained refusal %q", c.name, w.Body)
		}
	}
	if reached {
		t.Fatal("a pending workspace reached a connection or build")
	}
	f.exec(t, `UPDATE workspaces SET early_access_state='approved' WHERE id=$1`, f.workspace)
	if send(http.MethodPost, "/installations/start", nil); !reached {
		t.Fatal("an approved workspace cannot connect")
	}
}
