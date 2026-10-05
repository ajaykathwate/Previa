package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/runtime"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const realImage = "alpine:3.22"

type realHarness struct {
	*fixture
	docker    *client.Client
	instance  string
	inventory *runtime.Runtime
}

func newRealHarness(t *testing.T) *realHarness {
	t.Helper()
	if os.Getenv("TEST_HOUSEKEEPING") != "1" {
		t.Skip("set TEST_HOUSEKEEPING=1 (make test-housekeeping) to use the local Docker Engine")
	}
	f := newFixture(t)
	docker, err := dockerx.New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	if _, err := docker.ImageInspect(t.Context(), realImage); cerrdefs.IsNotFound(err) {
		pulled, err := docker.ImagePull(t.Context(), realImage, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := pulled.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	instance, err := database.Instance(t.Context(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := runtime.NewInventory(docker, instance, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	return &realHarness{fixture: f, docker: docker, instance: instance, inventory: inventory}
}

// container starts a sleeping container with Previa's labels for pr.
func (h *realHarness) container(t *testing.T, instance string, pr, generation int64, mounts ...mount.Mount) string {
	t.Helper()
	created, err := h.docker.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{Image: realImage, Cmd: []string{"sleep", "600"}, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: instance, dockerx.LabelKind: dockerx.KindPreview,
			dockerx.LabelPR: strconv.FormatInt(pr, 10), dockerx.LabelGeneration: strconv.FormatInt(generation, 10), dockerx.LabelRevision: "1",
		}},
		HostConfig: &container.HostConfig{Mounts: mounts},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := h.docker.ContainerStart(t.Context(), created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func (h *realHarness) exists(t *testing.T, id string) bool {
	t.Helper()
	_, err := h.docker.ContainerInspect(t.Context(), id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// drainCleanup runs every due cleanup operation with the runtime's executor.
func (h *realHarness) drainCleanup(t *testing.T) {
	t.Helper()
	worker, err := environments.NewOperationWorker(h.repo, h.inventory.CleanupExecutor(), h.logger, time.Minute,
		map[environments.OperationKind]time.Duration{environments.CleanupArtifact: 45 * time.Second, environments.ActivateRoute: time.Second,
			environments.RemoveRoute: time.Second, environments.ReportStatus: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		var id int64
		err := h.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='cleanup_artifact' AND state IN ('pending','retryable') ORDER BY id LIMIT 1").Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.Process(t.Context(), id, fmt.Sprintf("real-%d", id), 1, 5); err != nil {
			t.Fatalf("cleanup operation %d: %v", id, err)
		}
	}
	t.Fatal("cleanup did not drain")
}

func TestRealHousekeepingRemovesOnlyThisInstancesOrphans(t *testing.T) {
	h := newRealHarness(t)
	pr := h.open(t, 1, "sha-live")
	h.publish(t, pr, time.Hour)
	serving := h.container(t, h.instance, pr.PullRequestID, 1)
	h.exec(t, "UPDATE environments SET container_id=$2, image_tag=NULL WHERE pull_request_id=$1", pr.PullRequestID, serving)
	orphan := h.container(t, h.instance, pr.PullRequestID, 0)          // an abandoned earlier candidate
	neighbour := h.container(t, "zz"+h.instance, pr.PullRequestID, 0)  // another instance, same PR ID
	unknown := h.container(t, h.instance, pr.PullRequestID+100_000, 1) // a PR this database does not have
	resources := func(dryRun bool, at time.Time) *Resources {
		settings := h.settings
		settings.DryRun = dryRun
		r, err := NewResources(h.repo, h.inventory, h.pool, h.logger, settings)
		if err != nil {
			t.Fatal(err)
		}
		r.now = func() time.Time { return at }
		return r
	}

	// Everything was created moments ago: within the grace period, nothing goes.
	if pass, err := resources(false, time.Now()).RunOnce(t.Context()); err != nil || len(pass.Orphans) != 0 || len(pass.Removed) != 0 {
		t.Fatalf("young objects were treated as orphans: %+v %v", pass, err)
	}
	later := time.Now().Add(time.Hour)
	pass, err := resources(true, later).RunOnce(t.Context())
	if err != nil || len(pass.Orphans) != 1 || pass.Orphans[0].Artifact.ContainerID != orphan || len(pass.Removed) != 1 || pass.Removed[0].Artifact.ContainerID != unknown {
		t.Fatalf("dry run %+v %v", pass, err)
	}
	for _, id := range []string{serving, orphan, neighbour, unknown} {
		if !h.exists(t, id) {
			t.Fatal("a dry run removed a container")
		}
	}
	if _, err := resources(false, later).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.drainCleanup(t)
	if h.exists(t, orphan) || h.exists(t, unknown) {
		t.Fatal("orphans survived housekeeping")
	}
	if !h.exists(t, serving) || !h.exists(t, neighbour) {
		t.Fatal("housekeeping removed the serving preview or another instance's container")
	}
}

func TestRealHousekeepingPrunesUnderDiskPressure(t *testing.T) {
	h := newRealHarness(t)
	settings, err := config.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := egress.NewGateway(h.docker, settings.EgressImage)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := builds.NewBuilder(h.docker, gateway, builds.ConfigFrom(settings.BuilderConfig, h.instance, 10*time.Minute), h.logger)
	if err != nil {
		t.Fatal(err)
	}
	volume := func(name string, baseRepo int64) string {
		full := builds.CacheVolumeName(h.instance, name)
		if _, err := h.docker.VolumeCreate(t.Context(), client.VolumeCreateOptions{Name: full, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: h.instance, dockerx.LabelKind: dockerx.KindBuildCache,
			dockerx.LabelBaseRepo: strconv.FormatInt(baseRepo, 10),
		}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = h.docker.VolumeRemove(context.Background(), full, client.VolumeRemoveOptions{Force: true})
		})
		return full
	}
	revoked := volume("r555-h555", 555) // a repository this instance can no longer build
	busy := volume("r556-h556", 556)    // the same, but a builder holds it
	current := volume("r100-h100", 100) // the fixture's linked repository
	h.container(t, h.instance, 999_999, 1, mount.Mount{Type: mount.TypeVolume, Source: busy, Target: "/cache"})
	// An image that lost its tag to a later attempt of the same revision.
	scratch, err := h.docker.ContainerCreate(t.Context(), client.ContainerCreateOptions{Config: &container.Config{Image: realImage}})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := h.docker.ContainerCommit(t.Context(), scratch.ID, client.ContainerCommitOptions{Changes: []string{
		"LABEL " + dockerx.LabelManaged + "=true", "LABEL " + dockerx.LabelInstance + "=" + h.instance, "LABEL " + dockerx.LabelKind + "=" + dockerx.KindImage,
	}})
	_, _ = h.docker.ContainerRemove(context.Background(), scratch.ID, client.ContainerRemoveOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.docker.ImageRemove(context.Background(), committed.ID, client.ImageRemoveOptions{Force: true})
	})

	prune, err := NewPrune(builder, nil, h.pool, h.logger, h.settings)
	if err != nil {
		t.Fatal(err)
	}
	housekeeper, err := NewHousekeeper(h.pool, h.logger)
	if err != nil {
		t.Fatal(err)
	}
	housekeeper.Recovery, housekeeper.Prune = h.recovery(t), prune
	// No build waits for disk: the minute-by-minute pass prunes nothing.
	if err := housekeeper.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	volumeExists := func(name string) bool {
		_, err := h.docker.VolumeInspect(t.Context(), name, client.VolumeInspectOptions{})
		return err == nil
	}
	if !volumeExists(revoked) {
		t.Fatal("pruned without disk pressure")
	}
	// A build waits for disk space: the next pass prunes at once.
	waiting := h.open(t, 2, "sha-wait")
	h.exec(t, "UPDATE build_intents SET waiting_revision=revision, waiting_reason='disk' WHERE pull_request_id=$1", waiting.PullRequestID)
	if err := housekeeper.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if volumeExists(revoked) {
		t.Fatal("the unavailable repository's cache survived an emergency prune")
	}
	if !volumeExists(busy) || !volumeExists(current) {
		t.Fatal("an emergency prune removed a cache in use or within budget")
	}
	if _, err := h.docker.ImageInspect(t.Context(), committed.ID); !cerrdefs.IsNotFound(err) {
		t.Fatalf("the untagged image survived: %v", err)
	}
}
