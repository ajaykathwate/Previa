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
	"sync/atomic"
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
	// PrivatePreviews means the worker has an auth URL configured. A Reader
	// requires it on every live routing worker; it is not a reachability check.
	PrivatePreviews bool `json:"private_previews"`
	// Paused: the operator stopped new builds (BUILDS_PAUSED). A paused
	// worker reports Execution false; together, Paused is true only when no
	// live worker can build.
	Paused bool `json:"paused"`
	// Services are the managed services an execution worker offers, one entry
	// per kind ("postgres 17, 16"); none when no catalog is configured.
	Services []string `json:"services,omitempty"`
	// BuildTimeout is the overall operator deadline of execution workers.
	BuildTimeout time.Duration `json:"build_timeout,omitempty"`
	// Baselines: an execution worker stores repository test-data baselines
	// (it has the secret keys that encrypt them).
	Baselines bool `json:"baselines"`
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
	last   atomic.Int64 // Unix nanoseconds of the last recorded heartbeat
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
	duration := h.caps.BuildTimeout
	if duration <= 0 {
		duration = 12 * time.Minute
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO worker_capabilities (worker_id, execution, routing, reporting, housekeeping, discovery, paused, services, build_timeout_ns, private_previews, baselines)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (worker_id) DO UPDATE SET seen_at = clock_timestamp()`,
		h.id, h.caps.Execution, h.caps.Routing, h.caps.Reporting, h.caps.Housekeeping, h.caps.Discovery, h.caps.Paused, append([]string{}, h.caps.Services...), int64(duration), h.caps.PrivatePreviews, h.caps.Baselines); err != nil {
		return fmt.Errorf("record worker capabilities: %w", err)
	}
	if _, err := h.pool.Exec(ctx, "DELETE FROM worker_capabilities WHERE seen_at < clock_timestamp() - $1::bigint * interval '1 second'", int64(forget/time.Second)); err != nil {
		return fmt.Errorf("forget old workers: %w", err)
	}
	h.last.Store(time.Now().UnixNano())
	return nil
}

// Fresh fails when the server would no longer count this worker: its last
// heartbeat is older than Stale.
func (h *Heartbeat) Fresh() error {
	if age := time.Since(time.Unix(0, h.last.Load())); age > Stale {
		return fmt.Errorf("no heartbeat recorded for %s", age.Round(time.Second))
	}
	return nil
}

// Capabilities are what this worker announced.
func (h *Heartbeat) Capabilities() Capabilities { return h.caps }

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
	c, _, err := r.Live(ctx)
	return c, err
}

// Live is Current plus how many workers are live.
func (r *Reader) Live(ctx context.Context) (Capabilities, int64, error) {
	var c Capabilities
	var live int64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(bool_or(execution), false), COALESCE(bool_or(routing), false),
		COALESCE(bool_or(reporting), false), COALESCE(bool_or(housekeeping), false), COALESCE(bool_or(discovery), false),
		COALESCE(bool_or(paused), false), count(*), COALESCE(min(build_timeout_ns) FILTER (WHERE execution), 0),
		COALESCE(bool_and(private_previews) FILTER (WHERE routing), false),
		COALESCE((SELECT array_agg(DISTINCT s ORDER BY s) FROM worker_capabilities w, unnest(w.services) s
		          WHERE w.execution AND w.seen_at >= clock_timestamp() - $1::bigint * interval '1 second'), '{}'),
		COALESCE(bool_or(baselines) FILTER (WHERE execution), false)
		FROM worker_capabilities WHERE seen_at >= clock_timestamp() - $1::bigint * interval '1 second'`, int64(Stale/time.Second)).
		Scan(&c.Execution, &c.Routing, &c.Reporting, &c.Housekeeping, &c.Discovery, &c.Paused, &live, &c.BuildTimeout, &c.PrivatePreviews, &c.Services, &c.Baselines)
	if err != nil {
		return Capabilities{}, 0, fmt.Errorf("read worker capabilities: %w", err)
	}
	c.Paused = c.Paused && !c.Execution
	if len(c.Services) == 0 {
		c.Services = nil
	}
	return c, live, nil
}
