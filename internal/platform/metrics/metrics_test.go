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
	// Values an attacker or a PR could influence collapse to "other".
	WebhookDelivery("pull_request<script>", "accepted")
	WebhookDelivery("issue_comment", "weird")
	IngressRejected("/repositories/42")
	HTTPRequest("", "PROPFIND", 999, time.Millisecond, false)
	JobFinished("Build For PR 42!", "failed", time.Second)
	BuildFinished("failed", "https://evil.example/x", time.Second)
	Housekeeping("expiry", "deleted-everything", 3)
	GitHubResponse(200, "core\n", 10, true)

	if got := testutil.ToFloat64(webhookDeliveries.WithLabelValues("other", "accepted")); got != 1 {
		t.Fatalf("unknown event not bounded: %v", got)
	}
	if got := testutil.ToFloat64(webhookDeliveries.WithLabelValues("other", "other")); got != 1 {
		t.Fatalf("unknown outcome not bounded: %v", got)
	}
	if got := testutil.ToFloat64(ingressRejections.WithLabelValues("other")); got != 1 {
		t.Fatalf("unknown reason not bounded: %v", got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("unmatched", "other", "other")); got != 1 {
		t.Fatalf("unknown method/status not bounded: %v", got)
	}
	if got := testutil.ToFloat64(jobs.WithLabelValues("other", "failed")); got != 1 {
		t.Fatalf("unknown job kind not bounded: %v", got)
	}
	if got := testutil.ToFloat64(builds.WithLabelValues("failed", "other")); got != 1 {
		t.Fatalf("unknown failure code not bounded: %v", got)
	}
	if got := testutil.ToFloat64(housekeeping.WithLabelValues("expiry", "other")); got != 3 {
		t.Fatalf("unknown action not bounded: %v", got)
	}
	if got := testutil.ToFloat64(githubRateRemaining.WithLabelValues("other")); got != 10 {
		t.Fatalf("unknown resource not bounded: %v", got)
	}
	// Succeeded builds carry no code at all.
	BuildFinished("succeeded", "anything", time.Second)
	if got := testutil.ToFloat64(builds.WithLabelValues("succeeded", "")); got != 1 {
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
