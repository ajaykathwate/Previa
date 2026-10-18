package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrSuperseded reports that newer work replaced an operation before its
// external effect: a newer route version, request or teardown. The operation
// worker records the operation as superseded rather than failed.
var ErrSuperseded = errors.New("environment operation superseded by newer work")

// RouteTarget is a healthy candidate the router should send a preview's
// traffic to: its artifact, the container name the router dials on the PR's
// network, and its port.
type RouteTarget struct {
	Artifact  Artifact
	Backend   string
	Port      int
	NetworkID string
	// AuthConfigured lets the publisher serve private requests. Checked against
	// the repository setting under its lock before accepting a new target.
	AuthConfigured bool
}

// RouteDecision is the route controller's durable answer to one operation.
// Accepted is true when the operation's version is the accepted route, now or
// from an earlier attempt; TableVersion is the first route table version that
// includes it.
type RouteDecision struct {
	Accepted      bool
	EnvironmentID int64
	TableVersion  int64
}

// Route is one active route in a route table snapshot.
type Route struct {
	EnvironmentID int64
	Hostname      string // DNS label below the preview base domain
	Generation    int64
	Revision      int64
	Backend       string
	Port          int
	NetworkID     string // persisted attachment restored after the router is recreated
	// Asleep: its app is stopped while idle; visitors go to the wake page.
	Asleep bool
	// Private: only signed-in GitHub users who can see the repository may
	// open it.
	Private bool
}

// RouteTable is every active route at one version. Versions follow commit
// order, so a later snapshot always includes every earlier acceptance.
type RouteTable struct {
	Version int64
	Routes  []Route
}

// RouteState is the controller's current record for one PR's environment.
type RouteState struct {
	EnvironmentID      int64
	Generation         int64
	Revision           int64
	Active             bool
	ServedGeneration   int64
	ServedRevision     int64
	Served             bool
	TableVersion       int64
	Target, ServedWith Artifact
}

// compareVersion orders route versions by (generation, revision).
func compareVersion(generation, revision, otherGeneration, otherRevision int64) int {
	switch {
	case generation < otherGeneration || (generation == otherGeneration && revision < otherRevision):
		return -1
	case generation == otherGeneration && revision == otherRevision:
		return 0
	default:
		return 1
	}
}

// AcceptRoute is the activation fence. In one transaction it checks that the
// PR still wants exactly this request (open, current generation and commit,
// running or succeeded intent, access intact) and that the candidate is the
// environment's pending artifact, then moves the route forward to it. A route
// version never moves backwards, so a late activation can never replace a
// newer route or resurrect a removed one. No external call happens here.
func (r *Repository) AcceptRoute(ctx context.Context, op Operation, target RouteTarget) (RouteDecision, error) {
	if op.Kind != ActivateRoute || !target.complete() || target.Artifact != op.Payload.Artifact {
		return RouteDecision{}, errors.New("invalid route activation")
	}
	request := op.Payload.Request
	var decision RouteDecision
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		decision = RouteDecision{}
		q := dbgen.New(tx)
		scope, err := lockOwnerIntentScope(ctx, q, op.PullRequestID, "route")
		if errors.Is(err, ErrIntentUnavailable) {
			return nil // Access was revoked; revocation tears the preview down.
		}
		if err != nil {
			return err
		}
		if !scope.publishes(request) {
			return nil
		}
		if allowed, err := requestPublishable(ctx, q, request); err != nil || !allowed {
			return err
		}
		env, err := q.LockRouteEnvironment(ctx, op.PullRequestID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock environment for route activation: %w", err)
		}
		if ready, err := routeCandidate(scope, env, request, target); err != nil || !ready {
			return err
		}
		decision.EnvironmentID = env.ID
		route, err := q.LockRoute(ctx, env.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("lock route: %w", err)
		default:
			switch compareVersion(route.Generation, route.Revision, request.Generation, request.Revision) {
			case 1:
				return nil
			case 0:
				// A retry after a crash: accepted already, unless another target
				// somehow holds this version, which is never this operation's.
				decision.Accepted = route.State == "active" && route.ContainerID.String == target.Artifact.ContainerID
				decision.TableVersion = route.TableVersion
				return nil
			}
		}
		if err := r.publicationUsageTx(ctx, tx, request.PullRequestID); err != nil {
			return err
		}
		version, err := saveActiveRoute(ctx, q, op, env.ID, target)
		if err != nil {
			return err
		}
		decision.Accepted, decision.TableVersion = true, version
		return nil
	})
	if err != nil {
		return RouteDecision{}, err
	}
	return decision, nil
}

func requestPublishable(ctx context.Context, q *dbgen.Queries, request Request) (bool, error) {
	intent, err := readLockedIntent(ctx, q, request.PullRequestID)
	if err != nil {
		return false, err
	}
	if !intent.publishes(request) {
		return false, nil
	}
	return publicationAuthorized(ctx, q, request.PullRequestID)
}

func publicationAuthorized(ctx context.Context, q *dbgen.Queries, pr int64) (bool, error) {
	allowed, err := q.WorkspacePublicationAuthorized(ctx, pr)
	if err != nil {
		return false, fmt.Errorf("check workspace publication authority: %w", err)
	}
	return allowed, nil
}

// complete reports whether the target names a candidate and where it listens.
func (t RouteTarget) complete() bool {
	return t.Artifact.ContainerID != "" && t.Artifact.ImageTag != "" && t.Backend != "" &&
		t.Port > 0 && t.Port <= 65535 && t.NetworkID != ""
}

// publishes reports whether the locked PR still wants the request published:
// open, at its generation and commit, and not blocked from building.
func (s intentScope) publishes(request Request) bool {
	return s.pr.Generation == request.Generation && s.pr.HeadSha == request.HeadSHA && s.pr.State == StateOpen &&
		BuildBlockReason(s.pr.BuildStatus, s.pr.NeedsRefresh) == nil
}

// publishes reports whether the intent is the request's and its build is
// running or succeeded.
func (i *BuildIntent) publishes(request Request) bool {
	return i != nil && i.Generation == request.Generation && i.Revision == request.Revision &&
		i.HeadSHA == request.HeadSHA && (i.State == IntentRunning || i.State == IntentSucceeded)
}

// pendingCandidate reports whether the environment's pending candidate is the
// request's artifact.
func pendingCandidate(env dbgen.LockRouteEnvironmentRow, request Request, artifact Artifact) bool {
	return env.PendingGeneration.Valid && env.PendingGeneration.Int64 == request.Generation &&
		env.PendingRevision.Valid && env.PendingRevision.Int64 == request.Revision &&
		env.PendingContainerID.String == artifact.ContainerID && env.PendingImageTag.String == artifact.ImageTag
}

func routeCandidate(scope intentScope, env dbgen.LockRouteEnvironmentRow, request Request, target RouteTarget) (bool, error) {
	if !pendingCandidate(env, request, target.Artifact) {
		return false, nil
	}
	if scope.private && !target.AuthConfigured {
		return false, &OperationFailure{Code: "private_preview_unconfigured", Permanent: true}
	}
	return true, nil
}

// saveActiveRoute moves the PR's route forward to the target under a new
// route table version, which it returns.
func saveActiveRoute(ctx context.Context, q *dbgen.Queries, op Operation, environmentID int64, target RouteTarget) (int64, error) {
	request := op.Payload.Request
	version, err := q.NextRouteTableVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("advance route table version: %w", err)
	}
	affected, err := q.SaveActiveRoute(ctx, dbgen.SaveActiveRouteParams{
		EnvironmentID: environmentID, PullRequestID: op.PullRequestID, Generation: request.Generation, Revision: request.Revision,
		ContainerID: text(target.Artifact.ContainerID), ImageTag: text(target.Artifact.ImageTag), Backend: text(target.Backend),
		Port: pgtype.Int4{Int32: int32(target.Port), Valid: true}, NetworkID: text(target.NetworkID),
		TableVersion: version, OperationID: pgtype.Int8{Int64: op.ID, Valid: op.ID > 0},
	})
	if err != nil {
		return 0, fmt.Errorf("save accepted route: %w", err)
	}
	if affected != 1 {
		return 0, errors.New("accepted route did not move forward")
	}
	if err := q.WakeForNewRoute(ctx, environmentID); err != nil {
		return 0, fmt.Errorf("wake for the accepted route: %w", err)
	}
	return version, nil
}

// AcceptRouteRemoval is the removal fence. It records a removal tombstone at
// the teardown's generation unless a newer route version (an activation after
// a reopen) was already accepted. Removing strictly older content is always
// safe, so it needs neither GitHub access nor the PR to still be at that
// generation: a teardown that started still finishes after a quick reopen.
func (r *Repository) AcceptRouteRemoval(ctx context.Context, op Operation) (RouteDecision, error) {
	if op.Kind != RemoveRoute || op.Revision != 0 {
		return RouteDecision{}, errors.New("invalid route removal")
	}
	var decision RouteDecision
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		decision = RouteDecision{}
		q := dbgen.New(tx)
		if _, err := lockTeardownScope(ctx, q, op.PullRequestID); err != nil {
			return err
		}
		env, err := q.LockRouteEnvironment(ctx, op.PullRequestID)
		if errors.Is(err, pgx.ErrNoRows) {
			// No environment ever existed, so there is no route to remove.
			decision.Accepted = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock environment for route removal: %w", err)
		}
		decision.EnvironmentID = env.ID
		route, err := q.LockRoute(ctx, env.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("lock route: %w", err)
		default:
			switch compareVersion(route.Generation, route.Revision, op.Generation, 0) {
			case 1:
				return nil
			case 0:
				decision.Accepted = route.State == "removed"
				decision.TableVersion = route.TableVersion
				return nil
			}
		}
		version, err := q.NextRouteTableVersion(ctx)
		if err != nil {
			return fmt.Errorf("advance route table version: %w", err)
		}
		affected, err := q.SaveRemovedRoute(ctx, dbgen.SaveRemovedRouteParams{
			EnvironmentID: env.ID, PullRequestID: op.PullRequestID, Generation: op.Generation,
			TableVersion: version, OperationID: pgtype.Int8{Int64: op.ID, Valid: op.ID > 0},
		})
		if err != nil {
			return fmt.Errorf("save route removal: %w", err)
		}
		if affected != 1 {
			return errors.New("route removal did not move forward")
		}
		decision.Accepted, decision.TableVersion = true, version
		return nil
	})
	if err != nil {
		return RouteDecision{}, err
	}
	return decision, nil
}

// MarkRouteServed records that the router loaded the accepted version. It
// reports false, changing nothing, when a newer version was accepted since.
// Until then cleanup keeps the previously served target as well.
func (r *Repository) MarkRouteServed(ctx context.Context, environmentID, generation, revision int64) (bool, error) {
	affected, err := dbgen.New(r.pool).MarkRouteServed(ctx, dbgen.MarkRouteServedParams{
		EnvironmentID: environmentID, Generation: generation, Revision: revision,
	})
	if err != nil {
		return false, fmt.Errorf("mark route served: %w", err)
	}
	return affected == 1, nil
}

// RouteTable reads every active route and the table version in one snapshot.
func (r *Repository) RouteTable(ctx context.Context) (RouteTable, error) {
	rows, err := dbgen.New(r.pool).ReadRouteTable(ctx)
	if err != nil {
		return RouteTable{}, fmt.Errorf("read route table: %w", err)
	}
	var table RouteTable
	for _, row := range rows {
		table.Version = row.TableVersion
		if !row.EnvironmentID.Valid {
			continue
		}
		table.Routes = append(table.Routes, Route{
			EnvironmentID: row.EnvironmentID.Int64, Hostname: row.Subdomain.String,
			Generation: row.Generation.Int64, Revision: row.Revision.Int64,
			Backend: row.Backend.String, Port: int(row.Port.Int32), NetworkID: row.NetworkID.String, Asleep: row.Asleep, Private: row.Private,
		})
	}
	return table, nil
}

// RouteState returns the PR's route record; ok is false when none exists.
func (r *Repository) RouteState(ctx context.Context, pullRequestID int64) (RouteState, bool, error) {
	row, err := dbgen.New(r.pool).ReadRoute(ctx, pullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RouteState{}, false, nil
	}
	if err != nil {
		return RouteState{}, false, fmt.Errorf("read route: %w", err)
	}
	return RouteState{
		EnvironmentID: row.EnvironmentID, Generation: row.Generation, Revision: row.Revision,
		Active: row.State == "active", TableVersion: row.TableVersion,
		ServedGeneration: row.ServedGeneration.Int64, ServedRevision: row.ServedRevision.Int64, Served: row.ServedGeneration.Valid,
		Target:     Artifact{ContainerID: row.ContainerID.String, ImageTag: row.ImageTag.String},
		ServedWith: Artifact{ContainerID: row.ServedContainerID.String, ImageTag: row.ServedImageTag.String},
	}, true, nil
}

// listRouteArtifacts returns what the router serves or may still serve.
func listRouteArtifacts(ctx context.Context, q *dbgen.Queries, pullRequestID int64) ([]Artifact, error) {
	rows, err := q.ListRouteTargets(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("list routed artifacts: %w", err)
	}
	var routed []Artifact
	for _, row := range rows {
		var unconfirmed []Artifact
		if err := json.Unmarshal(row.UnconfirmedTargets, &unconfirmed); err != nil {
			return nil, fmt.Errorf("decode unconfirmed route targets: %w", err)
		}
		routed = append(routed,
			Artifact{ContainerID: row.ContainerID.String, ImageTag: row.ImageTag.String},
			Artifact{ContainerID: row.ServedContainerID.String, ImageTag: row.ServedImageTag.String})
		routed = append(routed, unconfirmed...)
	}
	return routed, nil
}

func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
