package housekeeping

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/riverqueue/river"
)

func (f *fixture) resources(t *testing.T, dryRun bool, now time.Time) *Resources {
	t.Helper()
	settings := f.settings
	settings.DryRun = dryRun
	r, err := NewResources(f.repo, f.inventory, f.pool, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	return r
}

func TestOrphansWaitForTheGracePeriodAndDryRunOnlyLogs(t *testing.T) {
	f := newFixture(t)
	live := f.open(t, 1, "sha-live")
	f.publish(t, live, time.Hour)
	var generation int64
	f.scan(t, "SELECT generation FROM pull_requests WHERE id=$1", []any{&generation}, live.PullRequestID)
	now := time.Now()
	old := now.Add(-time.Hour)
	// The serving preview, an abandoned candidate of an earlier generation,
	// and a candidate created moments ago.
	f.inventory.add(environments.Resource{PullRequestID: live.PullRequestID, Generation: generation, Revision: 1,
		Artifact: environments.Artifact{ContainerID: "container-" + strconv.FormatInt(live.PullRequestID, 10), ImageTag: "image-" + strconv.FormatInt(live.PullRequestID, 10)}, CreatedAt: old})
	f.inventory.add(environments.Resource{PullRequestID: live.PullRequestID, Generation: generation - 1, Revision: 1,
		Artifact: environments.Artifact{ContainerID: "abandoned", ImageTag: "abandoned-image"}, CreatedAt: old})
	f.inventory.add(environments.Resource{PullRequestID: live.PullRequestID, Generation: generation - 1, Revision: 2,
		Artifact: environments.Artifact{ContainerID: "brand-new"}, CreatedAt: now.Add(-time.Minute)})

	pass, err := f.resources(t, true, now).RunOnce(t.Context())
	if err != nil || len(pass.Orphans) != 1 || pass.Orphans[0].Artifact.ContainerID != "abandoned" || pass.Young != 1 {
		t.Fatalf("dry run %+v %v", pass, err)
	}
	if n := f.count(t, "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact'"); n != 0 {
		t.Fatal("a dry run scheduled cleanup")
	}
	pass, err = f.resources(t, false, now).RunOnce(t.Context())
	if err != nil || len(pass.Orphans) != 1 {
		t.Fatalf("pass %+v %v", pass, err)
	}
	if n := f.count(t, "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact' AND payload->'resource'->'artifact'->>'container_id' = 'abandoned'"); n != 1 {
		t.Fatal("the abandoned candidate was not scheduled for cleanup")
	}
	if n := f.count(t, "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact' AND payload->'resource'->'artifact'->>'container_id' <> 'abandoned'"); n != 0 {
		t.Fatal("cleanup was scheduled for the serving preview or a young object")
	}
}

func TestObjectsOfUnknownPRsAreRemovedAfterTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.inventory.add(environments.Resource{PullRequestID: 9001, Artifact: environments.Artifact{ImageTag: "orphan-image"}, CreatedAt: now.Add(-time.Hour)})
	f.inventory.add(environments.Resource{PullRequestID: 9001, Artifact: environments.Artifact{ContainerID: "orphan-container"}, CreatedAt: now.Add(-time.Hour)})
	f.inventory.add(environments.Resource{PullRequestID: 9002, Artifact: environments.Artifact{ContainerID: "just-created"}, CreatedAt: now.Add(-time.Minute)})
	pass, err := f.resources(t, true, now).RunOnce(t.Context())
	if err != nil || pass.UnknownPRs != 2 || len(pass.Removed) != 2 || len(f.inventory.removed) != 0 {
		t.Fatalf("dry run %+v %v removed %v", pass, err, f.inventory.removed)
	}
	if _, err := f.resources(t, false, now).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.inventory.removed) != 2 || f.inventory.removed[0].Artifact.ContainerID != "orphan-container" {
		t.Fatalf("removed %+v (containers must go before images)", f.inventory.removed)
	}
	if left, _ := f.inventory.Resources(t.Context(), 9002); len(left) != 1 {
		t.Fatal("an object younger than the grace period was removed")
	}
}

type fakeCaches struct {
	mu       sync.Mutex
	caches   []builds.Cache
	dangling []string
	removed  []string
}

func (c *fakeCaches) Caches(context.Context) ([]builds.Cache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]builds.Cache(nil), c.caches...), nil
}

func (c *fakeCaches) RemoveCache(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, name)
	for i, cache := range c.caches {
		if cache.Name == name {
			c.caches = append(c.caches[:i], c.caches[i+1:]...)
			break
		}
	}
	return nil
}

func (c *fakeCaches) DanglingImages(context.Context) ([]string, error) { return c.dangling, nil }

func (c *fakeCaches) RemoveDanglingImage(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, id)
	return nil
}

type fakeWorkspaces struct {
	list    []source.WorkspaceInfo
	removed []string
}

func (w *fakeWorkspaces) List() ([]source.WorkspaceInfo, error) { return w.list, nil }
func (w *fakeWorkspaces) Remove(info source.WorkspaceInfo) error {
	w.removed = append(w.removed, info.Path)
	return nil
}

func TestPruneKeepsCachesWithinBudgetAndNeverRemovesInUse(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	day := 24 * time.Hour
	caches := &fakeCaches{caches: []builds.Cache{
		{Name: "revoked", BaseRepoID: 555, Size: 1 << 30, CreatedAt: now.Add(-5 * day)},
		{Name: "oldest", BaseRepoID: 100, Size: 4 << 30, CreatedAt: now.Add(-4 * day)},
		{Name: "busy", BaseRepoID: 100, Size: 4 << 30, CreatedAt: now.Add(-3 * day), InUse: true},
		{Name: "newer", BaseRepoID: 100, Size: 4 << 30, CreatedAt: now.Add(-2 * day)},
		{Name: "newest", BaseRepoID: 100, Size: 2 << 30, CreatedAt: now.Add(-day)},
	}, dangling: []string{"sha256:dangling"}}
	workspaces := &fakeWorkspaces{list: []source.WorkspaceInfo{
		{Path: "/w/old", CreatedAt: now.Add(-2 * time.Hour)},
		{Path: "/w/running", CreatedAt: now.Add(-time.Minute)},
	}}
	settings := f.settings // 10 GiB budget
	settings.DryRun = true
	dry, err := NewPrune(caches, workspaces, f.pool, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	dry.now = func() time.Time { return now }
	if pass, err := dry.RunOnce(t.Context(), false); err != nil || pass.Caches != 2 || pass.Images != 1 || pass.Workspaces != 1 || len(caches.removed)+len(workspaces.removed) != 0 {
		t.Fatalf("dry run %+v %v", pass, err)
	}
	settings.DryRun = false
	prune, err := NewPrune(caches, workspaces, f.pool, f.logger, settings)
	if err != nil {
		t.Fatal(err)
	}
	prune.now = func() time.Time { return now }
	pass, err := prune.RunOnce(t.Context(), false)
	// 15 GiB total: the revoked repository's cache goes, then the oldest unused
	// one until 10 GiB fit; the busy one is never touched.
	if err != nil || pass.Caches != 2 || pass.CacheBytes != 5<<30 {
		t.Fatalf("prune %+v %v", pass, err)
	}
	if !equal(caches.removed, []string{"sha256:dangling", "revoked", "oldest"}) || !equal(workspaces.removed, []string{"/w/old"}) {
		t.Fatalf("removed %v %v", caches.removed, workspaces.removed)
	}
	// Under disk pressure the budget halves to 5 GiB: the unused caches go,
	// oldest first, and the busy one stays even though it alone is 4 GiB.
	if pass, err := prune.RunOnce(t.Context(), true); err != nil || pass.Caches != 2 {
		t.Fatalf("emergency %+v %v", pass, err)
	}
	if len(caches.caches) != 1 || caches.caches[0].Name != "busy" {
		t.Fatalf("left %+v", caches.caches)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHousekeeperPrunesWhenABuildWaitsForDiskAndRegistersItsJobs(t *testing.T) {
	f := newFixture(t)
	waiting := f.open(t, 1, "sha-wait")
	caches := &fakeCaches{caches: []builds.Cache{{Name: "big", BaseRepoID: 100, Size: 8 << 30, CreatedAt: time.Now().Add(-time.Hour)}}}
	prune, err := NewPrune(caches, &fakeWorkspaces{}, f.pool, f.logger, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHousekeeper(f.pool, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	h.Recovery, h.Prune = f.recovery(t), prune
	if err := h.Recover(t.Context()); err != nil || len(caches.removed) != 0 {
		t.Fatalf("pruned without disk pressure: %v %v", caches.removed, err)
	}
	f.exec(t, "UPDATE build_intents SET waiting_revision=revision, waiting_reason='disk' WHERE pull_request_id=$1", waiting.PullRequestID)
	if err := h.Recover(t.Context()); err != nil || !equal(caches.removed, []string{"big"}) {
		t.Fatalf("no emergency prune: %v %v", caches.removed, err)
	}
	caches.caches = append(caches.caches, builds.Cache{Name: "again", BaseRepoID: 100, Size: 8 << 30})
	if err := h.Recover(t.Context()); err != nil || len(caches.removed) != 1 {
		t.Fatalf("emergency prunes are not rate limited: %v", caches.removed)
	}

	h.Resources, h.Expiry, h.Records = f.resources(t, false, time.Now()), &Expiry{}, &Records{}
	cfg := &river.Config{}
	h.Register(cfg)
	if cfg.Queues[Queue].MaxWorkers != 1 || len(cfg.PeriodicJobs) != 5 || cfg.Workers == nil {
		t.Fatalf("registration: %+v %d periodic jobs", cfg.Queues, len(cfg.PeriodicJobs))
	}
	if insertOpts().MaxAttempts != 1 || !insertOpts().UniqueOpts.ByArgs {
		t.Fatal("housekeeping jobs must not pile up or retry")
	}
}

func TestAVanishedActivePreviewIsRecordedAsFailed(t *testing.T) {
	f := newFixture(t)
	gone := f.open(t, 1, "sha-gone")
	f.publish(t, gone, time.Hour) // the database says it runs; Docker has no such container
	present := f.open(t, 2, "sha-present")
	f.publish(t, present, time.Hour)
	f.inventory.add(environments.Resource{PullRequestID: present.PullRequestID, Generation: 1, Revision: 1,
		Artifact: environments.Artifact{ContainerID: "container-" + strconv.FormatInt(present.PullRequestID, 10)}, CreatedAt: time.Now().Add(-time.Hour)})

	pass, err := f.resources(t, true, time.Now()).RunOnce(t.Context())
	if err != nil || len(pass.Missing) != 1 || pass.Missing[0] != gone.PullRequestID {
		t.Fatalf("dry run %+v %v", pass, err)
	}
	if n := f.count(t, "SELECT count(*) FROM environments WHERE status='running'"); n != 2 {
		t.Fatal("a dry run changed an environment")
	}
	if _, err := f.resources(t, false, time.Now()).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status, code string
	var container *string
	f.scan(t, "SELECT status, last_error_code, container_id FROM environments WHERE pull_request_id=$1", []any{&status, &code, &container}, gone.PullRequestID)
	if status != "failed" || code != "preview_missing" || container != nil {
		t.Fatalf("vanished preview recorded as %s/%s/%v", status, code, container)
	}
	if n := f.count(t, "SELECT count(*) FROM environments WHERE pull_request_id=$1 AND status='running'", present.PullRequestID); n != 1 {
		t.Fatal("a preview whose container exists was marked missing")
	}
	if n := f.count(t, "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND pull_request_id=$1", gone.PullRequestID); n == 0 {
		t.Fatal("the vanished preview was not reported")
	}
}
