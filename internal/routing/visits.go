package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
)

// VisitInterval is how often the router's request counters are read.
const VisitInterval = time.Minute

// VisitHealth remembers when the router's request counters were last read,
// so nothing that depends on visits (idle previews) acts on stale counts.
type VisitHealth struct{ last atomic.Int64 }

// Fresh reports whether the counters were read within the duration.
func (h *VisitHealth) Fresh(within time.Duration) bool {
	last := h.last.Load()
	return last > 0 && time.Since(time.Unix(0, last)) <= within
}

func (h *VisitHealth) read() { h.last.Store(time.Now().UnixNano()) }

// VisitRecorder adds requests to an environment; *environments.Repository
// implements it.
type VisitRecorder interface {
	RecordRequests(ctx context.Context, environmentID int64, service string, requests float64) (int64, error)
}

// sumRequests runs inside the router container: it totals Traefik's
// traefik_service_requests_total over codes, methods and protocols, printing
// one "service count" line per preview service, so the output stays small
// however many label combinations the router reports.
const sumRequests = `wget -q -T 5 -O - "http://$PREVIA_METRICS/metrics" | awk '
/^traefik_service_requests_total\{/ && match($0, /service="preview-[0-9]+-g[0-9]+-r[0-9]+@file"/) {
  n[substr($0, RSTART + 9, RLENGTH - 10)] += $NF
}
END { for (s in n) printf "%s %.0f\n", s, n[s] }'`

var previewService = regexp.MustCompile(`^preview-([0-9]+)-g[0-9]+-r[0-9]+@file$`)

// ServiceRequests reads how many requests the router has passed to each
// preview service, from its Prometheus metrics at metricsAddress (host:port
// inside the router container). Only counts are read; Previa never sees
// request details.
func (t *Traefik) ServiceRequests(ctx context.Context, metricsAddress string) (map[string]float64, error) {
	if host, port, err := net.SplitHostPort(metricsAddress); err != nil || host == "" || port == "" {
		return nil, errors.New("the router's metrics address must be host:port")
	}
	result, err := dockerx.RunWithEnv(ctx, t.docker, t.container, []string{"PREVIA_METRICS=" + metricsAddress}, "sh", "-c", sumRequests)
	if err != nil {
		return nil, fmt.Errorf("%w: read router metrics: %w", ErrRouterUnavailable, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("%w: read router metrics: exit code %d", ErrRouterUnavailable, result.ExitCode)
	}
	return parseServiceRequests(result.Stdout), nil
}

func parseServiceRequests(out string) map[string]float64 {
	counts := map[string]float64{}
	for line := range strings.SplitSeq(out, "\n") {
		service, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !previewService.MatchString(service) {
			continue
		}
		if n, err := strconv.ParseFloat(value, 64); err == nil && n >= 0 {
			counts[service] = n
		}
	}
	return counts
}

// WatchVisits records, every VisitInterval until ctx ends, how many requests
// each preview received and when (for "Last opened" and idle previews).
func WatchVisits(ctx context.Context, t *Traefik, metricsAddress string, recorder VisitRecorder, health *VisitHealth, logger *slog.Logger) {
	ticker := time.NewTicker(VisitInterval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := recordVisits(readCtx, t, metricsAddress, recorder)
		cancel()
		if err == nil {
			health.read()
		}
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			logger.WarnContext(ctx, "preview visits not read; retrying every minute", "error", err)
			failing = true
		case err == nil && failing:
			logger.InfoContext(ctx, "preview visits read again")
			failing = false
		}
	}
}

func recordVisits(ctx context.Context, t *Traefik, metricsAddress string, recorder VisitRecorder) error {
	counts, err := t.ServiceRequests(ctx, metricsAddress)
	if err != nil {
		return err
	}
	var errs []error
	for service, requests := range counts {
		match := previewService.FindStringSubmatch(service)
		environmentID, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			continue
		}
		name, _, _ := strings.Cut(service, "@")
		if _, err := recorder.RecordRequests(ctx, environmentID, name, requests); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
