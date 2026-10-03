package environments_test

import (
	"context"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestPerKindTimeoutsAndRescue(t *testing.T) {
	f := newFixture(t)
	s, err := env.NewService(f.repo, newRuntime(), f.logger, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	build := &env.BuildWorker{Service: s, Broker: f.broker}
	teardown := &env.TeardownWorker{Service: s, Broker: f.broker}
	ping := &queue.PingWorker{Logger: f.logger}
	wake := &env.WakeWorker{Broker: f.broker}
	if wake.Timeout(nil) != 15*time.Second || queue.RescueAfter(wake.Timeout(nil)) != 30*time.Second {
		t.Fatal("incorrect coordination timeout/rescue")
	}
	if build.Timeout(nil) != 16*time.Minute || teardown.Timeout(nil) != 3*time.Minute || ping.Timeout(nil) != 5*time.Second {
		t.Fatal("incorrect per-kind timeouts")
	}
	if queue.RescueAfter(build.Timeout(nil)) != 16*time.Minute || queue.RescueAfter(ping.Timeout(nil)) != 30*time.Second {
		t.Fatal("incorrect rescue deadlines")
	}
	workers := river.NewWorkers()
	env.RegisterWorkers(workers, s, f.broker)
	river.AddWorker(workers, ping)
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers, Queues: map[string]river.QueueConfig{queue.PingQueue: {MaxWorkers: 1}, env.BuildQueue: {MaxWorkers: 1}, env.TeardownQueue: {MaxWorkers: 1}}, JobTimeout: queue.PingTimeout, RescueStuckJobsAfter: queue.RescueScanFloor})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate crashed jobs. Do not wait minutes in the test: backdate attempts.
	types := []struct {
		args river.JobArgs
		age  time.Duration
		want string
	}{
		{queue.PingArgs{PRKey: "rescue:ping"}, time.Minute, "discarded"},
		{env.BuildArgs(env.NewRequest(1, 1, "sha-A", "open")), time.Minute, "running"},
		{env.BuildArgs(env.NewRequest(2, 1, "sha-B", "open")), 17 * time.Minute, "discarded"},
		{env.TeardownArgs(env.NewRequest(3, 1, "sha-C", "closed")), 4 * time.Minute, "discarded"},
	}
	ids := make([]int64, len(types))
	for i, tt := range types {
		insert, err := client.Insert(t.Context(), tt.args, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = insert.Job.ID
		if _, err := f.pool.Exec(t.Context(), "UPDATE river_job SET state='running',attempt=1,max_attempts=1,attempted_at=$2 WHERE id=$1", ids[i], time.Now().Add(-tt.age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.StopAndCancel(ctx); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		matched := true
		for i, tt := range types {
			job, err := client.JobGet(t.Context(), ids[i])
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "running" && string(job.State) != "running" {
				t.Fatalf("legitimate slow build rescued: %s", job.State)
			}
			if string(job.State) != tt.want {
				matched = false
			}
		}
		if matched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired job kinds were not rescued")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
