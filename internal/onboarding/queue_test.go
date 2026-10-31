package onboarding

import (
	"context"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// The real queue: the server's insert-only producer records the job with the
// request, and a River worker with only the setup check registered runs it.
func TestTheQueueRunsARequestedCheck(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	migrator, err := rivermigrate.New(riverpgxv5.New(f.pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	producer, err := queue.NewProducer(f.pool, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(f.pool, producer, f.platform, f.caps, f.secrets, Options{PreviewDomain: "preview.test", Discover: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil || outcome != CheckRequested {
		t.Fatalf("request: %s %v", outcome, err)
	}
	cfg := &river.Config{Logger: slogDiscard(), Workers: river.NewWorkers(), Queues: map[string]river.QueueConfig{}, FetchPollInterval: 200 * time.Millisecond}
	Register(cfg, runner(t, f, &fakeFiles{tree: readyTree()}))
	worker, err := river.NewClient(riverpgxv5.New(f.pool), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Stop(context.Background()) }()
	for f.check(t, f.pr).State != "complete" {
		select {
		case <-ctx.Done():
			t.Fatalf("the queued check never ran: %+v", f.check(t, f.pr))
		case <-time.After(100 * time.Millisecond):
		}
	}
	if result, ok := decodeResult(f.check(t, f.pr).Result); !ok || !result.Passed() {
		t.Fatalf("result: %+v", result)
	}
}
