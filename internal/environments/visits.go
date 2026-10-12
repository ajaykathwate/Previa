package environments

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
)

// RecordRequests adds the requests the router passed to an environment's
// preview since the last read, and notes the visit time when there were
// any. requests is the router's counter for service: a new service (a newer
// version of the preview) or a smaller counter (the router restarted) counts
// from zero. Workers reading the same router concurrently count each request
// once. It returns how many requests were added.
func (r *Repository) RecordRequests(ctx context.Context, environmentID int64, service string, requests float64) (int64, error) {
	if environmentID <= 0 || service == "" || requests < 0 || math.IsNaN(requests) || math.IsInf(requests, 0) {
		return 0, errors.New("invalid request count")
	}
	var added int64
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.StartVisitCounter(ctx, dbgen.StartVisitCounterParams{EnvironmentID: environmentID, Service: service}); err != nil {
			return err
		}
		last, err := q.LockVisitCounter(ctx, environmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // The environment is gone.
		}
		if err != nil {
			return err
		}
		switch {
		case last.Service != service || requests < last.Requests:
			added = int64(requests)
		default:
			added = int64(requests) - int64(last.Requests)
		}
		if err := q.SaveVisitCounter(ctx, dbgen.SaveVisitCounterParams{EnvironmentID: environmentID, Service: service, Requests: requests}); err != nil {
			return err
		}
		if added <= 0 {
			return nil
		}
		return q.AddVisits(ctx, dbgen.AddVisitsParams{EnvironmentID: environmentID, Requests: added})
	})
	if err != nil {
		return 0, fmt.Errorf("record preview requests: %w", err)
	}
	return added, nil
}
