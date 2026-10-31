package metrics

import (
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every Previa metric plus the Go runtime and process
// collectors. It is served only on the internal listener.
var Registry = prometheus.NewRegistry()

// Label values are bounded: each comes from a fixed set or a code-defined
// identifier pattern, never from request data, PR IDs, URLs or secrets.
const other = "other"

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// waitReasons bound the waiting label (environments.Wait*).
var waitReasons = []string{"capacity", "disk", "paused", "account", "quota", "workspace_builds", "workspace_minutes", "workspace_daily", "workspace_previews", "workspace_restricted", "workspace_accounting"}

// bounded returns v when it is in allowed (or, with no allowed set, a short
// code-defined identifier), and "other" otherwise.
func bounded(v string, allowed ...string) string {
	if len(allowed) == 0 {
		if identifier.MatchString(v) {
			return v
		}
		return other
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return other
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return other
	}
	return strconv.Itoa(status/100) + "xx"
}

var (
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_http_requests_total", Help: "HTTP requests by route pattern, method and status class.",
	}, []string{"route", "method", "status"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "previa_http_request_duration_seconds", Help: "HTTP request duration by route pattern (event streams excluded).",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"route"})
	ingressRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_ingress_rejections_total", Help: "Requests refused by ingress controls.",
	}, []string{"reason"})
	sseStreams = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "previa_sse_streams", Help: "Open Server-Sent Event streams.",
	})
	webhookDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_webhook_deliveries_total", Help: "GitHub webhook deliveries by event and outcome.",
	}, []string{"event", "outcome"})

	jobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_jobs_total", Help: "Worked River jobs by kind and outcome.",
	}, []string{"kind", "outcome"})
	jobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "previa_job_duration_seconds", Help: "River job run time by kind.",
		Buckets: []float64{.01, .05, .1, .5, 1, 5, 15, 30, 60, 120, 300, 600, 1200},
	}, []string{"kind"})
	jobQueueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "previa_job_queue_wait_seconds", Help: "Time from a job becoming due to a worker starting it, by queue.",
		Buckets: []float64{.05, .1, .5, 1, 5, 15, 30, 60, 120, 300, 600, 1800},
	}, []string{"queue"})

	builds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_builds_total", Help: "Finished build attempts by outcome and failure code.",
	}, []string{"outcome", "code"})
	buildDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "previa_build_duration_seconds", Help: "Build attempt duration (source to healthy candidate) by outcome.",
		Buckets: []float64{5, 15, 30, 60, 120, 180, 300, 450, 600, 900, 1200},
	}, []string{"outcome"})
	buildWaits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_build_waits_total", Help: "Build admissions deferred, by reason.",
	}, []string{"reason"})
	previewLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "previa_request_to_preview_seconds", Help: "From a build request becoming eligible (after Automatic's quiet period) to its preview serving.",
		Buckets: []float64{15, 30, 60, 90, 120, 180, 300, 450, 600, 900, 1200, 1800, 3600},
	})
	buildTraffic = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "previa_build_egress_bytes", Help: "A finished build's traffic through the egress proxy, sent (up) and received (down).",
		Buckets: []float64{1 << 20, 10 << 20, 100 << 20, 256 << 20, 512 << 20, 1 << 30, 2 << 30, 4 << 30, 8 << 30},
	}, []string{"direction"})
	buildDiskFree = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "previa_build_disk_free_bytes", Help: "Free bytes on the Docker host storage at the last build's disk check.",
	})

	housekeeping = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_housekeeping_actions_total", Help: "Repairs and deletions made by housekeeping, by job and action.",
	}, []string{"job", "action"})

	githubRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previa_github_requests_total", Help: "GitHub API requests by status class (error: no response).",
	}, []string{"status"})
	githubRateRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "previa_github_rate_limit_remaining", Help: "Requests left in GitHub's current rate-limit window, by resource.",
	}, []string{"resource"})
	githubRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "previa_github_rate_limited_total", Help: "GitHub responses that reported a rate limit.",
	})
)

func init() {
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "previa_build_info", Help: "Always 1; labels name the running release and Go version.",
	}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(version.Version, version.GoVersion()).Set(1)
	Registry.MustRegister(buildInfo)
	Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		httpRequests, httpDuration, ingressRejections, sseStreams, webhookDeliveries,
		jobs, jobDuration, jobQueueWait, builds, buildDuration, buildWaits, buildTraffic, buildDiskFree, previewLatency,
		housekeeping, githubRequests, githubRateRemaining, githubRateLimited,
	)
}

// Handler serves the registry in the Prometheus exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry, MaxRequestsInFlight: 4, Timeout: 10 * time.Second})
}

// HTTPRequest records a served request. route is the router's pattern
// (for example /repositories/{id}), never the raw path.
func HTTPRequest(route, method string, status int, d time.Duration, stream bool) {
	if route == "" {
		route = "unmatched"
	}
	method = bounded(method, http.MethodGet, http.MethodPost, http.MethodHead)
	httpRequests.WithLabelValues(route, method, statusClass(status)).Inc()
	if !stream {
		httpDuration.WithLabelValues(route).Observe(d.Seconds())
	}
}

// IngressRejected counts a request refused before reaching a handler.
func IngressRejected(reason string) {
	ingressRejections.WithLabelValues(bounded(reason, "rate_limited", "wrong_host", "forwarded_host", "body_too_large", "overloaded")).Inc()
}

// StreamOpened counts an open event stream until the returned func is called.
func StreamOpened() (closed func()) {
	sseStreams.Inc()
	return func() { sseStreams.Dec() }
}

// WebhookDelivery counts one webhook request.
func WebhookDelivery(event, outcome string) {
	webhookDeliveries.WithLabelValues(
		bounded(event, "pull_request", "installation", "installation_repositories", "ping"),
		bounded(outcome, "accepted", "ignored", "invalid_signature", "invalid", "conflict", "failed", "too_large", "overloaded", "disabled"),
	).Inc()
}

// JobStarted records how long a job waited past its due time.
func JobStarted(queue string, wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	jobQueueWait.WithLabelValues(bounded(queue)).Observe(wait.Seconds())
}

// JobFinished records a worked job: outcome is succeeded, failed, snoozed or cancelled.
func JobFinished(kind, outcome string, d time.Duration) {
	kind = bounded(kind)
	jobs.WithLabelValues(kind, bounded(outcome, "succeeded", "failed", "snoozed", "cancelled")).Inc()
	jobDuration.WithLabelValues(kind).Observe(d.Seconds())
}

// BuildFinished records a build attempt that reached a result: outcome is
// succeeded or failed (code is the safe failure category), or superseded.
func BuildFinished(outcome, code string, d time.Duration) {
	outcome = bounded(outcome, "succeeded", "failed", "superseded", "interrupted")
	if outcome != "failed" {
		code = ""
	} else {
		code = bounded(code)
	}
	builds.WithLabelValues(outcome, code).Inc()
	buildDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

// BuildWaited counts an admission deferred for capacity, disk, a pause, the
// account's other build or its daily build minutes.
func BuildWaited(reason string) {
	buildWaits.WithLabelValues(bounded(reason, waitReasons...)).Inc()
}

// PreviewPublished records the headline latency: from a request becoming
// eligible to its preview serving.
func PreviewPublished(d time.Duration) { previewLatency.Observe(max(0, d.Seconds())) }

// BuildTraffic records one build's bytes sent and received through the
// egress proxy.
func BuildTraffic(up, down int64) {
	buildTraffic.WithLabelValues("up").Observe(float64(max(up, 0)))
	buildTraffic.WithLabelValues("down").Observe(float64(max(down, 0)))
}

// BuildDiskFree records the free bytes measured before a build.
func BuildDiskFree(bytes int64) { buildDiskFree.Set(float64(bytes)) }

// Housekeeping counts n actions of one housekeeping job.
func Housekeeping(job, action string, n int) {
	if n <= 0 {
		return
	}
	housekeeping.WithLabelValues(
		bounded(job, "recovery", "resources", "pull_requests", "expiry", "prune"),
		bounded(action, "wakeups", "operations", "teardowns", "orphans", "unknown_removed", "missing", "confirmed", "expired", "images", "workspaces", "caches"),
	).Add(float64(n))
}

// GitHubResponse records one GitHub API response and its rate-limit headers.
func GitHubResponse(status int, resource string, remaining int, known bool) {
	githubRequests.WithLabelValues(statusClass(status)).Inc()
	if known {
		githubRateRemaining.WithLabelValues(bounded(resource, "core", "search", "graphql", "integration_manifest", "code_scanning_upload")).Set(float64(remaining))
	}
	if status == http.StatusTooManyRequests || (status == http.StatusForbidden && known && remaining == 0) {
		githubRateLimited.Inc()
	}
}

// GitHubError records a GitHub request that got no response.
func GitHubError() { githubRequests.WithLabelValues("error").Inc() }
