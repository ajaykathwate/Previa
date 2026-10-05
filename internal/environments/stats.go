package environments

import (
	"context"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
)

// Stats summarizes environments, unfinished operations and current build
// requests for scrape-time metrics. It holds counts only.
type Stats struct {
	Environments map[string]int64 // by status
	Operations   []OperationCount
	Requests     []RequestCount
}

// OperationCount is the number of operations of one kind in one state.
type OperationCount struct {
	Kind, State string
	Count       int64
}

// RequestCount is the number of current build requests in one state,
// waiting for one reason ("" when not waiting).
type RequestCount struct {
	State, Waiting string
	Count          int64
}

// Stats reads the counts in one short read.
func (r *Repository) Stats(ctx context.Context) (Stats, error) {
	q := dbgen.New(r.pool)
	statuses, err := q.EnvironmentStatusCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("count environments: %w", err)
	}
	operations, err := q.OperationStateCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("count operations: %w", err)
	}
	requests, err := q.BuildRequestCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("count build requests: %w", err)
	}
	s := Stats{Environments: make(map[string]int64, len(statuses))}
	for _, row := range statuses {
		s.Environments[row.Status] = row.Environments
	}
	for _, row := range operations {
		s.Operations = append(s.Operations, OperationCount{Kind: row.Kind, State: row.State, Count: row.Operations})
	}
	for _, row := range requests {
		s.Requests = append(s.Requests, RequestCount{State: row.State, Waiting: row.Waiting, Count: row.Requests})
	}
	return s, nil
}
