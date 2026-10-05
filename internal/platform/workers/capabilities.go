// Package workers records what each running worker process can do, so the
// server can say truthfully whether builds can run (migration 0017).
package workers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Capabilities are what a worker has real, validated dependencies for.
type Capabilities struct {
	Execution    bool `json:"execution"`    // builds, previews and their publication
	Routing      bool `json:"routing"`      // the route controller (part of execution)
	Reporting    bool `json:"reporting"`    // PR comments and check runs
	Housekeeping bool `json:"housekeeping"` // recovery, expiry, cleanup, pruning
	Discovery    bool `json:"discovery"`    // PR discovery and mergeability
}

const (
	// Interval between heartbeats; a worker silent for Stale is gone.
	Interval = 30 * time.Second
	Stale    = 3 * Interval
	forget   = 24 * time.Hour
)

// Heartbeat keeps one worker's row fresh until stopped.
type Heartbeat struct {
	pool   *pgxpool.Pool
	id     string
	caps   Capabilities
	logger *slog.Logger
	done   chan struct{}
	exited chan struct{}
}

// Start records caps for this process and refreshes them every Interval.
func Start(ctx context.Context, pool *pgxpool.Pool, caps Capabilities, logger *slog.Logger) (*Heartbeat, error) {
	if pool == nil || logger == nil {
		return nil, errors.New("worker heartbeat requires a database and a logger")
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("create worker identity: %w", err)
	}
	h := &Heartbeat{pool: pool, id: hex.EncodeToString(raw), caps: caps, logger: logger, done: make(chan struct{}), exited: make(chan struct{})}
	if err := h.beat(ctx); err != nil {
		return nil, err
	}
	go h.run(context.WithoutCancel(ctx))
	return h, nil
}

func (h *Heartbeat) beat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, `INSERT INTO worker_capabilities (worker_id, execution, routing, reporting, housekeeping, discovery)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (worker_id) DO UPDATE SET seen_at = clock_timestamp()`,
		h.id, h.caps.Execution, h.caps.Routing, h.caps.Reporting, h.caps.Housekeeping, h.caps.Discovery); err != nil {
		return fmt.Errorf("record worker capabilities: %w", err)
	}
	if _, err := h.pool.Exec(ctx, "DELETE FROM worker_capabilities WHERE seen_at < clock_timestamp() - $1::bigint * interval '1 second'", int64(forget/time.Second)); err != nil {
		return fmt.Errorf("forget old workers: %w", err)
	}
	return nil
}

func (h *Heartbeat) run(ctx context.Context) {
	defer close(h.exited)
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			if err := h.beat(ctx); err != nil {
				h.logger.WarnContext(ctx, "worker heartbeat failed", "error", err)
			}
		}
	}
}

// Stop ends the heartbeat and removes this worker's row, so the server stops
// counting on it at once.
func (h *Heartbeat) Stop(ctx context.Context) error {
	close(h.done)
	<-h.exited
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx, "DELETE FROM worker_capabilities WHERE worker_id = $1", h.id); err != nil {
		return fmt.Errorf("remove worker capabilities: %w", err)
	}
	return nil
}

// Reader reports what the running workers can do together.
type Reader struct{ pool *pgxpool.Pool }

func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

// Current combines the capabilities of every worker seen within Stale.
func (r *Reader) Current(ctx context.Context) (Capabilities, error) {
	var c Capabilities
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(bool_or(execution), false), COALESCE(bool_or(routing), false),
		COALESCE(bool_or(reporting), false), COALESCE(bool_or(housekeeping), false), COALESCE(bool_or(discovery), false)
		FROM worker_capabilities WHERE seen_at >= clock_timestamp() - $1::bigint * interval '1 second'`, int64(Stale/time.Second)).
		Scan(&c.Execution, &c.Routing, &c.Reporting, &c.Housekeeping, &c.Discovery)
	if err != nil {
		return Capabilities{}, fmt.Errorf("read worker capabilities: %w", err)
	}
	return c, nil
}
