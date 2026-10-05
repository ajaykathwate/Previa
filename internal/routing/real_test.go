package routing_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

var numbers atomic.Int32

func sha(n int64) string { return fmt.Sprintf("%040x", run+n) }

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// event records a PR webhook and makes the resulting Automatic intent due now.
func (h *harness) event(t *testing.T, number int32, action, head string) env.Request {
	t.Helper()
	ctx := ctxFor(t)
	mergeable := true
	request, err := h.events.ProcessPR(ctx, webhooks.PREvent{RepoID: h.repoID, Number: number, HeadSHA: head, Action: action, Mergeable: &mergeable, MergeableState: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, "UPDATE build_intents SET available_at=now()-interval '1 second' WHERE pull_request_id=$1", request.PullRequestID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.prs, request.PullRequestID) {
		h.prs = append(h.prs, request.PullRequestID)
	}
	return request
}

func (h *harness) build(t *testing.T, request env.Request) {
	t.Helper()
	if err := h.service.Build(ctxFor(t), request); err != nil {
		t.Fatalf("build %+v: %v", request, err)
	}
}

func (h *harness) process(t *testing.T, worker *env.OperationWorker, id int64) error {
	t.Helper()
	return worker.Process(ctxFor(t), id, fmt.Sprintf("real-%d-%d", id, time.Now().UnixNano()), 1, 5)
}

// drain runs every due operation, oldest first, until none is due.
func (h *harness) drain(t *testing.T) {
	t.Helper()
	for range 300 {
		var id int64
		err := h.pool.QueryRow(ctxFor(t), "SELECT id FROM environment_operations WHERE state IN ('pending','retryable') AND available_at <= now() ORDER BY id LIMIT 1").Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = h.process(t, h.worker, id)
	}
	t.Fatal("operations did not drain")
}

func (h *harness) operation(t *testing.T, kind env.OperationKind, pr, generation int64) env.Operation {
	t.Helper()
	var op env.Operation
	var raw []byte
	var state string
	if err := h.pool.QueryRow(ctxFor(t), "SELECT id, pull_request_id, generation, revision, payload, state FROM environment_operations WHERE kind=$1 AND pull_request_id=$2 AND generation=$3 ORDER BY id DESC LIMIT 1", string(kind), pr, generation).
		Scan(&op.ID, &op.PullRequestID, &op.Generation, &op.Revision, &raw, &state); err != nil {
		t.Fatalf("load %s of PR %d generation %d: %v", kind, pr, generation, err)
	}
	if err := json.Unmarshal(raw, &op.Payload); err != nil {
		t.Fatal(err)
	}
	op.Kind, op.State = kind, env.OperationState(state)
	return op
}

func (h *harness) state(t *testing.T, id int64) string {
	t.Helper()
	var state string
	if err := h.pool.QueryRow(ctxFor(t), "SELECT state FROM environment_operations WHERE id=$1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (h *harness) snapshot(t *testing.T, pr int64) env.Snapshot {
	t.Helper()
	snapshot, err := h.repo.Snapshot(ctxFor(t), pr)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (h *harness) host(t *testing.T, pr int64) string {
	t.Helper()
	var label string
	if err := h.pool.QueryRow(ctxFor(t), "SELECT subdomain FROM environments WHERE pull_request_id=$1", pr).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if !routing.ValidLabel(label) || !strings.HasPrefix(label, "pr-") {
		t.Fatalf("hostname label %q", label)
	}
	return routing.Host(label, baseDomain)
}

type response struct {
	status   int
	body     string
	location string
	cookies  int
}

// get requests https://host/path through the router's published port, with
// the certificate verified against Pebble's root.
func (h *harness) get(host, path string) (response, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", h.httpsAddr)
		},
		TLSClientConfig:   tlsConfig(h.roots, host),
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := httpClient.Get("https://" + host + path)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	return response{status: r.StatusCode, body: strings.TrimSpace(string(body)), location: r.Header.Get("Location"), cookies: len(r.Header.Values("Set-Cookie"))}, err
}

func (h *harness) eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

func (h *harness) serves(t *testing.T, host, want string) {
	t.Helper()
	h.eventually(t, "serve "+want, func() error {
		r, err := h.get(host, "/")
		if err != nil {
			return err
		}
		if r.status != http.StatusOK || r.body != want {
			return fmt.Errorf("got %d %q", r.status, r.body)
		}
		return nil
	})
}

// servesNow checks one response without waiting.
func (h *harness) servesNow(t *testing.T, host, want string) {
	t.Helper()
	r, err := h.get(host, "/")
	if err != nil || r.status != http.StatusOK || r.body != want {
		t.Fatalf("expected %q: %+v %v", want, r, err)
	}
}

func (h *harness) unavailable(t *testing.T, host string) {
	t.Helper()
	h.eventually(t, "redirect "+host+" to the unavailable page", func() error {
		r, err := h.get(host, "/anything?x=1")
		if err != nil {
			return err
		}
		if r.status != http.StatusFound || r.location != unavailableURL || r.cookies != 0 {
			return fmt.Errorf("got %+v", r)
		}
		return nil
	})
}

func (h *harness) gone(t *testing.T, containerID, image string) {
	t.Helper()
	ctx := ctxFor(t)
	if containerID != "" {
		if _, err := h.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
			t.Fatalf("container %s still exists (%v)", containerID, err)
		}
	}
	if image != "" {
		if _, err := h.docker.ImageInspect(ctx, image); !cerrdefs.IsNotFound(err) {
			t.Fatalf("image %s still exists (%v)", image, err)
		}
	}
}

// noLeftovers asserts Docker holds nothing for the PR and the router left its
// network.
func (h *harness) noLeftovers(t *testing.T, pr int64) {
	t.Helper()
	ctx := ctxFor(t)
	resources, err := h.runtime.Resources(ctx, pr)
	if err != nil || len(resources) != 0 {
		t.Fatalf("leftovers for PR %d: %+v %v", pr, resources, err)
	}
	filter := client.Filters{}.Add("label", dockerx.LabelPR+"="+fmt.Sprint(pr))
	images, err := h.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil || len(images.Items) != 0 {
		t.Fatalf("images left for PR %d: %d %v", pr, len(images.Items), err)
	}
	networks, err := h.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil || len(networks.Items) != 0 {
		t.Fatalf("networks left for PR %d: %d %v", pr, len(networks.Items), err)
	}
	router, err := h.docker.ContainerInspect(ctx, h.routerName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name := range router.Container.NetworkSettings.Networks {
		if name == fmt.Sprintf("previa-pr-%d", pr) {
			t.Fatalf("router still attached to %s", name)
		}
	}
}

// closePR closes a PR and starts its teardown through the close path.
func (h *harness) closePR(t *testing.T, number int32, head string) env.Request {
	t.Helper()
	closed := h.event(t, number, "closed", head)
	if err := h.service.Teardown(ctxFor(t), closed); err != nil {
		t.Fatal(err)
	}
	return closed
}

func TestRealRoutingServesStableHTTPSAndKeepsThePreviewThroughAFailedSwap(t *testing.T) {
	h := realHarness(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(1))
	h.build(t, a)
	h.drain(t)
	host := h.host(t, a.PullRequestID)
	h.serves(t, host, marker(a.PullRequestID, a.Generation, a.Revision))
	if s := h.snapshot(t, a.PullRequestID); s.Status != "running" {
		t.Fatalf("environment %+v", s)
	}
	cert, err := h.certificate(host)
	if err != nil || !slices.Contains(cert.DNSNames, "*."+baseDomain) {
		t.Fatalf("certificate %v %v", cert.DNSNames, err)
	}
	t.Logf("HTTPS: https://%s served by a Pebble-issued wildcard certificate %v", host, cert.DNSNames)
	h.unavailable(t, "nobody-here."+baseDomain)
	// Plain HTTP is redirected to HTTPS for the same host.
	request, _ := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/x", nil)
	request.Host = host
	plain, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = plain.Body.Close()
	if plain.StatusCode != http.StatusMovedPermanently && plain.StatusCode != http.StatusPermanentRedirect || plain.Header.Get("Location") != "https://"+host+"/x" {
		t.Fatalf("HTTP redirect %d %q", plain.StatusCode, plain.Header.Get("Location"))
	}
	first := h.snapshot(t, a.PullRequestID).Artifact

	// A newer commit swaps the preview behind the same hostname.
	b := h.event(t, number, "synchronize", sha(2))
	h.build(t, b)
	h.drain(t)
	h.serves(t, host, marker(b.PullRequestID, b.Generation, b.Revision))
	if again := h.host(t, a.PullRequestID); again != host {
		t.Fatalf("hostname changed from %s to %s", host, again)
	}
	h.drain(t)
	h.gone(t, first.ContainerID, first.ImageTag)
	serving := h.snapshot(t, a.PullRequestID).Artifact

	// A candidate that stops before publication never takes over.
	c := h.event(t, number, "synchronize", sha(3))
	h.build(t, c)
	candidate := h.snapshot(t, a.PullRequestID).PendingArtifact
	if _, err := h.docker.ContainerStop(ctxFor(t), candidate.ContainerID, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	s := h.snapshot(t, a.PullRequestID)
	if s.Status != "failed" || s.Artifact != serving || s.AppliedGeneration != c.Generation || s.PendingArtifact.ContainerID != "" {
		t.Fatalf("failed swap: %+v", s)
	}
	h.servesNow(t, host, marker(b.PullRequestID, b.Generation, b.Revision))
	h.gone(t, candidate.ContainerID, candidate.ImageTag)
	h.reports.mu.Lock()
	reported := slices.Contains(h.reports.codes, "crashed")
	h.reports.mu.Unlock()
	if !reported {
		t.Fatal("the failed swap was not reported")
	}

	h.closePR(t, number, sha(3))
	h.drain(t)
	h.unavailable(t, host)
	h.noLeftovers(t, a.PullRequestID)
}

func TestRealRoutingFencesDelayedStaleOperations(t *testing.T) {
	h := realHarness(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(10))
	h.build(t, a)
	opA := h.operation(t, env.ActivateRoute, a.PullRequestID, a.Generation)
	b := h.event(t, number, "synchronize", sha(11))
	h.build(t, b)
	opB := h.operation(t, env.ActivateRoute, b.PullRequestID, b.Generation)
	if err := h.process(t, h.worker, opB.ID); err != nil {
		t.Fatal(err)
	}
	host := h.host(t, a.PullRequestID)
	h.serves(t, host, marker(b.PullRequestID, b.Generation, b.Revision))

	// A's activation arrives late. Even straight to the controller (skipping the
	// worker's own freshness check) the fence refuses it and the table stays.
	version, _ := h.file.Version()
	if err := h.controller.Execute(ctxFor(t), opA); !errors.Is(err, env.ErrSuperseded) {
		t.Fatalf("stale activation: %v", err)
	}
	if after, _ := h.file.Version(); after != version {
		t.Fatalf("stale activation rewrote the table: %d -> %d", version, after)
	}
	h.servesNow(t, host, marker(b.PullRequestID, b.Generation, b.Revision))
	if err := h.process(t, h.worker, opA.ID); err != nil || h.state(t, opA.ID) != "superseded" {
		t.Fatalf("stale activation through the worker: %s %v", h.state(t, opA.ID), err)
	}
	h.drain(t)

	// Same-generation Rebuild: a new revision of the same commit.
	if _, err := h.pool.Exec(ctxFor(t), "UPDATE build_intents SET state='succeeded', completed_at=clock_timestamp() WHERE pull_request_id=$1", b.PullRequestID); err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := h.pool.QueryRow(ctxFor(t), "SELECT id FROM users LIMIT 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	outcome, err := env.NewIntentRepository(h.pool).RequestIntent(ctxFor(t), env.IntentCommand{UserID: userID, PullRequestID: b.PullRequestID, ExpectedRevision: b.Revision, IdempotencyKey: fmt.Sprintf("rebuild-%016d", number), Action: env.Rebuild})
	if err != nil {
		t.Fatal(err)
	}
	rebuild := b
	rebuild.Revision = outcome.Intent.Revision
	h.build(t, rebuild)
	opRebuild := h.operation(t, env.ActivateRoute, rebuild.PullRequestID, rebuild.Generation)
	if opRebuild.Revision != rebuild.Revision {
		t.Fatalf("rebuild activation revision %d", opRebuild.Revision)
	}
	if err := h.process(t, h.worker, opRebuild.ID); err != nil {
		t.Fatal(err)
	}
	h.serves(t, host, marker(rebuild.PullRequestID, rebuild.Generation, rebuild.Revision))
	// B's candidate still exists (its cleanup has not run), so the replayed
	// older revision passes the candidate check and is refused by the fence.
	if err := h.controller.Execute(ctxFor(t), opB); !errors.Is(err, env.ErrSuperseded) {
		t.Fatalf("older revision replayed over the rebuild: %v", err)
	}
	h.servesNow(t, host, marker(rebuild.PullRequestID, rebuild.Generation, rebuild.Revision))
	h.drain(t)

	// Close, then reopen before the removal runs: the late removal must not
	// remove the reopened PR's new route.
	closed := h.closePR(t, number, sha(11))
	removal := h.operation(t, env.RemoveRoute, a.PullRequestID, closed.Generation)
	preClose := removal.Payload.Previous
	reopened := h.event(t, number, "reopened", sha(12))
	h.build(t, reopened)
	opC := h.operation(t, env.ActivateRoute, reopened.PullRequestID, reopened.Generation)
	if err := h.process(t, h.worker, opC.ID); err != nil {
		t.Fatal(err)
	}
	h.serves(t, host, marker(reopened.PullRequestID, reopened.Generation, reopened.Revision))
	if err := h.controller.Execute(ctxFor(t), removal); !errors.Is(err, env.ErrSuperseded) {
		t.Fatalf("late removal: %v", err)
	}
	h.servesNow(t, host, marker(reopened.PullRequestID, reopened.Generation, reopened.Revision))
	if err := h.process(t, h.worker, removal.ID); err != nil || h.state(t, removal.ID) != "superseded" {
		t.Fatalf("late removal through the worker: %s %v", h.state(t, removal.ID), err)
	}
	h.drain(t)
	h.gone(t, preClose.ContainerID, preClose.ImageTag)
	h.servesNow(t, host, marker(reopened.PullRequestID, reopened.Generation, reopened.Revision))

	h.closePR(t, number, sha(12))
	h.drain(t)
	h.unavailable(t, host)
	h.noLeftovers(t, a.PullRequestID)
}

// gatedRouter blocks the first convergence check until released.
type gatedRouter struct {
	inner   routing.Router
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedRouter) Attach(ctx context.Context, networkID string) error {
	return g.inner.Attach(ctx, networkID)
}

func (g *gatedRouter) Router(ctx context.Context, name string) (routing.Observed, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.inner.Router(ctx, name)
}

func TestRealRoutingCloseDuringPublicationWins(t *testing.T) {
	h := realHarness(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(20))
	h.build(t, a)
	h.drain(t)
	host := h.host(t, a.PullRequestID)
	h.serves(t, host, marker(a.PullRequestID, a.Generation, a.Revision))
	b := h.event(t, number, "synchronize", sha(21))
	h.build(t, b)
	opB := h.operation(t, env.ActivateRoute, b.PullRequestID, b.Generation)
	gate := &gatedRouter{inner: h.traefik, entered: make(chan struct{}), release: make(chan struct{})}
	gated, err := h.newController(gate)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := h.newWorker(env.Executors{env.ActivateRoute: gated, env.RemoveRoute: h.controller, env.CleanupArtifact: h.runtime.CleanupExecutor(), env.ReportStatus: h.reports})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.process(t, worker, opB.ID) }()
	select {
	case <-gate.entered: // B is accepted and written; its confirmation waits.
	case <-time.After(time.Minute):
		t.Fatal("activation never reached the router")
	}
	closed := h.closePR(t, number, sha(21))
	removal := h.operation(t, env.RemoveRoute, a.PullRequestID, closed.Generation)
	if err := h.process(t, h.worker, removal.ID); err != nil {
		t.Fatal(err)
	}
	h.unavailable(t, host)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := h.state(t, opB.ID); state != "superseded" {
		t.Fatalf("activation during close: %s", state)
	}
	h.drain(t)
	h.unavailable(t, host)
	if s := h.snapshot(t, a.PullRequestID); s.Status != "torn_down" {
		t.Fatalf("environment %+v", s)
	}
	h.noLeftovers(t, a.PullRequestID)
}

func TestRealRoutingRecoversAfterControllerAndRouterRestarts(t *testing.T) {
	h := realHarness(t)
	ctx := ctxFor(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(30))
	h.build(t, a)
	op := h.operation(t, env.ActivateRoute, a.PullRequestID, a.Generation)
	host := h.host(t, a.PullRequestID)
	// The controller crashes right after accepting: the version is durable but
	// not written, so the router does not serve it yet.
	endpoint, err := h.backends().Backend(ctx, routing.Candidate{PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, ContainerID: op.Payload.Artifact.ContainerID, ImageTag: op.Payload.Artifact.ImageTag})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.traefik.Attach(ctx, endpoint.NetworkID); err != nil {
		t.Fatal(err)
	}
	if d, err := h.repo.AcceptRoute(ctx, op, env.RouteTarget{Artifact: op.Payload.Artifact, Backend: endpoint.Name, Port: endpoint.Port, NetworkID: endpoint.NetworkID}); err != nil || !d.Accepted {
		t.Fatalf("accept %+v %v", d, err)
	}
	h.unavailable(t, host)
	// A restarted controller rewrites the table from PostgreSQL.
	restarted, err := h.newController(h.traefik)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	want := marker(a.PullRequestID, a.Generation, a.Revision)
	h.serves(t, host, want)
	// The interrupted operation resumes idempotently and finishes the swap.
	if err := h.process(t, h.worker, op.ID); err != nil || h.state(t, op.ID) != "succeeded" {
		t.Fatalf("resumed activation %s %v", h.state(t, op.ID), err)
	}
	if s := h.snapshot(t, a.PullRequestID); s.Status != "running" {
		t.Fatalf("environment %+v", s)
	}
	// The router restarts: routes come back from the file and the certificate
	// from its persistent ACME storage, without a new issuance. Traefik writes a
	// new certificate to that storage asynchronously, so restart only once the
	// certificate it serves is stored. (Traefik renews at start what it thinks
	// is near expiry; the harness gives it Pebble's real certificate duration.)
	var stored *x509.Certificate
	h.eventually(t, "the served certificate in ACME storage", func() error {
		served, err := h.certificate(host)
		if err != nil {
			return err
		}
		if stored, err = h.readStoredCertificate(); err != nil {
			return err
		}
		if served.SerialNumber.Cmp(stored.SerialNumber) != 0 {
			return errors.New("not stored yet")
		}
		return nil
	})
	restartedAt := time.Now()
	if _, err := h.docker.ContainerRestart(ctx, h.routerName, client.ContainerRestartOptions{}); err != nil {
		t.Fatal(err)
	}
	h.serves(t, host, want)
	// Traefik may answer with its default certificate for a moment while it
	// loads the stored one; it must settle on exactly that one.
	h.eventually(t, "the restarted router serves the certificate from its ACME storage", func() error {
		after, err := h.certificate(host)
		if err != nil {
			return err
		}
		if after.SerialNumber.Cmp(stored.SerialNumber) != 0 {
			return errors.New("not the stored certificate")
		}
		return nil
	})
	if logs := h.routerLogs(t, restartedAt); strings.Contains(logs, "Server responded with a certificate") {
		t.Fatal("the router obtained a new certificate after restarting")
	}
	// Losing the file drops every route until the controller writes it again.
	if err := os.Remove(h.file.Path()); err != nil {
		t.Fatal(err)
	}
	h.eventually(t, "routes dropped with the file", func() error {
		r, err := h.get(host, "/")
		if err != nil {
			return err
		}
		if r.status != http.StatusNotFound {
			return fmt.Errorf("still %d", r.status)
		}
		return nil
	})
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h.serves(t, host, want)

	h.closePR(t, number, sha(30))
	h.drain(t)
	h.noLeftovers(t, a.PullRequestID)
}

// storedCertificate reads the wildcard certificate from the router's
// persistent acme.json.
// readStoredCertificate reads the one certificate in the router's acme.json.
func (h *harness) readStoredCertificate() (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := dockerx.Run(ctx, h.docker, h.routerName, "cat", "/acme/acme.json")
	if err != nil || result.ExitCode != 0 {
		return nil, fmt.Errorf("read acme.json: %v (exit %d)", err, result.ExitCode)
	}
	var storage map[string]struct {
		Certificates []struct {
			Certificate string `json:"certificate"`
		} `json:"Certificates"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &storage); err != nil {
		return nil, err
	}
	certificates := storage["preview"].Certificates
	if len(certificates) != 1 {
		return nil, fmt.Errorf("acme.json holds %d certificates", len(certificates))
	}
	chain, err := base64.StdEncoding.DecodeString(certificates[0].Certificate)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(chain)
	if block == nil {
		return nil, errors.New("acme.json certificate is not PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// routerLogs returns the router's output since a moment.
func (h *harness) routerLogs(t *testing.T, since time.Time) string {
	t.Helper()
	reader, err := h.docker.ContainerLogs(ctxFor(t), h.routerName, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Since: since.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, io.LimitReader(reader, 4<<20)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// crashOnce performs its first operation and then reports a failure, as a
// worker that crashed after the side effect but before recording it would.
type crashOnce struct {
	inner   env.OperationExecutor
	crashed atomic.Bool
}

func (c *crashOnce) Execute(ctx context.Context, op env.Operation) error {
	err := c.inner.Execute(ctx, op)
	if err == nil && c.crashed.CompareAndSwap(false, true) {
		return &env.OperationFailure{Code: "simulated_crash"}
	}
	return err
}

func TestRealRoutingTeardownLeavesNothingButUnrelatedObjects(t *testing.T) {
	h := realHarness(t)
	ctx := ctxFor(t)
	// Objects that must survive: an unlabelled container, an image of another
	// PR that carries Previa's labels, and another PR's labelled container.
	unrelated, err := h.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: "previa-rt-unrelated-" + runSuffix, Config: &container.Config{Image: appBaseImage, Cmd: []string{"sleep", "3600"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.docker.ContainerRemove(context.Background(), unrelated.ID, client.ContainerRemoveOptions{Force: true})
	})
	otherPR := int64(999_000_000) + run%1_000_000
	foreignRef := fmt.Sprintf("previa/pr-%d:g1-r1-%s", otherPR, sha(99)[:12])
	foreignImage, err := h.docker.ContainerCommit(ctx, unrelated.ID, client.ContainerCommitOptions{Reference: foreignRef, Changes: []string{
		"LABEL " + dockerx.LabelManaged + "=true", "LABEL " + dockerx.LabelKind + "=" + dockerx.KindImage, "LABEL " + dockerx.LabelPR + "=" + fmt.Sprint(otherPR),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.docker.ImageRemove(context.Background(), foreignRef, client.ImageRemoveOptions{Force: true})
	})
	foreign, err := h.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: foreignRef, Labels: map[string]string{
		dockerx.LabelManaged: "true", dockerx.LabelKind: dockerx.KindPreview, dockerx.LabelPR: fmt.Sprint(otherPR),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.docker.ContainerRemove(context.Background(), foreign.ID, client.ContainerRemoveOptions{Force: true})
	})

	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(40))
	h.build(t, a)
	h.drain(t)
	b := h.event(t, number, "synchronize", sha(41))
	h.build(t, b)
	h.drain(t)
	host := h.host(t, a.PullRequestID)
	h.serves(t, host, marker(b.PullRequestID, b.Generation, b.Revision))
	active := h.snapshot(t, a.PullRequestID).Artifact

	// A duplicate close/teardown creates one removal.
	closed := h.closePR(t, number, sha(41))
	if err := h.service.Teardown(ctx, closed); err != nil {
		t.Fatal(err)
	}
	var removals int
	if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", a.PullRequestID).Scan(&removals); err != nil || removals != 1 {
		t.Fatalf("removals %d %v", removals, err)
	}
	removal := h.operation(t, env.RemoveRoute, a.PullRequestID, closed.Generation)

	// Router outage: the removal is accepted durably but cannot be confirmed,
	// so it stays retryable and nothing is cleaned yet.
	if _, err := h.docker.ContainerStop(ctx, h.routerName, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	impatient, err := h.newController(h.traefik)
	if err != nil {
		t.Fatal(err)
	}
	impatient.Converge = 3 * time.Second
	outage, err := h.newWorker(env.Executors{env.RemoveRoute: impatient})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.process(t, outage, removal.ID); err == nil || h.state(t, removal.ID) != "retryable" {
		t.Fatalf("removal during an outage: %s %v", h.state(t, removal.ID), err)
	}
	if _, err := h.docker.ContainerInspect(ctx, active.ContainerID, client.ContainerInspectOptions{}); err != nil {
		t.Fatal("the preview was removed before its route removal converged")
	}
	if _, err := h.docker.ContainerStart(ctx, h.routerName, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, "UPDATE environment_operations SET available_at=now() WHERE id=$1", removal.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.process(t, h.worker, removal.ID); err != nil || h.state(t, removal.ID) != "succeeded" {
		t.Fatalf("removal after the outage: %s %v", h.state(t, removal.ID), err)
	}
	h.unavailable(t, host)

	// The container is already gone when its cleanup runs, and the worker
	// "crashes" after one removal before recording it: both resume safely.
	if _, err := h.docker.ContainerRemove(ctx, active.ContainerID, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	crashy, err := h.newWorker(env.Executors{env.CleanupArtifact: &crashOnce{inner: h.runtime.CleanupExecutor()}})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		var id int64
		err := h.pool.QueryRow(ctx, "SELECT id FROM environment_operations WHERE kind='cleanup_artifact' AND pull_request_id=$1 AND state IN ('pending','retryable') ORDER BY id LIMIT 1", a.PullRequestID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = h.process(t, crashy, id)
		if _, err := h.pool.Exec(ctx, "UPDATE environment_operations SET available_at=now() WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
	}
	h.drain(t)
	if s := h.snapshot(t, a.PullRequestID); s.Status != "torn_down" {
		t.Fatalf("environment %+v", s)
	}
	h.gone(t, active.ContainerID, active.ImageTag)
	h.noLeftovers(t, a.PullRequestID)
	if _, err := h.docker.ContainerInspect(ctx, unrelated.ID, client.ContainerInspectOptions{}); err != nil {
		t.Fatal("unrelated container removed")
	}
	if _, err := h.docker.ContainerInspect(ctx, foreign.ID, client.ContainerInspectOptions{}); err != nil {
		t.Fatal("another PR's container removed")
	}
	if inspected, err := h.docker.ImageInspect(ctx, foreignRef); err != nil || inspected.ID != foreignImage.ID {
		t.Fatal("another PR's image removed")
	}
	// The PR and its lifetime environment are kept for a later reopen.
	var envs int
	if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&envs); err != nil || envs != 1 {
		t.Fatal("lifetime environment was not retained")
	}
}

func TestRealRoutingRevocationTearsDownWithoutGitHubAccess(t *testing.T) {
	h := realHarness(t)
	ctx := ctxFor(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(50))
	h.build(t, a)
	h.drain(t)
	host := h.host(t, a.PullRequestID)
	h.serves(t, host, marker(a.PullRequestID, a.Generation, a.Revision))
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), "UPDATE installations SET status='active', cleanup_pending=false WHERE github_installation_id=$1", h.installationID)
	})
	// The installation is suspended. The harness has no GitHub App client at
	// all: everything below runs on PostgreSQL and Docker only.
	deliveries := webhooks.NewDeliveryService(h.pool, h.broker, nil)
	sum := sha256.Sum256([]byte(fmt.Sprint("suspend", run)))
	event := &github.InstallationEvent{Action: new("suspend"), Installation: &github.Installation{
		ID: new(h.installationID), Account: &github.User{Login: new("routing")}, UpdatedAt: &github.Timestamp{Time: time.Now()},
	}}
	if err := deliveries.Process(ctx, "routing-suspend-"+runSuffix, "installation", hex.EncodeToString(sum[:]), event); err != nil {
		t.Fatal(err)
	}
	sweeper, err := env.NewTeardownSweeper(h.repo, h.runtime, h.logger, 50)
	if err != nil {
		t.Fatal(err)
	}
	if sweep, err := sweeper.RunOnce(ctx); err != nil || sweep.Started < 1 {
		t.Fatalf("sweep %+v %v", sweep, err)
	}
	h.drain(t)
	h.unavailable(t, host)
	if sweep, err := sweeper.RunOnce(ctx); err != nil || sweep.Cleared != 1 {
		t.Fatalf("marker not cleared: %+v %v", sweep, err)
	}
	h.noLeftovers(t, a.PullRequestID)
	if s := h.snapshot(t, a.PullRequestID); s.Status != "torn_down" {
		t.Fatalf("environment %+v", s)
	}
	// Restored access does not bring the preview back by itself.
	if _, err := h.pool.Exec(ctx, "UPDATE installations SET status='active' WHERE github_installation_id=$1", h.installationID); err != nil {
		t.Fatal(err)
	}
	var intent string
	if err := h.pool.QueryRow(ctx, "SELECT state FROM build_intents WHERE pull_request_id=$1", a.PullRequestID).Scan(&intent); err != nil || intent != "superseded" {
		t.Fatalf("intent %q %v", intent, err)
	}
}

func TestRealRoutingCloseWhileBuildingCleansTheCandidate(t *testing.T) {
	h := realHarness(t)
	number := numbers.Add(1)
	a := h.event(t, number, "opened", sha(60))
	gate := make(chan struct{})
	h.builder.mu.Lock()
	h.builder.gate = gate
	h.builder.mu.Unlock()
	t.Cleanup(func() {
		h.builder.mu.Lock()
		h.builder.gate = nil
		h.builder.mu.Unlock()
	})
	done := make(chan error, 1)
	go func() { done <- h.service.Build(ctxFor(t), a) }()
	// Admission provisions the environment before the build starts.
	h.eventually(t, "build admitted", func() error {
		var status string
		return h.pool.QueryRow(ctxFor(t), "SELECT status FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&status)
	})
	h.closePR(t, number, sha(60))
	h.drain(t)
	close(gate)
	if err := <-done; err != nil {
		t.Logf("superseded build returned: %v", err)
	}
	h.drain(t)
	if s := h.snapshot(t, a.PullRequestID); s.Status != "torn_down" {
		t.Fatalf("environment %+v", s)
	}
	h.noLeftovers(t, a.PullRequestID)
}
