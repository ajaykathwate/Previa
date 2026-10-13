package routing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Real Traefik calls this fixture for every private request. Cookie signing and
// GitHub permissions are independently covered by previewaccess's round trip.
const authFixture = `set -eu
mkdir -p /www/cgi-bin
cat > /www/cgi-bin/auth <<'AUTH'
#!/bin/sh
if [ "$HTTP_COOKIE" = 'fixture-access=allowed' ]; then
  printf 'Status: 200 OK\r\nContent-Type: text/plain\r\n\r\nallowed'
else
  printf 'Status: 403 Forbidden\r\nContent-Type: text/plain\r\n\r\ndenied'
fi
AUTH
chmod +x /www/cgi-bin/auth
exec httpd -f -p 8080 -h /www
`

const authFixtureImage = "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

func TestRealPrivateRoutingRecoversConfigurationAndEnforcesAuth(t *testing.T) {
	h := realHarness(t)
	ctx := ctxFor(t)
	number := numbers.Add(1)
	old := h.event(t, number, "opened", sha(90))
	h.build(t, old)
	h.drain(t)
	host := h.host(t, old.PullRequestID)
	h.serves(t, host, marker(old.PullRequestID, old.Generation, old.Revision))
	if _, err := h.pool.Exec(ctx, "UPDATE repos SET private_previews=true WHERE id=$1", h.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, "UPDATE route_table SET version=version+1 WHERE singleton"); err != nil {
		t.Fatal(err)
	}
	if err := h.controller.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := h.pool.Exec(cleanup, "UPDATE repos SET private_previews=false WHERE id=$1", h.repoID); err != nil {
			t.Error(err)
		}
		if _, err := h.pool.Exec(cleanup, "UPDATE route_table SET version=version+1 WHERE singleton"); err != nil {
			t.Error(err)
		}
		if err := h.controller.Sync(cleanup); err != nil {
			t.Error(err)
		}
	})
	a := h.event(t, number, "synchronize", sha(91))
	h.build(t, a)
	op := h.operation(t, env.ActivateRoute, a.PullRequestID, a.Generation)
	var failure *env.OperationFailure
	if err := h.controller.Execute(ctx, op); !errors.As(err, &failure) || failure.Code != "private_preview_unconfigured" || !failure.Permanent {
		t.Fatalf("missing authentication must fail explicitly: %v", err)
	}
	state, ok, err := h.repo.RouteState(ctx, a.PullRequestID)
	if err != nil || !ok || state.Generation != old.Generation || state.Revision != old.Revision {
		t.Fatalf("unconfigured candidate changed the accepted route: %+v %v", state, err)
	}
	h.unavailable(t, host)
	before, err := h.file.Version()
	if err != nil {
		t.Fatal(err)
	}
	authName := "previa-rt-auth-" + runSuffix
	t.Cleanup(func() {
		_, _ = h.docker.ContainerRemove(context.Background(), authName, client.ContainerRemoveOptions{Force: true})
	})
	if err := ensureImage(ctx, h.docker, authFixtureImage); err != nil {
		t.Fatal(err)
	}
	if err := h.run(ctx, authName, "auth", &container.Config{Image: authFixtureImage, Cmd: []string{"sh", "-c", authFixture}}, &container.HostConfig{}); err != nil {
		t.Fatal(err)
	}
	c, err := routing.NewController(h.repo, h.backends(), h.traefik, h.file, routing.Settings{
		BaseDomain: baseDomain, EntryPoint: "websecure", UnavailableURL: unavailableURL,
		WakeURL:   strings.TrimSuffix(unavailableURL, routing.UnavailablePath) + routing.WakePath,
		AccessURL: "http://auth:8080/cgi-bin/auth",
	}, h.logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if after, err := h.file.Version(); err != nil || after != before {
		t.Fatalf("auth recovery changed the route version: %d -> %d %v", before, after, err)
	}
	h.eventually(t, "previous private route restored at the same version", func() error {
		r, err := h.getWithCookie(host, "/", "fixture-access=allowed")
		if err != nil {
			return err
		}
		if r.status != http.StatusOK || r.body != marker(old.PullRequestID, old.Generation, old.Revision) {
			return fmt.Errorf("recovered private route: %+v", r)
		}
		return nil
	})
	worker, err := h.newWorker(env.Executors{env.ActivateRoute: c, env.RemoveRoute: c, env.CleanupArtifact: h.runtime.CleanupExecutor(), env.ReportStatus: h.reports})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.process(t, worker, op.ID); err != nil {
		t.Fatal(err)
	}
	if s := h.snapshot(t, a.PullRequestID); s.Status != "running" {
		t.Fatalf("configured private preview not published: %+v", s)
	}
	h.eventually(t, "anonymous private request refused", func() error {
		r, err := h.get(host, "/")
		if err != nil {
			return err
		}
		if r.status != http.StatusForbidden || r.body != "denied" {
			return fmt.Errorf("anonymous private request: %+v", r)
		}
		return nil
	})
	h.eventually(t, "authorized private request served", func() error {
		r, err := h.getWithCookie(host, "/", "fixture-access=allowed")
		if err != nil {
			return err
		}
		if r.status != http.StatusOK || r.body != marker(a.PullRequestID, a.Generation, a.Revision) {
			return fmt.Errorf("authorized private request: %+v", r)
		}
		return nil
	})
}
