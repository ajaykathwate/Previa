package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLabelsStayBounded(t *testing.T) {
	// The collectors are process-wide, so counts are compared before and
	// after: the test stays correct when it runs more than once (-count).
	counters := []struct {
		name   string
		metric prometheus.Collector
		want   float64
	}{
		{"unknown event", webhookDeliveries.WithLabelValues("other", "accepted"), 1},
		{"unknown outcome", webhookDeliveries.WithLabelValues("other", "other"), 1},
		{"unknown reason", ingressRejections.WithLabelValues("other"), 1},
		{"unknown method/status", httpRequests.WithLabelValues("unmatched", "other", "other"), 1},
		{"unknown job kind", jobs.WithLabelValues("other", "failed"), 1},
		{"unknown failure code", builds.WithLabelValues("failed", "other"), 1},
		{"unknown action", housekeeping.WithLabelValues("expiry", "other"), 3},
	}
	before := make([]float64, len(counters))
	for i, c := range counters {
		before[i] = testutil.ToFloat64(c.metric)
	}

	// Values an attacker or a PR could influence collapse to "other".
	WebhookDelivery("pull_request<script>", "accepted")
	WebhookDelivery("issue_comment", "weird")
	IngressRejected("/repositories/42")
	HTTPRequest("", "PROPFIND", 999, time.Millisecond, false)
	JobFinished("Build For PR 42!", "failed", time.Second)
	BuildFinished("failed", "https://evil.example/x", time.Second)
	Housekeeping("expiry", "deleted-everything", 3)
	GitHubResponse(200, "core\n", 10, true)

	for i, c := range counters {
		if got := testutil.ToFloat64(c.metric) - before[i]; got != c.want {
			t.Fatalf("%s not bounded: %v, want %v", c.name, got, c.want)
		}
	}
	// A gauge: set, not added to.
	if got := testutil.ToFloat64(githubRateRemaining.WithLabelValues("other")); got != 10 {
		t.Fatalf("unknown resource not bounded: %v", got)
	}
	// Succeeded builds carry no code at all.
	succeeded := builds.WithLabelValues("succeeded", "")
	prior := testutil.ToFloat64(succeeded)
	BuildFinished("succeeded", "anything", time.Second)
	if got := testutil.ToFloat64(succeeded) - prior; got != 1 {
		t.Fatalf("success carried a code: %v", got)
	}
}

func TestStateKeepsTheLastGoodSnapshotAndRefreshesLazily(t *testing.T) {
	calls, fail := 0, false
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_failures_total", Help: "x"})
	c := &stateCollector{interval: time.Hour, timeout: time.Second, failures: failures, refresh: func(_ context.Context, s *State) error {
		calls++
		if fail {
			return errors.New("database down")
		}
		s.QueueJobs("build", "available", int64(calls))
		s.Workers(1, map[string]bool{"execution": true, "made_up": true})
		return nil
	}}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	read := func() string {
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, f := range families {
			b.WriteString(f.String())
		}
		return b.String()
	}
	first := read()
	if !strings.Contains(first, `previa_queue_jobs`) || !strings.Contains(first, `value:"other"`) {
		t.Fatalf("snapshot: %s", first)
	}
	read()
	if calls != 1 {
		t.Fatalf("refreshed %d times within the interval", calls)
	}
	fail, c.last = true, time.Time{}
	if after := read(); !strings.Contains(after, "previa_queue_jobs") || testutil.ToFloat64(failures) != 1 {
		t.Fatalf("a failed refresh dropped the snapshot or was not counted: %s", after)
	}
}

func TestEgressCountersAreExportedWithBoundedLabels(t *testing.T) {
	SetEgressSource(func(context.Context) (map[string]int64, int64, int64, error) {
		return map[string]int64{"forwarded": 3, "refused_address": 2, "an attacker's label": 9}, 100, 200, nil
	})
	defer SetEgressSource(nil)
	families, err := Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			key := f.GetName()
			for _, l := range m.GetLabel() {
				key += "/" + l.GetValue()
			}
			found[key] = m.GetCounter().GetValue()
		}
	}
	if found["previa_egress_connections_total/forwarded"] != 3 || found["previa_egress_bytes_total/down"] != 200 {
		t.Fatalf("egress counters %v", found)
	}
	for key := range found {
		if strings.Contains(key, "attacker") {
			t.Fatalf("an unbounded label was exported: %s", key)
		}
	}
}
