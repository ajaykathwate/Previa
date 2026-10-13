package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
)

type fakeFence struct {
	mu       sync.Mutex
	decision environments.RouteDecision
	err      error
	table    environments.RouteTable
	tableErr error
	state    environments.RouteState
	hasState bool
	served   []string
	accepts  []environments.RouteTarget
	removals int
}

func (f *fakeFence) AcceptRoute(_ context.Context, _ environments.Operation, target environments.RouteTarget) (environments.RouteDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepts = append(f.accepts, target)
	return f.decision, f.err
}
func (f *fakeFence) AcceptRouteRemoval(context.Context, environments.Operation) (environments.RouteDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removals++
	return f.decision, f.err
}
func (f *fakeFence) MarkRouteServed(_ context.Context, env, generation, revision int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, serviceName(env, generation, revision))
	return true, nil
}
func (f *fakeFence) RouteTable(context.Context) (environments.RouteTable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.table, f.tableErr
}
func (f *fakeFence) RouteState(context.Context, int64) (environments.RouteState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.hasState, nil
}

type fakeRouter struct {
	mu        sync.Mutex
	attached  []string
	attachErr error
	observe   func(name string) (Observed, error)
}

func (r *fakeRouter) Attach(_ context.Context, networkID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attached = append(r.attached, networkID)
	return r.attachErr
}
func (r *fakeRouter) Router(_ context.Context, name string) (Observed, error) { return r.observe(name) }

func activation() environments.Operation {
	request := environments.NewRequest(11, 2, "sha", "open")
	request.Revision = 1
	return environments.Operation{ID: 1, PullRequestID: 11, Generation: 2, Revision: 1, Kind: environments.ActivateRoute,
		Payload: environments.OperationPayload{Request: request, Artifact: environments.Artifact{ContainerID: "c1", ImageTag: "previa/pr-11:g2-r1-sha"}}}
}

func removal() environments.Operation {
	request := environments.NewRequest(11, 3, "sha", "closed")
	return environments.Operation{ID: 2, PullRequestID: 11, Generation: 3, Kind: environments.RemoveRoute, Payload: environments.OperationPayload{Request: request}}
}

func newTestController(t *testing.T, fence *fakeFence, backends BackendFunc, router *fakeRouter) (*Controller, *File) {
	t.Helper()
	file, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewController(fence, backends, router, file, testSettings, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.Converge, c.Poll = 200*time.Millisecond, 5*time.Millisecond
	return c, file
}

func healthyBackend(context.Context, Candidate) (Backend, error) {
	return Backend{Name: "previa-pr11-g2-r1-1", Port: 8080, NetworkID: "net-11"}, nil
}

func serving(service string) func(string) (Observed, error) {
	return func(string) (Observed, error) {
		return Observed{Present: true, Service: service, Status: "enabled"}, nil
	}
}

func TestControllerActivatesVerifiedCandidate(t *testing.T) {
	route := environments.Route{EnvironmentID: 5, Hostname: "pr-11-1-aaaaaaaa", Generation: 2, Revision: 1, Backend: "previa-pr11-g2-r1-1", Port: 8080}
	fence := &fakeFence{decision: environments.RouteDecision{Accepted: true, EnvironmentID: 5, TableVersion: 4}, table: environments.RouteTable{Version: 4, Routes: []environments.Route{route}}}
	var seen []Candidate
	router := &fakeRouter{observe: serving("preview-5-g2-r1")}
	c, file := newTestController(t, fence, func(ctx context.Context, cand Candidate) (Backend, error) {
		seen = append(seen, cand)
		return healthyBackend(ctx, cand)
	}, router)
	if err := c.Execute(t.Context(), activation()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != (Candidate{PullRequestID: 11, Generation: 2, Revision: 1, ContainerID: "c1", ImageTag: "previa/pr-11:g2-r1-sha"}) {
		t.Fatalf("verified %+v", seen)
	}
	if len(router.attached) != 1 || router.attached[0] != "net-11" {
		t.Fatalf("attached %v", router.attached)
	}
	if len(fence.accepts) != 1 || fence.accepts[0].Backend != "previa-pr11-g2-r1-1" || fence.accepts[0].Port != 8080 || fence.accepts[0].NetworkID != "net-11" {
		t.Fatalf("accepted %+v", fence.accepts)
	}
	if v, _ := file.Version(); v != 4 {
		t.Fatalf("file version %d", v)
	}
	data, _ := os.ReadFile(file.Path())
	if !strings.Contains(string(data), "preview-5-g2-r1") {
		t.Fatal("route table not written")
	}
	if len(fence.served) != 1 || fence.served[0] != "preview-5-g2-r1" {
		t.Fatalf("served %v", fence.served)
	}
}

func TestPrivateActivationReportsMissingAuthInsteadOfConvergence(t *testing.T) {
	route := environments.Route{EnvironmentID: 5, Hostname: "pr-11-1-aaaaaaaa", Generation: 2, Revision: 1, Backend: "previa-pr11-g2-r1-1", Port: 8080, Private: true}
	fence := &fakeFence{err: &environments.OperationFailure{Code: "private_preview_unconfigured", Permanent: true}, table: environments.RouteTable{Version: 4, Routes: []environments.Route{route}}}
	c, file := newTestController(t, fence, healthyBackend, &fakeRouter{observe: serving("noop")})
	var failure *environments.OperationFailure
	if err := c.Execute(t.Context(), activation()); !errors.As(err, &failure) || failure.Code != "private_preview_unconfigured" || !failure.Permanent {
		t.Fatalf("missing auth must fail explicitly: %v", err)
	}
	data, err := os.ReadFile(file.Path())
	if err != nil || strings.Contains(string(data), "http://previa-pr11") || len(fence.served) != 0 {
		t.Fatalf("missing auth exposed or confirmed a private app: %s, %v", data, err)
	}
}

func TestMissingPrivateAuthRemainsPermanentWhenRouteRefreshFails(t *testing.T) {
	refreshErr := errors.New("route table unavailable")
	fence := &fakeFence{
		err:      &environments.OperationFailure{Code: "private_preview_unconfigured", Permanent: true},
		tableErr: refreshErr,
	}
	c, _ := newTestController(t, fence, healthyBackend, &fakeRouter{})
	err := c.Execute(t.Context(), activation())
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || failure.Code != "private_preview_unconfigured" || !failure.Permanent {
		t.Fatalf("route refresh hid a permanent configuration failure: %v", err)
	}
	if !errors.Is(err, refreshErr) || len(fence.served) != 0 {
		t.Fatalf("refresh failure lost or route confirmed: %v, %v", err, fence.served)
	}
}

func TestSyncRefreshesAuthConfigurationWithoutANewRouteVersion(t *testing.T) {
	route := environments.Route{EnvironmentID: 5, Hostname: "pr-11-1-aaaaaaaa", Generation: 2, Revision: 1, Backend: "previa-pr11-g2-r1-1", Port: 8080, Private: true}
	fence := &fakeFence{table: environments.RouteTable{Version: 4, Routes: []environments.Route{route}}}
	c, file := newTestController(t, fence, healthyBackend, &fakeRouter{})
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.settings.AccessURL = "http://server:8080/previews/auth"
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Path())
	if err != nil || !strings.Contains(string(data), "forwardAuth") || !strings.Contains(string(data), "preview-5-g2-r1") {
		t.Fatalf("auth config was not refreshed: %s, %v", data, err)
	}
	if version, err := file.Version(); err != nil || version != 4 {
		t.Fatalf("version = %d, %v", version, err)
	}
}

func TestControllerSupersededActivationWritesNothing(t *testing.T) {
	fence := &fakeFence{decision: environments.RouteDecision{Accepted: false, EnvironmentID: 5}}
	c, file := newTestController(t, fence, healthyBackend, &fakeRouter{observe: serving("x")})
	if err := c.Execute(t.Context(), activation()); !errors.Is(err, environments.ErrSuperseded) {
		t.Fatalf("got %v", err)
	}
	if v, _ := file.Version(); v != 0 || len(fence.served) != 0 {
		t.Fatal("superseded activation published")
	}
}

func TestControllerClassifiesFailures(t *testing.T) {
	unhealthy := &environments.OperationFailure{Code: "unhealthy", Permanent: true}
	cases := []struct {
		name      string
		backends  BackendFunc
		router    *fakeRouter
		code      string
		permanent bool
	}{
		{"candidate failure keeps its code", func(context.Context, Candidate) (Backend, error) { return Backend{}, unhealthy }, &fakeRouter{}, "unhealthy", true},
		{"unclassified candidate error retries", func(context.Context, Candidate) (Backend, error) { return Backend{}, errors.New("docker") }, &fakeRouter{}, "candidate_unverified", false},
		{"router attach fails", healthyBackend, &fakeRouter{attachErr: errors.New("gone")}, "router_unavailable", false},
		{"router never converges", healthyBackend, &fakeRouter{observe: serving("preview-5-g1-r1")}, "route_not_converged", false},
		{"router API down", healthyBackend, &fakeRouter{observe: func(string) (Observed, error) { return Observed{}, ErrRouterUnavailable }}, "router_unavailable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fence := &fakeFence{decision: environments.RouteDecision{Accepted: true, EnvironmentID: 5, TableVersion: 1}, table: environments.RouteTable{Version: 1}}
			c, _ := newTestController(t, fence, tc.backends, tc.router)
			err := c.Execute(t.Context(), activation())
			var failure *environments.OperationFailure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Permanent != tc.permanent {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestControllerStopsWaitingWhenANewerVersionIsAccepted(t *testing.T) {
	fence := &fakeFence{decision: environments.RouteDecision{Accepted: true, EnvironmentID: 5, TableVersion: 1}, table: environments.RouteTable{Version: 1},
		state: environments.RouteState{EnvironmentID: 5, Generation: 2, Revision: 2, Active: true}, hasState: true}
	c, _ := newTestController(t, fence, healthyBackend, &fakeRouter{observe: serving("preview-5-g2-r2")})
	if err := c.Execute(t.Context(), activation()); !errors.Is(err, environments.ErrSuperseded) {
		t.Fatalf("got %v", err)
	}
	if len(fence.served) != 0 {
		t.Fatal("older version recorded as served")
	}
}

func TestControllerRemovesRoute(t *testing.T) {
	fence := &fakeFence{decision: environments.RouteDecision{Accepted: true, EnvironmentID: 5, TableVersion: 6}, table: environments.RouteTable{Version: 6}}
	present := true
	router := &fakeRouter{observe: func(string) (Observed, error) {
		defer func() { present = false }() // gone on the second look
		return Observed{Present: present, Service: "preview-5-g2-r1", Status: "enabled"}, nil
	}}
	c, file := newTestController(t, fence, healthyBackend, router)
	if err := c.Execute(t.Context(), removal()); err != nil {
		t.Fatal(err)
	}
	if v, _ := file.Version(); v != 6 || fence.removals != 1 || len(fence.served) != 1 || fence.served[0] != "preview-5-g3-r0" {
		t.Fatalf("version %d removals %d served %v", v, fence.removals, fence.served)
	}
	// A teardown of a PR that never had an environment changes nothing.
	fence = &fakeFence{decision: environments.RouteDecision{Accepted: true}}
	c, file = newTestController(t, fence, healthyBackend, router)
	if err := c.Execute(t.Context(), removal()); err != nil {
		t.Fatal(err)
	}
	if v, _ := file.Version(); v != 0 || len(fence.served) != 0 {
		t.Fatal("removal without an environment wrote a table")
	}
	// A newer route (a reopened PR's activation) wins over a late removal.
	fence = &fakeFence{decision: environments.RouteDecision{Accepted: false, EnvironmentID: 5}}
	c, _ = newTestController(t, fence, healthyBackend, router)
	if err := c.Execute(t.Context(), removal()); !errors.Is(err, environments.ErrSuperseded) {
		t.Fatalf("got %v", err)
	}
}

func TestControllerSyncWritesOnlyWhenBehind(t *testing.T) {
	fence := &fakeFence{table: environments.RouteTable{}}
	c, file := newTestController(t, fence, healthyBackend, &fakeRouter{})
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file.Path()); err != nil {
		t.Fatal("empty table not written on first sync")
	}
	fence.table = environments.RouteTable{Version: 3}
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, _ := file.Version(); v != 3 {
		t.Fatalf("version %d", v)
	}
	fence.table = environments.RouteTable{Version: 2} // never backwards
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, _ := file.Version(); v != 3 {
		t.Fatalf("version went back to %d", v)
	}
}

func TestControllerSyncRestoresNetworksAtTheSameVersion(t *testing.T) {
	fence := &fakeFence{table: environments.RouteTable{Version: 3, Routes: []environments.Route{
		{EnvironmentID: 5, Hostname: "pr-11-1-aaaaaaaa", Backend: "app-11", Port: 8080, NetworkID: "net-11"},
		{EnvironmentID: 6, Hostname: "pr-12-1-aaaaaaaa", Backend: "app-12", Port: 8080, NetworkID: "net-12", Asleep: true},
		{EnvironmentID: 7, Hostname: "pr-13-1-aaaaaaaa", Backend: "app-13", Port: 8080, NetworkID: "net-11", Private: true},
	}}}
	router := &fakeRouter{}
	c, file := newTestController(t, fence, healthyBackend, router)
	for range 2 {
		if err := c.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(router.attached, ",") != "net-11,net-12,net-11,net-12" {
		t.Fatalf("network recovery: %v", router.attached)
	}
	// An older table must not connect networks or replace a newer file.
	fence.table.Version = 2
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, _ := file.Version(); v != 3 || len(router.attached) != 4 || len(fence.accepts) != 0 {
		t.Fatalf("stale sync changed accepted routing: version=%d attachments=%v", v, router.attached)
	}
}

func TestControllerSyncKeepsPrivateDenialWhenNetworkRecoveryFails(t *testing.T) {
	route := environments.Route{EnvironmentID: 5, Hostname: "pr-11-1-aaaaaaaa", Backend: "app-11", Port: 8080, NetworkID: "net-11", Private: true}
	fence := &fakeFence{table: environments.RouteTable{Version: 3, Routes: []environments.Route{route}}}
	router := &fakeRouter{attachErr: ErrRouterUnavailable}
	c, file := newTestController(t, fence, healthyBackend, router)
	if err := c.Sync(t.Context()); !errors.Is(err, ErrRouterUnavailable) {
		t.Fatalf("network failure not returned for retry: %v", err)
	}
	data, err := os.ReadFile(file.Path())
	if err != nil || strings.Contains(string(data), "http://app-11") || len(router.attached) != 1 {
		t.Fatalf("missing auth denial was not written: %s %v", data, err)
	}
}

func TestControllerRejectsOtherOperations(t *testing.T) {
	c, _ := newTestController(t, &fakeFence{}, healthyBackend, &fakeRouter{})
	err := c.Execute(t.Context(), environments.Operation{Kind: environments.CleanupArtifact})
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || !failure.Permanent {
		t.Fatalf("got %v", err)
	}
}
