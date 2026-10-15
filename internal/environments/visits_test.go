package environments_test

import (
	"testing"
)

// Each read of the router's counter adds only what is new; a newer version's
// service or a restarted router counts from zero; the last visit is noted.
func TestRecordRequestsAddsOnlyNewRequests(t *testing.T) {
	f := newFixture(t)
	r := f.prEvent(t, 41, "sha-visits")
	var env int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environments WHERE pull_request_id=$1", r.PullRequestID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		service  string
		requests float64
		added    int64
		total    int64
	}{
		{"preview-1-g1-r1", 10, 10, 10},
		{"preview-1-g1-r1", 10, 0, 10},
		{"preview-1-g1-r1", 15, 5, 15},
		{"preview-1-g1-r2", 4, 4, 19}, // a newer version
		{"preview-1-g1-r2", 2, 2, 21}, // the router restarted
	} {
		added, err := f.repo.RecordRequests(t.Context(), env, step.service, step.requests)
		if err != nil || added != step.added {
			t.Fatalf("step %d: added %d %v, want %d", i, added, err, step.added)
		}
		var total int64
		var visited bool
		if err := f.pool.QueryRow(t.Context(), "SELECT visits, last_visited_at IS NOT NULL FROM environments WHERE id=$1", env).Scan(&total, &visited); err != nil {
			t.Fatal(err)
		}
		if total != step.total || !visited {
			t.Fatalf("step %d: %d visits (noted %v), want %d", i, total, visited, step.total)
		}
	}
	if added, err := f.repo.RecordRequests(t.Context(), env+1000, "preview-x-g1-r1", 5); err != nil || added != 0 {
		t.Fatalf("an unknown environment: %d %v", added, err)
	}
}
