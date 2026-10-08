package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// EgressSource reads the egress proxy's forwarding counters: connections by
// outcome and bytes each way. The worker sets it; other processes export
// nothing.
type EgressSource func(ctx context.Context) (connections map[string]int64, up, down int64, err error)

// egressOutcomes bound the outcome label (egress.Outcomes).
var egressOutcomes = []string{"forwarded", "unapproved", "refused_address", "unresolvable", "dial_failed", "limit", "revoked", "byte_limit"}

var (
	egressConnections = prometheus.NewDesc("previa_egress_connections_total",
		"Outbound connections the egress proxy forwarded or refused, by outcome, since it started.", []string{"outcome"}, nil)
	egressBytes = prometheus.NewDesc("previa_egress_bytes_total",
		"Bytes forwarded to approved destinations (up) and back (down), since the egress proxy started.", []string{"direction"}, nil)
)

// egressCollector reads the counters at most every 15 seconds.
type egressCollector struct {
	mu     sync.Mutex
	source EgressSource
	last   time.Time
	conns  map[string]int64
	up     int64
	down   int64
}

var egress = &egressCollector{}

func init() { Registry.MustRegister(egress) }

// SetEgressSource makes this process export the egress proxy's counters.
func SetEgressSource(source EgressSource) {
	egress.mu.Lock()
	defer egress.mu.Unlock()
	egress.source, egress.last = source, time.Time{}
}

func (c *egressCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- egressConnections
	ch <- egressBytes
}

func (c *egressCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source == nil {
		return
	}
	if time.Since(c.last) >= 15*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conns, up, down, err := c.source(ctx)
		cancel()
		if err == nil {
			c.conns, c.up, c.down, c.last = conns, up, down, time.Now()
		}
	}
	if c.conns == nil {
		return
	}
	for _, outcome := range egressOutcomes {
		ch <- prometheus.MustNewConstMetric(egressConnections, prometheus.CounterValue, float64(c.conns[outcome]), outcome)
	}
	ch <- prometheus.MustNewConstMetric(egressBytes, prometheus.CounterValue, float64(c.up), "up")
	ch <- prometheus.MustNewConstMetric(egressBytes, prometheus.CounterValue, float64(c.down), "down")
}
