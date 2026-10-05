package environments_test

import (
	"testing"

	env "github.com/ajaykathwatee/previa/internal/environments"
)

// PostgreSQL returns JSONB in its own normalized form. Scheduling the same
// work again must find the existing operation, not report a conflict.
func TestRescheduledCleanupIsTheSameOperation(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	resources := []env.Resource{{PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, Artifact: env.Artifact{ContainerID: "orphan", ImageTag: "orphan-image"}}}
	for range 2 {
		if err := f.repo.ScheduleResourceCleanup(t.Context(), a, resources); err != nil {
			t.Fatal(err)
		}
	}
	var operations int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact' AND pull_request_id=$1", a.PullRequestID).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("cleanup operations %d %v", operations, err)
	}
}
