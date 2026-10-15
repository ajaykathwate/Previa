package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Fence is the route controller's durable state; *environments.Repository
// implements it.
type Fence interface {
	AcceptRoute(ctx context.Context, op environments.Operation, target environments.RouteTarget) (environments.RouteDecision, error)
	AcceptRouteRemoval(ctx context.Context, op environments.Operation) (environments.RouteDecision, error)
	MarkRouteServed(ctx context.Context, environmentID, generation, revision int64) (bool, error)
	RouteTable(ctx context.Context) (environments.RouteTable, error)
	RouteState(ctx context.Context, pullRequestID int64) (environments.RouteState, bool, error)
}

// Candidate identifies the container a route activation wants to publish.
type Candidate struct {
	PullRequestID int64
	Generation    int64
	Revision      int64
	ContainerID   string
	ImageTag      string
}

// Backend is how the router reaches a verified candidate.
type Backend struct {
	Name      string // container name, resolved on the PR's network
	Port      int
	NetworkID string
}

// Backends verifies that a candidate is the PR's healthy preview and says how
// to reach it. Errors that are *environments.OperationFailure keep their code.
type Backends interface {
	Backend(ctx context.Context, candidate Candidate) (Backend, error)
}

// BackendFunc adapts a function to Backends.
type BackendFunc func(context.Context, Candidate) (Backend, error)

func (f BackendFunc) Backend(ctx context.Context, c Candidate) (Backend, error) { return f(ctx, c) }

// Router is the Docker-side control of the router; *Traefik implements it.
type Router interface {
	Attach(ctx context.Context, networkID string) error
	Router(ctx context.Context, name string) (Observed, error)
}

// Controller is the single logical owner of route changes. It implements
// environments.OperationExecutor for route activations and removals, and
// environments.RouteSyncer.
//
// The fence is in PostgreSQL: a route version is accepted only while the PR
// still wants it and only if it is newer than the accepted one, with removals
// kept as tombstones. Traefik's configuration is a materialized view of the
// accepted table, written to one file whose version only moves forward, so
// the router can lag (by its reload time) but never step back to an older
// route. A version counts as served only once Traefik's API shows it.
type Controller struct {
	fence    Fence
	backends Backends
	router   Router
	file     *File
	settings Settings
	logger   *slog.Logger
	// Converge bounds how long an operation waits for the router to load a
	// version; Poll is how often it looks.
	Converge time.Duration
	Poll     time.Duration
}

func NewController(fence Fence, backends Backends, router Router, file *File, settings Settings, logger *slog.Logger) (*Controller, error) {
	if fence == nil || backends == nil || router == nil || file == nil || logger == nil {
		return nil, errors.New("route controller requires a fence, backends, a router, a file and a logger")
	}
	if err := settings.validate(); err != nil {
		return nil, err
	}
	return &Controller{fence: fence, backends: backends, router: router, file: file, settings: settings, logger: logger,
		Converge: 30 * time.Second, Poll: 250 * time.Millisecond}, nil
}

// Execute performs one route operation.
func (c *Controller) Execute(ctx context.Context, op environments.Operation) error {
	switch op.Kind {
	case environments.ActivateRoute:
		return c.activate(ctx, op)
	case environments.RemoveRoute:
		return c.remove(ctx, op)
	default:
		return &environments.OperationFailure{Code: "invalid_operation", Permanent: true}
	}
}

// activate publishes a healthy candidate: verify it, let the router reach its
// network, accept the version behind the fence, write the table, and wait
// until the router serves it.
func (c *Controller) activate(ctx context.Context, op environments.Operation) error {
	artifact := op.Payload.Artifact
	backend, err := c.backends.Backend(ctx, Candidate{
		PullRequestID: op.PullRequestID, Generation: op.Generation, Revision: op.Revision,
		ContainerID: artifact.ContainerID, ImageTag: artifact.ImageTag,
	})
	if err != nil {
		var failure *environments.OperationFailure
		if errors.As(err, &failure) {
			return err
		}
		return &environments.OperationFailure{Code: "candidate_unverified", Err: err}
	}
	if err := c.router.Attach(ctx, backend.NetworkID); err != nil {
		return &environments.OperationFailure{Code: "router_unavailable", Err: err}
	}
	decision, err := c.fence.AcceptRoute(ctx, op, environments.RouteTarget{
		Artifact: artifact, Backend: backend.Name, Port: backend.Port, NetworkID: backend.NetworkID,
		AuthConfigured: c.settings.AccessURL != "",
	})
	if err != nil {
		var failure *environments.OperationFailure
		if errors.As(err, &failure) && failure.Code == "private_preview_unconfigured" {
			// Refresh existing routes to deny access, without accepting this
			// failed candidate as a target that later recovery could publish.
			if err := c.Sync(ctx); err != nil {
				// A refresh failure must not turn missing authentication into a
				// transient activation failure that keeps Publish retrying.
				return errors.Join(failure, fmt.Errorf("refresh private preview denial: %w", err))
			}
			return failure
		}
		return &environments.OperationFailure{Code: "route_store_unavailable", Err: err}
	}
	if !decision.Accepted {
		return environments.ErrSuperseded
	}
	if err := c.publish(ctx, decision.TableVersion); err != nil {
		return err
	}
	want := serviceName(decision.EnvironmentID, op.Generation, op.Revision)
	return c.await(ctx, op, decision.EnvironmentID, op.Revision, func(o Observed) bool {
		return o.Present && o.Service == want && (o.Status == "enabled" || o.Status == "warning")
	})
}

// remove withdraws a preview's route: record the tombstone behind the fence,
// write the table, and wait until the router stops serving the host (it then
// falls through to the unavailable page).
func (c *Controller) remove(ctx context.Context, op environments.Operation) error {
	decision, err := c.fence.AcceptRouteRemoval(ctx, op)
	if err != nil {
		return &environments.OperationFailure{Code: "route_store_unavailable", Err: err}
	}
	if !decision.Accepted {
		return environments.ErrSuperseded
	}
	if decision.EnvironmentID == 0 {
		return nil // No environment, so nothing was ever routed.
	}
	if err := c.publish(ctx, decision.TableVersion); err != nil {
		return err
	}
	return c.await(ctx, op, decision.EnvironmentID, 0, func(o Observed) bool { return !o.Present })
}

// publish writes the accepted route table unless the file already holds a
// version at least as new, which then includes this change.
func (c *Controller) publish(ctx context.Context, atLeast int64) error {
	table, err := c.fence.RouteTable(ctx)
	if err != nil {
		return &environments.OperationFailure{Code: "route_store_unavailable", Err: err}
	}
	if table.Version < atLeast {
		return &environments.OperationFailure{Code: "route_store_unavailable", Err: errors.New("route table snapshot is older than an accepted version")}
	}
	content, err := Render(c.settings, table)
	if err != nil {
		return &environments.OperationFailure{Code: "route_config_invalid", Err: err}
	}
	if _, err := c.file.Write(table.Version, content); err != nil {
		return &environments.OperationFailure{Code: "route_config_unwritable", Err: err}
	}
	return nil
}

// await polls the router until converged reports the version is served, then
// records that. A newer accepted version ends the wait as superseded; the
// newer operation confirms its own version.
func (c *Controller) await(ctx context.Context, op environments.Operation, environmentID, revision int64, converged func(Observed) bool) error {
	deadline := time.Now().Add(c.Converge)
	var last Observed
	var lastErr error
	for {
		observed, err := c.router.Router(ctx, routerName(environmentID))
		if err == nil && converged(observed) {
			if _, err := c.fence.MarkRouteServed(ctx, environmentID, op.Generation, revision); err != nil {
				return &environments.OperationFailure{Code: "route_store_unavailable", Err: err}
			}
			return nil
		}
		last, lastErr = observed, err
		state, ok, err := c.fence.RouteState(ctx, op.PullRequestID)
		if err != nil {
			return &environments.OperationFailure{Code: "route_store_unavailable", Err: err}
		}
		if ok && compare(state.Generation, state.Revision, op.Generation, revision) > 0 {
			return environments.ErrSuperseded
		}
		if !time.Now().Before(deadline) {
			code := "route_not_converged"
			if lastErr != nil {
				code = "router_unavailable"
			}
			c.logger.WarnContext(ctx, "router did not load route version", "operation_id", op.ID, "pr_id", op.PullRequestID,
				"generation", op.Generation, "revision", revision, "router_status", last.Status, "router_errors", last.Errors, "error", lastErr)
			return &environments.OperationFailure{Code: code, Err: lastErr}
		}
		select {
		case <-ctx.Done():
			return &environments.OperationFailure{Code: "route_not_converged", Err: ctx.Err()}
		case <-time.After(c.Poll):
		}
	}
}

// Sync refreshes the accepted route table at startup and during recovery,
// including worker configuration changes at the same table version.
func (c *Controller) Sync(ctx context.Context) error {
	table, current, err := syncTable(ctx, c.fence, c.file, c.settings)
	if err != nil || !current {
		return err
	}
	// Recreating Traefik drops Docker attachments even when the route file is
	// current. Restore them from the accepted targets; never reactivate a route.
	seen := make(map[string]bool)
	var errs []error
	for _, route := range table.Routes {
		if route.NetworkID == "" || seen[route.NetworkID] {
			continue
		}
		seen[route.NetworkID] = true
		if err := c.router.Attach(ctx, route.NetworkID); err != nil {
			errs = append(errs, fmt.Errorf("restore router network for environment %d: %w", route.EnvironmentID, err))
		}
	}
	return errors.Join(errs...)
}

// RouteTables reads the accepted route table; *environments.Repository
// implements it.
type RouteTables interface {
	RouteTable(ctx context.Context) (environments.RouteTable, error)
}

// Sync refreshes the accepted route table and worker settings unless the file
// holds a newer table. It also sends unknown hosts to the unavailable page
// and makes Traefik obtain the wildcard certificate before any preview exists.
func Sync(ctx context.Context, tables RouteTables, file *File, settings Settings) error {
	_, _, err := syncTable(ctx, tables, file, settings)
	return err
}

func syncTable(ctx context.Context, tables RouteTables, file *File, settings Settings) (environments.RouteTable, bool, error) {
	current, err := file.Version()
	if err != nil {
		return environments.RouteTable{}, false, err
	}
	table, err := tables.RouteTable(ctx)
	if err != nil {
		return environments.RouteTable{}, false, fmt.Errorf("read accepted route table: %w", err)
	}
	if current > table.Version {
		return environments.RouteTable{}, false, nil
	}
	content, err := Render(settings, table)
	if err != nil {
		return environments.RouteTable{}, false, err
	}
	if _, err := file.Refresh(table.Version, content); err != nil {
		return environments.RouteTable{}, false, err
	}
	return table, true, nil
}

func compare(generation, revision, otherGeneration, otherRevision int64) int {
	switch {
	case generation < otherGeneration || (generation == otherGeneration && revision < otherRevision):
		return -1
	case generation == otherGeneration && revision == otherRevision:
		return 0
	default:
		return 1
	}
}

// SyncArgs asks a worker to rewrite the router's file from the accepted
// route table, after a change made outside a route operation (a
// repository's previews made private or public).
type SyncArgs struct {
	Version int64 `json:"version"`
}

func (SyncArgs) Kind() string { return "route_sync" }

// SyncQueue runs route syncs one at a time.
const SyncQueue = "route_sync"

// InsertOpts deduplicates a route version while allowing a newer version
// to queue during a running sync. Each sync writes the latest table.
func (SyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: SyncQueue, MaxAttempts: 10, UniqueOpts: river.UniqueOpts{
		ByArgs:  true,
		ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled, rivertype.JobStateRetryable},
	}}
}

// SyncWorker runs SyncArgs.
type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]
	Controller *Controller
}

func (w *SyncWorker) Work(ctx context.Context, _ *river.Job[SyncArgs]) error {
	return w.Controller.Sync(ctx)
}
