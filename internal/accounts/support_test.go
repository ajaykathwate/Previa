package accounts

import (
	"errors"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/support"
	"github.com/jackc/pgx/v5"
)

func TestSupportScopeOperatorsAuditLimitsAndRetention(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s := support.NewService(f.pool, support.Settings{Operators: []int64{999}, MaxBytes: 1024, PerDay: 2, RetentionDays: 90})
	repo := f.text(t, `SELECT public_id::text FROM repos WHERE id=$1`, f.repo)
	if _, err := s.Create(ctx, f.owner, f.workspace, "other", strings.Repeat("a", 1025), "", ""); !errors.Is(err, support.ErrInput) {
		t.Fatal("oversize accepted", err)
	}
	if _, err := s.Create(ctx, f.owner, f.workspace, "other", "test", "00000000-0000-0000-0000-000000000000", ""); !errors.Is(err, support.ErrInput) {
		t.Fatal("unknown repository accepted", err)
	}
	id, err := s.Create(ctx, f.owner, f.workspace, "build", "<script>alert('secret')</script>", repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(ctx, f.member, 102, id, false); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("workspace admin read another member's support", err)
	}
	if _, err = s.Read(ctx, f.owner, 101, id, true); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("workspace owner gained operator access", err)
	}
	if list, err := s.Requests(ctx, f.member, 999, true); err != nil || len(list) != 1 || list[0].Message != "" {
		t.Fatal("operator index should not expose bodies", list, err)
	}
	if n := f.id(t, `SELECT count(*) FROM operator_activity`); n != 0 {
		t.Fatal("unexpected operator read")
	}
	r, err := s.Read(ctx, f.member, 999, id, true)
	if err != nil || r.Message != "<script>alert('secret')</script>" {
		t.Fatal(r, err)
	}
	if n := f.id(t, `SELECT count(*) FROM operator_activity WHERE action='support_read'`); n != 1 {
		t.Fatal("operator read not audited")
	}
	if err = s.Resolve(ctx, 101, id, r.Revision); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("owner resolved support", err)
	}
	if err = s.Resolve(ctx, 999, id, r.Revision+1); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("stale update accepted", err)
	}
	if err = s.Resolve(ctx, 999, id, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, f.owner, 0, "abuse", "second", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, f.owner, 0, "other", "third", "", ""); !errors.Is(err, support.ErrLimit) {
		t.Fatal("request allowance not enforced", err)
	}

	f.exec(t, `UPDATE support_requests SET resolved_at=clock_timestamp()-interval '91 days' WHERE id=$1`, id)
	if err = s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.id(t, `SELECT count(*) FROM support_requests`); n != 1 {
		t.Fatal("retention deleted open request or retained resolved body")
	}
	if support.NewService(f.pool, support.Settings{}).Operator(101) {
		t.Fatal("unset operators must deny everyone")
	}
}

func TestSupportAllowanceSurvivesWorkspaceMessageErasure(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s := support.NewService(f.pool, support.Settings{PerDay: 1})
	if _, err := s.Create(ctx, f.owner, f.workspace, "other", "a private request", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.s.transaction(ctx, func(tx pgx.Tx) error { return s.EraseWorkspaceTx(ctx, tx, f.workspace) }); err != nil {
		t.Fatal(err)
	}
	if n := f.id(t, `SELECT count(*) FROM support_requests`); n != 0 {
		t.Fatal("request content was not erased")
	}
	if _, err := s.Create(ctx, f.owner, 0, "other", "second request", "", ""); !errors.Is(err, support.ErrLimit) {
		t.Fatal("workspace message erasure reset the daily allowance", err)
	}
}
func TestPublicStatusUsesFreshProbesAndAuditedIncidents(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s := support.NewService(f.pool, support.Settings{Operators: []int64{999}})
	a, i, err := s.Status(ctx)
	if err != nil || len(a) != 2 || len(i) != 0 {
		t.Fatal(a, i, err)
	}
	for _, v := range a {
		if v.State != "unknown" {
			t.Fatal("unreported availability invented", v)
		}
	}
	if err = s.Observe(ctx, "worker", "builds", true); err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, "worker", "routing", false); err != nil {
		t.Fatal(err)
	}
	a, _, err = s.Status(ctx)
	if err != nil || a[0].State != "available" || a[1].State != "unavailable" {
		t.Fatal(a, err)
	}
	f.exec(t, `UPDATE service_availability SET checked_at=clock_timestamp()-interval '91 seconds'`)
	a, _, err = s.Status(ctx)
	if err != nil || a[0].State != "unknown" {
		t.Fatal("stale healthy shown", a, err)
	}
	if err = s.Incident(ctx, 101, "", "builds", "Problem", "investigating", 0); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("owner published public incident", err)
	}
	if err = s.Incident(ctx, 999, "", "builds", "Queue delay", "investigating", 0); err != nil {
		t.Fatal(err)
	}
	_, i, err = s.Status(ctx)
	if err != nil || len(i) != 1 {
		t.Fatal(i, err)
	}
	if err = s.Incident(ctx, 999, i[0].ID, "builds", "Recovered", "resolved", i[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.Incident(ctx, 999, i[0].ID, "builds", "Stale overwrite", "investigating", i[0].Revision); !errors.Is(err, support.ErrUnavailable) {
		t.Fatal("stale incident accepted", err)
	}
	if n := f.id(t, `SELECT count(*) FROM operator_activity WHERE action IN ('incident_create','incident_update')`); n != 2 {
		t.Fatal("incident changes not audited", n)
	}
}
