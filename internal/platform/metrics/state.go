package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// State is a snapshot of durable state read from PostgreSQL at scrape time:
// queue depth and age, environments, operations, build requests and live
// workers. The composition root fills it from the features' own queries.
type State struct {
	queueJobs      *prometheus.GaugeVec
	queueOldest    *prometheus.GaugeVec
	environments   *prometheus.GaugeVec
	operations     *prometheus.GaugeVec
	requests       *prometheus.GaugeVec
	workersLive    prometheus.Gauge
	capability     *prometheus.GaugeVec
	backupLastSeen prometheus.Gauge
	backupKnown    bool
}

func newState() *State {
	return &State{
		queueJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_queue_jobs", Help: "River jobs by queue and state (available, scheduled, retryable, running).",
		}, []string{"queue", "state"}),
		queueOldest: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_queue_oldest_available_seconds", Help: "Age of the oldest job that is due but not started, by queue.",
		}, []string{"queue"}),
		environments: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_environments", Help: "Preview environments by status.",
		}, []string{"status"}),
		operations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_operations", Help: "Unfinished or failed durable operations (routes, reports, cleanup) by kind and state.",
		}, []string{"kind", "state"}),
		requests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_build_requests", Help: "Current build requests by state and waiting reason.",
		}, []string{"state", "waiting"}),
		workersLive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "previa_workers_live", Help: "Workers whose capability heartbeat is fresh.",
		}),
		capability: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "previa_worker_capability", Help: "1 when some live worker provides the capability.",
		}, []string{"capability"}),
		backupLastSeen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "previa_backup_last_success_timestamp_seconds", Help: "When the last database backup succeeded (Unix time).",
		}),
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// QueueJobs records n jobs of queue in state.
func (s *State) QueueJobs(queue, state string, n int64) {
	s.queueJobs.WithLabelValues(bounded(queue), bounded(state, "available", "scheduled", "retryable", "running")).Set(float64(n))
}

// QueueOldest records how long queue's oldest due job has waited.
func (s *State) QueueOldest(queue string, age time.Duration) {
	s.queueOldest.WithLabelValues(bounded(queue)).Set(max(0, age.Seconds()))
}

// Environments records n environments in status.
func (s *State) Environments(status string, n int64) {
	s.environments.WithLabelValues(bounded(status)).Set(float64(n))
}

// Operations records n durable operations of kind in state.
func (s *State) Operations(kind, state string, n int64) {
	s.operations.WithLabelValues(bounded(kind), bounded(state)).Set(float64(n))
}

// Requests records n build requests in state, waiting for reason (or "").
func (s *State) Requests(state, waiting string, n int64) {
	if waiting != "" {
		waiting = bounded(waiting, waitReasons...)
	}
	s.requests.WithLabelValues(bounded(state), waiting).Set(float64(n))
}

// Workers records the live workers and what they provide.
func (s *State) Workers(live int64, capabilities map[string]bool) {
	s.workersLive.Set(float64(live))
	for name, on := range capabilities {
		s.capability.WithLabelValues(bounded(name, "execution", "routing", "reporting", "housekeeping", "discovery", "paused")).Set(boolGauge(on))
	}
}

// BackupSucceeded records the last successful backup's time.
func (s *State) BackupSucceeded(at time.Time) {
	s.backupLastSeen.Set(float64(at.Unix()))
	s.backupKnown = true
}

func (s *State) collect(ch chan<- prometheus.Metric) {
	for _, c := range []prometheus.Collector{s.queueJobs, s.queueOldest, s.environments, s.operations, s.requests, s.workersLive, s.capability} {
		c.Collect(ch)
	}
	if s.backupKnown {
		s.backupLastSeen.Collect(ch)
	}
}

// stateCollector refreshes State at most every interval, bounded by timeout,
// and serves the last good snapshot when a refresh fails.
type stateCollector struct {
	refresh  func(context.Context, *State) error
	interval time.Duration
	timeout  time.Duration
	failures prometheus.Counter

	mu      sync.Mutex
	current *State
	last    time.Time
}

var (
	stateFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "previa_state_refresh_failures_total", Help: "Scrape-time state reads from PostgreSQL that failed.",
	})
	stateMu      sync.Mutex
	stateCurrent *stateCollector
)

func init() { Registry.MustRegister(stateFailures) }

// RegisterState adds the scrape-time state gauges to Registry, replacing any
// earlier registration. refresh fills a fresh State; a failed refresh keeps
// the previous snapshot and counts previa_state_refresh_failures_total.
func RegisterState(refresh func(context.Context, *State) error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCurrent != nil {
		Registry.Unregister(stateCurrent)
	}
	stateCurrent = &stateCollector{refresh: refresh, interval: 10 * time.Second, timeout: 3 * time.Second, failures: stateFailures}
	Registry.MustRegister(stateCurrent)
}

// Describe sends nothing: the collector is unchecked because its label
// values are only known after reading the database.
func (c *stateCollector) Describe(chan<- *prometheus.Desc) {}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || time.Since(c.last) >= c.interval {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		next := newState()
		if err := c.refresh(ctx, next); err != nil {
			c.failures.Inc()
		} else {
			c.current = next
		}
		cancel()
		c.last = time.Now()
	}
	if c.current != nil {
		c.current.collect(ch)
	}
}
