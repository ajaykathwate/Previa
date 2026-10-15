package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/moby/moby/client"
)

// Exercise the official image under the actual service restrictions, including
// authenticated readiness, fresh data, isolation and candidate cleanup.
func TestRealRuntimeManagedMySQL(t *testing.T) {
	f := newRealFixture(t)
	catalog, err := config.ParseServiceCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	image, ok := catalog.Image("mysql", "8.4")
	if !ok {
		t.Fatal("default catalog must offer MySQL")
	}
	settings := repoconfig.Config{Services: []repoconfig.BackendService{{Kind: "mysql", Version: "8.4", Image: image}}}
	target := f.target(31, false)
	r, logs := f.runtime(t, target, settings, func(p *Policy) { p.StartupTimeout = 3 * time.Minute })
	id := requestIdentity{PullRequestID: target.PullRequestID, Generation: 1, Revision: 1, HeadSHA: request(31).HeadSHA}
	ctx := buildCtx(t)
	netID, err := r.servicesNetwork(ctx, id, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = f.docker.NetworkRemove(cleanupCtx, netID, client.NetworkRemoveOptions{})
	})
	for attempt := range 2 {
		plan, err := planBackend(fmt.Sprintf("mysql-test-%d", attempt), settings)
		if err != nil {
			t.Fatal(err)
		}
		s := plan.services[0]
		volumes, err := r.serviceImages(ctx, plan.services, logs)
		if err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{dockerx.LabelManaged: "true", dockerx.LabelInstance: f.instance, dockerx.LabelKind: dockerx.KindService}
		cfg, host, networking := serviceSpec(s, volumes[s.Kind], netID, labels, r.policy)
		created, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: host, NetworkingConfig: networking})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		})
		if _, err := f.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.waitServicesReady(ctx, plan.services, map[string]string{"mysql": created.ID}, logs); err != nil {
			output, _ := f.docker.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "30"})
			if output != nil {
				defer func() { _ = output.Close() }()
			}
			t.Fatalf("MySQL startup: %v (inspect container logs for %s)", err, created.ID)
		}
		if ready, _, err := r.serviceAnswers(ctx, s, created.ID, false); err != nil || !ready {
			t.Fatalf("unauthenticated health: %v %v", ready, err)
		}
		wrong := s
		wrong.password = "wrong-password"
		if ready, _, err := r.serviceAnswers(ctx, wrong, created.ID, true); err != nil || ready {
			t.Fatalf("wrong password must be refused: %v %v", ready, err)
		}
		result, err := dockerx.RunWithEnv(ctx, f.docker, created.ID, []string{"MYSQL_PWD=" + s.password}, "mysql", "--protocol=tcp", "-h127.0.0.1", "-upreview", "-Dpreview", "-NBe",
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='preview'; CREATE TABLE sample (id INT); INSERT INTO sample VALUES (42); SELECT id FROM sample;")
		if err != nil || result.ExitCode != 0 || result.Stdout != "0\n42" {
			t.Fatalf("fresh database and writes: %+v %v", result, err)
		}
		inspected, err := f.docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
		if err != nil || !inspected.Container.HostConfig.ReadonlyRootfs || len(inspected.Container.NetworkSettings.Networks) != 1 || len(inspected.Container.HostConfig.Tmpfs) != len(host.Tmpfs) {
			t.Fatalf("MySQL isolation: %v", err)
		}
		for _, mount := range inspected.Container.Mounts {
			if mount.Type != "tmpfs" {
				t.Fatal("MySQL must not create persistent volumes")
			}
		}
		if err := r.RemoveContainer(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
	}
}
