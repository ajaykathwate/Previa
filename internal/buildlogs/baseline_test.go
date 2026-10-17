package buildlogs_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/buildlogs"
)

// A baseline refresh's log continues across writers while the refresh runs,
// redacts like an attempt's, and fences writers once the refresh stops.
func TestBaselineRefreshLogs(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var refresh int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO baseline_refreshes (repo_id, sequence, trigger, state, started_at, log_open)
		VALUES ($1, 1, 'button', 'running', now(), true) RETURNING id`, f.repoID).Scan(&refresh); err != nil {
		t.Fatal(err)
	}
	build, err := f.store.OpenBaseline(ctx, refresh, redactor(t))
	if err != nil {
		t.Fatal(err)
	}
	build.System("Building the test data baseline from branch main.")
	_, _ = build.Write([]byte("step 1/3\nstep 2/3\n"))
	if err := build.Close(ctx); err != nil {
		t.Fatal(err)
	}
	seed, err := f.store.OpenBaseline(ctx, refresh, redactor(t, "generated-db-password"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = seed.Write([]byte("connecting with generated-db-password\nseeded 3 products\n"))
	if err := seed.Close(ctx); err != nil {
		t.Fatal(err)
	}
	lines, err := f.store.BaselineLinesFrom(ctx, refresh, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for i, l := range lines {
		if l.Sequence != int64(i) {
			t.Fatalf("one sequence across writers: %+v", lines)
		}
		texts = append(texts, l.Text)
	}
	joined := strings.Join(texts, "\n")
	if len(lines) != 5 || !lines[0].System || strings.Contains(joined, "generated-db-password") || !strings.Contains(joined, "seeded 3 products") {
		t.Fatalf("stored lines: %q", texts)
	}
	filter, err := buildlogs.ParseFilter("previa", "")
	if err != nil {
		t.Fatal(err)
	}
	if system, err := f.store.BaselineLinesBefore(ctx, refresh, 100, 10, filter); err != nil || len(system) != 1 {
		t.Fatalf("filtered: %+v %v", system, err)
	}
	search, _ := buildlogs.ParseFilter("", "STEP")
	if found, err := f.store.BaselineLinesBefore(ctx, refresh, 100, 10, search); err != nil || len(found) != 2 || found[0].Sequence != 1 {
		t.Fatalf("search, in order: %+v %v", found, err)
	}
	// The refresh finishes: a late writer stores nothing, a new one cannot open.
	late, err := f.store.OpenBaseline(ctx, refresh, redactor(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE baseline_refreshes SET state='succeeded', finished_at=now(), log_open=false WHERE id=$1", refresh); err != nil {
		t.Fatal(err)
	}
	_, _ = late.Write([]byte("too late\n"))
	if err := late.Close(ctx); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("a late writer is fenced: %v", err)
	}
	if _, err := f.store.OpenBaseline(ctx, refresh, redactor(t)); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("a finished refresh takes no writer: %v", err)
	}
	var stored int64
	if err := f.pool.QueryRow(ctx, "SELECT log_lines FROM baseline_refreshes WHERE id=$1", refresh).Scan(&stored); err != nil || stored != 5 {
		t.Fatalf("counters: %d %v", stored, err)
	}
}
