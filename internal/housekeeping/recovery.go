package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/environments"
)

// Scheduling recreates lost wake-ups for due and stuck build requests and for
// closed PRs whose teardown has not run; *environments.Broker implements it.
type Scheduling interface {
	RecoverScheduling(ctx context.Context, batchSize int32) (int, error)
}

// Operations recreates lost operation wake-ups and releases expired leases;
// *environments.Repository implements it.
type Operations interface {
	RecoverOperations(ctx context.Context, batchSize int32) (int, error)
}

// Sweeper starts requested teardowns (revocation) that have not started;
// *environments.TeardownSweeper implements it.
type Sweeper interface {
	RunOnce(ctx context.Context) (environments.TeardownSweep, error)
}

// RouteSync rewrites the router's file from the accepted route table when it
// drifted; *routing.Controller implements it.
type RouteSync interface {
	Sync(ctx context.Context) error
}

// Recovery resumes whatever a crash, a lost River job or exhausted retries
// left behind. Every step is bounded and safe to repeat; durable state, not
// process memory, says what is due. It never authorizes work: recovered build
// requests are only re-dispatched under the usual policy, generation and
// deadline checks.
type Recovery struct {
	scheduling Scheduling
	operations Operations
	sweeper    Sweeper
	routes     RouteSync // optional: nil when routing is not configured
	logger     *slog.Logger
	batch      int32
}

func NewRecovery(scheduling Scheduling, operations Operations, sweeper Sweeper, routes RouteSync, logger *slog.Logger, batch int32) (*Recovery, error) {
	if scheduling == nil || operations == nil || sweeper == nil || logger == nil || batch <= 0 {
		return nil, errors.New("recovery requires scheduling, operations, a teardown sweeper, a logger and a batch size")
	}
	return &Recovery{scheduling: scheduling, operations: operations, sweeper: sweeper, routes: routes, logger: logger, batch: batch}, nil
}

// RecoveryPass is what one pass recovered.
type RecoveryPass struct {
	Wakeups    int // build requests and closes given a new wake-up
	Operations int // operations given a new wake-up
	Teardowns  int // requested teardowns started
}

// RunOnce runs every step even when one fails, and reports all failures.
func (r *Recovery) RunOnce(ctx context.Context) (RecoveryPass, error) {
	var pass RecoveryPass
	var errs []error
	var err error
	if pass.Wakeups, err = r.scheduling.RecoverScheduling(ctx, r.batch); err != nil {
		errs = append(errs, fmt.Errorf("recover scheduling: %w", err))
	}
	if pass.Operations, err = r.operations.RecoverOperations(ctx, r.batch); err != nil {
		errs = append(errs, fmt.Errorf("recover operations: %w", err))
	}
	sweep, err := r.sweeper.RunOnce(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("teardown sweep: %w", err))
	}
	pass.Teardowns = sweep.Started
	if r.routes != nil {
		if err := r.routes.Sync(ctx); err != nil {
			errs = append(errs, fmt.Errorf("sync routes: %w", err))
		}
	}
	if pass.Wakeups > 0 || pass.Operations > 0 || pass.Teardowns > 0 {
		r.logger.InfoContext(ctx, "housekeeping recovery", "wakeups", pass.Wakeups, "operations", pass.Operations, "teardowns", pass.Teardowns)
	}
	return pass, errors.Join(errs...)
}
