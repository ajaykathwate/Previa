package queue

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func jobCount(t *testing.T, kind, outcome string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "previa_jobs_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["kind"] == kind && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestStatsReadsQueueDepthAndJobsAreMeasured runs River's real table: a due
// job counts as available with its age, and working it is measured.
func TestStatsReadsQueueDepthAndJobsAreMeasured(t *testing.T) {
	pool, logger := queuePool(t)
	producer, err := NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("queue-stats:%d", time.Now().UnixNano())
	cleanKeys(t, pool, key)
	id, _, err := NewService(pool, producer).EnqueuePing(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Stats(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range stats {
		if s.Queue == PingQueue && s.State == "available" && s.Jobs >= 1 && s.OldestDue >= 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the due ping is not counted: %+v", stats)
	}
	before := jobCount(t, "ping", "succeeded")
	worker, err := NewWorker(pool, config.WorkerConfig{MaxConcurrentBuilds: 1}, logger)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := worker.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	startWorker(t, worker)
	waitCompletion(t, events, id)
	if after := jobCount(t, "ping", "succeeded"); after < before+1 {
		t.Fatalf("the worked job was not measured: %v -> %v", before, after)
	}
}

func TestJobOutcomes(t *testing.T) {
	for err, want := range map[error]string{
		nil:                               "succeeded",
		river.JobSnooze(time.Second):      "snoozed",
		river.JobCancel(errors.New("no")): "cancelled",
		errors.New("boom"):                "failed",
		fmt.Errorf("wrapped: %w", &rivertype.JobSnoozeError{}): "snoozed",
	} {
		if got := jobOutcome(err); got != want {
			t.Fatalf("%v: %s, want %s", err, got, want)
		}
	}
}
