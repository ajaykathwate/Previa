package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/moby/moby/client"
)

// startService runs one managed service on network, as a candidate's
// companion would, and returns its container and plan.
func startService(t *testing.T, ctx context.Context, f *realFixture, r *Runtime, kind, version, network string, logs LogSink) (string, serviceRun) {
	t.Helper()
	catalog, err := config.ParseServiceCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	image, ok := catalog.Image(kind, version)
	if !ok {
		t.Fatalf("the default catalog lacks %s %s", kind, version)
	}
	settings := repoconfig.Config{Services: []repoconfig.BackendService{{Kind: kind, Version: version, Image: image}}}
	plan, err := planBackend(fmt.Sprintf("data-test-%d", time.Now().UnixNano()), settings)
	if err != nil {
		t.Fatal(err)
	}
	s := plan.services[0]
	volumes, err := r.serviceImages(ctx, plan.services, logs)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{dockerx.LabelManaged: "true", dockerx.LabelInstance: f.instance, dockerx.LabelKind: dockerx.KindService}
	cfg, host, networking := serviceSpec(s, volumes[kind], network, labels, r.policy)
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
	if err := r.waitServicesReady(ctx, plan.services, map[string]string{kind: created.ID}, logs); err != nil {
		t.Fatalf("%s %s did not start: %v", kind, version, err)
	}
	return created.ID, s
}

// sql runs statements as the service's application user and returns the
// output rows.
func sql(t *testing.T, ctx context.Context, f *realFixture, id string, s serviceRun, statements string) string {
	t.Helper()
	var result dockerx.ExecResult
	var err error
	if s.Kind == "mysql" {
		result, err = dockerx.RunWithEnv(ctx, f.docker, id, []string{"MYSQL_PWD=" + s.password}, "mysql", "--protocol=tcp", "-h127.0.0.1", "-upreview", "-Dpreview", "-NBe", statements)
	} else {
		result, err = dockerx.RunWithEnv(ctx, f.docker, id, []string{"PGPASSWORD=" + s.password}, "psql", "-h", "127.0.0.1", "-U", "preview", "-d", "preview", "-tAc", statements)
	}
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("%s: %+v %v", statements, result, err)
	}
	return result.Stdout
}

// TestRealRuntimeCopiesTestData proves the copy mechanics of persistent test
// data on real PostgreSQL and MySQL images under the services' own
// restrictions: a dump streamed straight from one candidate's database into
// another's (also from PostgreSQL 16 into 17), a dump captured and restored
// later (a baseline), a restart that empties a database for a fallback, and
// refused input reported as a restore failure.
func TestRealRuntimeCopiesTestData(t *testing.T) {
	f := newRealFixture(t)
	for _, tc := range []struct{ kind, from, to string }{{"postgres", "16", "17"}, {"mysql", "8.4", "8.4"}} {
		t.Run(tc.kind, func(t *testing.T) {
			target := f.target(41, false)
			if tc.kind == "mysql" {
				target = f.target(42, false)
			}
			r, logs := f.runtime(t, target, repoconfig.Config{}, func(p *Policy) { p.StartupTimeout = 3 * time.Minute })
			ctx := buildCtx(t)
			id := requestIdentity{PullRequestID: target.PullRequestID, Generation: 1, Revision: 1, HeadSHA: request(41).HeadSHA}
			netID, err := r.servicesNetwork(ctx, id, target)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_, _ = f.docker.NetworkRemove(cleanupCtx, netID, client.NetworkRemoveOptions{})
			})
			serving, servingRun := startService(t, ctx, f, r, tc.kind, tc.from, netID, logs)
			next, nextRun := startService(t, ctx, f, r, tc.kind, tc.to, netID, logs)
			sql(t, ctx, f, serving, servingRun, "CREATE TABLE orders (id INT PRIMARY KEY, customer VARCHAR(40) NOT NULL); "+
				"INSERT INTO orders VALUES (1, 'Preview customer A'), (2, 'Preview customer B'), (3, 'Added by a tester'); "+
				"CREATE VIEW big_orders AS SELECT customer FROM orders WHERE id > 1;")

			// Kept data: straight from the serving database into the next one.
			copied, problem := r.copyService(ctx, serving, next, tc.kind)
			if problem != "" || copied == 0 {
				t.Fatalf("copy: %q after %d bytes", problem, copied)
			}
			if got := sql(t, ctx, f, next, nextRun, "SELECT count(*) FROM orders; SELECT customer FROM big_orders ORDER BY customer;"); !strings.Contains(got, "3") ||
				!strings.Contains(got, "Added by a tester") || !strings.Contains(got, "Preview customer B") {
				t.Fatalf("copied data %q", got)
			}

			// A fallback restarts the database: it starts empty with the same
			// credentials.
			l := &launch{r: r, plan: backend{services: []serviceRun{nextRun}}, created: map[string]string{tc.kind: next}, logs: logs}
			if err := l.restartServices(ctx); err != nil {
				t.Fatal(err)
			}
			if got := sql(t, ctx, f, next, nextRun, tablesQuery(tc.kind)); strings.TrimSpace(got) != "0" {
				t.Fatalf("after a restart the database holds %q tables", got)
			}

			// A baseline: a dump captured now and restored later.
			out, err := dockerx.StreamOutput(ctx, f.docker, serving, dumpCommand(tc.kind)...)
			if err != nil {
				t.Fatal(err)
			}
			var dump bytes.Buffer
			if _, err := io.Copy(&dump, out); err != nil {
				t.Fatal(err)
			}
			if result, err := out.Wait(ctx); err != nil || result.ExitCode != 0 {
				t.Fatalf("capture: %+v %v", result, err)
			}
			restored, err := dockerx.FeedInput(ctx, f.docker, next, bytes.NewReader(dump.Bytes()), restoreCommand(tc.kind)...)
			if err != nil || restored.ExitCode != 0 {
				t.Fatalf("restore: %v (exit %d)", err, restored.ExitCode)
			}
			if got := sql(t, ctx, f, next, nextRun, "SELECT count(*) FROM orders;"); strings.TrimSpace(got) != "3" {
				t.Fatalf("restored %q orders", got)
			}

			// Input that is no dump fails the restore without showing output.
			if err := l.restartServices(ctx); err != nil {
				t.Fatal(err)
			}
			refused, _ := dockerx.FeedInput(ctx, f.docker, next, strings.NewReader("this is not a database dump\x00\x01"), restoreCommand(tc.kind)...)
			if refused.ExitCode == 0 || restoreProblem(refused) != "restore_failed" {
				t.Fatalf("garbage restored: %+v", refused)
			}
		})
	}
}

func tablesQuery(kind string) string {
	if kind == "mysql" {
		return "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'preview';"
	}
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public';"
}
