package environments_test

import (
	"context"
	"sync"
	"testing"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/jackc/pgx/v5"
)

// heardPublications records what the activating transaction passes on.
type heardPublications struct {
	mu   sync.Mutex
	seen []env.Publication
}

func (h *heardPublications) PublishedTx(_ context.Context, _ pgx.Tx, p env.Publication) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, p)
	return nil
}

type publicationRow struct {
	Sequence             int32
	SHA                  string
	Generation, Revision int64
	Backfilled           bool
}

func publications(t *testing.T, f *fixture, pr int64) []publicationRow {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), "SELECT sequence, head_sha, generation, revision, backfilled FROM preview_publications WHERE pull_request_id=$1 ORDER BY sequence", pr)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[publicationRow])
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A publication is recorded when route activation makes a candidate the
// serving preview, once per candidate: a new commit and a same-commit
// Rebuild each get one, a failed update and a retry get none.
func TestPublicationsRecordEachServedCandidateOnce(t *testing.T) {
	f := newFixture(t)
	heard := &heardPublications{}
	f.repo.WithPublications(heard)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if got := publications(t, f, a.PullRequestID); len(got) != 0 {
		t.Fatalf("a built candidate the router has not accepted is not a publication: %+v", got)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	drainWith(t, f, executor)
	got := publications(t, f, a.PullRequestID)
	if len(got) != 1 || got[0].Sequence != 1 || got[0].SHA != "sha-A" || got[0].Generation != a.Generation || got[0].Backfilled {
		t.Fatalf("first publication: %+v", got)
	}

	// A failed update keeps the previous publication and records nothing.
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	executor.fail = map[env.OperationKind]error{env.ActivateRoute: &env.OperationFailure{Code: "unhealthy", Permanent: true}}
	processKind(t, f, executor, env.ActivateRoute, b.Generation)
	executor.fail = nil
	drainWith(t, f, executor)
	if got := publications(t, f, a.PullRequestID); len(got) != 1 {
		t.Fatalf("a failed update is not a publication: %+v", got)
	}

	c := f.event(t, "synchronize", "sha-C")
	if err := s.Build(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	drainWith(t, f, executor)
	got = publications(t, f, a.PullRequestID)
	if len(got) != 2 || got[1].Sequence != 2 || got[1].SHA != "sha-C" || got[1].Generation != c.Generation {
		t.Fatalf("a new commit's publication: %+v", got)
	}

	// An explicit Rebuild of the same commit: a new request revision.
	if _, err := f.pool.Exec(t.Context(), `UPDATE build_intents SET base_revision=revision, revision=revision+1, state='pending', started_at=NULL, completed_at=NULL,
		available_at=now()-interval '1 second' WHERE pull_request_id=$1`, c.PullRequestID); err != nil {
		t.Fatal(err)
	}
	rebuild := c
	rebuild.Revision = got[1].Revision + 1
	if err := s.Build(t.Context(), rebuild); err != nil {
		t.Fatal(err)
	}
	drainWith(t, f, executor)
	got = publications(t, f, a.PullRequestID)
	if len(got) != 3 || got[2].SHA != "sha-C" || got[2].Revision != rebuild.Revision || got[2].Sequence != 3 {
		t.Fatalf("a same-commit Rebuild is a new publication: %+v", got)
	}
	heard.mu.Lock()
	defer heard.mu.Unlock()
	if len(heard.seen) != 3 || heard.seen[0].Sequence != 1 || heard.seen[2].Sequence != 3 || heard.seen[2].HeadSHA != "sha-C" {
		t.Fatalf("each publication is passed on once: %+v", heard.seen)
	}
}
