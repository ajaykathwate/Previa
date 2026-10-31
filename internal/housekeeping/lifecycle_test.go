package housekeeping

import (
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/source"
)

func TestExplicitErasureRemovesOnlyOwnedSourcesAndWaitsForCaches(t *testing.T) {
	f := newFixture(t)
	request := f.open(t, 1, strings.Repeat("a", 40))
	var repo int64
	f.scan(t, `SELECT repo_id FROM pull_requests WHERE id=$1`, []any{&repo}, request.PullRequestID)
	caches := &fakeCaches{caches: []builds.Cache{{Name: "mine", BaseRepoID: 100}, {Name: "busy", BaseRepoID: 100, InUse: true}, {Name: "other", BaseRepoID: 999}}}
	sources := &fakeWorkspaces{list: []source.WorkspaceInfo{{Path: "mine", PullRequestID: request.PullRequestID, Marked: true}, {Path: "other", PullRequestID: 999, Marked: true}, {Path: "unknown"}}}
	cleanup := NewLifecycle(f.pool, caches, sources)
	done, err := cleanup.RepositoryDrained(t.Context(), repo)
	if err != nil || done {
		t.Fatal("busy cache falsely drained", done, err)
	}
	if len(sources.removed) != 1 || sources.removed[0] != "mine" {
		t.Fatal("wrong source cleanup", sources.removed)
	}
	if len(caches.removed) != 1 || caches.removed[0] != "mine" {
		t.Fatal("wrong cache cleanup", caches.removed)
	}
	caches.caches[0].InUse = false
	done, err = cleanup.RepositoryDrained(t.Context(), repo)
	if err != nil || !done {
		t.Fatal(done, err)
	}
	for _, name := range caches.removed {
		if name == "other" {
			t.Fatal("another repository's cache removed")
		}
	}
}
