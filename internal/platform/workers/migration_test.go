package workers

import (
	"os"
	"strings"
	"testing"
)

func TestPrivatePreviewMigrationDefaultsExistingWorkersToNoAccess(t *testing.T) {
	p := pool(t)
	ctx := t.Context()
	data, err := os.ReadFile("../../../db/migrations/0029_private_preview_capability.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("private-preview capability migration has no rollback")
	}
	if _, err := p.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	// This is the row an older routing worker writes before the upgrade.
	if _, err := p.Exec(ctx, `INSERT INTO worker_capabilities
		(worker_id, execution, routing, reporting, housekeeping, discovery)
		VALUES ('legacyworker000000', true, true, false, true, false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(p)
	if c, err := reader.Current(ctx); err != nil || !c.Routing || c.PrivatePreviews {
		t.Fatalf("upgraded legacy worker advertised private access: %+v %v", c, err)
	}
	if _, err := p.Exec(ctx, "UPDATE worker_capabilities SET private_previews=true"); err != nil {
		t.Fatal(err)
	}
	if c, err := reader.Current(ctx); err != nil || !c.PrivatePreviews {
		t.Fatalf("configured replacement cannot advertise private access: %+v %v", c, err)
	}
	// Rollback preserves the worker row. Reapplying the upgrade requires a
	// fresh announcement rather than inheriting an old private-access claim.
	if _, err := p.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	if c, live, err := reader.Live(ctx); err != nil || live != 1 || !c.Routing || c.PrivatePreviews {
		t.Fatalf("rollback/reapply inherited stale access or lost the worker: %+v live=%d %v", c, live, err)
	}
}
