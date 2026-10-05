package environments_test

import (
	"testing"

	"github.com/ajaykathwatee/previa/internal/webhooks"
)

// reportWork returns the PR's wanted report version and its report operations.
func reportWork(t *testing.T, f *fixture, pr int64) (wanted int64, operations int) {
	t.Helper()
	_ = f.pool.QueryRow(t.Context(), "SELECT wanted_version FROM pull_request_reports WHERE pull_request_id=$1", pr).Scan(&wanted)
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND pull_request_id=$1", pr).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	return wanted, operations
}

func TestReportableChangesRecordReportWork(t *testing.T) {
	f := newFixture(t)
	f.repo.WithReports()
	a := f.event(t, "opened", "sha-A") // observed with a new intent, then queued
	if wanted, ops := reportWork(t, f, a.PullRequestID); wanted != 2 || ops != 2 {
		t.Fatalf("after open: wanted %d, operations %d", wanted, ops)
	}
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil { // admitted, then built
		t.Fatal(err)
	}
	if wanted, _ := reportWork(t, f, a.PullRequestID); wanted != 4 {
		t.Fatalf("after build: wanted %d", wanted)
	}
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r}) // published
	if wanted, _ := reportWork(t, f, a.PullRequestID); wanted != 5 {
		t.Fatalf("after publication: wanted %d", wanted)
	}
	closed := f.event(t, "closed", "sha-A")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r})
	wanted, ops := reportWork(t, f, a.PullRequestID)
	if wanted != 8 || ops != 8 {
		t.Fatalf("after teardown: wanted %d, operations %d", wanted, ops)
	}
	var pending int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND state <> 'succeeded'").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("report operations left %d %v", pending, err)
	}
	// A duplicate teardown records nothing new.
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	if again, _ := reportWork(t, f, a.PullRequestID); again != wanted {
		t.Fatalf("duplicate teardown reported again: %d", again)
	}
}

func TestUntouchedManualPRsRecordNoReportWork(t *testing.T) {
	f := newFixture(t)
	f.repo.WithReports()
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET build_mode='manual' WHERE id=$1", f.repoID); err != nil {
		t.Fatal(err)
	}
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 2, HeadSHA: "sha-M", Action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if wanted, ops := reportWork(t, f, r.PullRequestID); wanted != 0 || ops != 0 {
		t.Fatalf("a PR nobody asked to build got report work: %d %d", wanted, ops)
	}
}

func TestReportsStayOffByDefault(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	if wanted, ops := reportWork(t, f, a.PullRequestID); wanted != 0 || ops != 0 {
		t.Fatalf("report work without WithReports: %d %d", wanted, ops)
	}
}
