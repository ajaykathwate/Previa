package runtime

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/moby/moby/client"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type everyCrash struct{ crashes chan Crash }

func (e everyCrash) RecordCrash(_ context.Context, c Crash) (bool, error) {
	e.crashes <- c
	return true, nil
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// After startup a live preview's output is followed redacted, it sleeps and
// wakes healthy, and when it stops its last output reaches the build log.
func TestRealRuntimeFollowsSleepsWakesAndReportsStops(t *testing.T) {
	f := newRealFixture(t)
	target := f.target(9, false)
	r, logs := f.runtime(t, target, repoconfig.Config{HealthCheckPath: "/health"}, nil)
	req := request(9)
	const environmentID = 79
	artifact, err := r.Build(buildCtx(t), req, environments.Attempt{EnvironmentID: environmentID, LogEpoch: 1})
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// Following: Docker's timestamps, the startup log's redaction.
	if _, _, err := r.FollowApp(ctx, environmentID+1, artifact.ContainerID, "10"); err == nil {
		t.Fatal("another preview's container was followed")
	}
	stream, redactor, err := r.FollowApp(ctx, environmentID, artifact.ContainerID, "200")
	if err != nil {
		t.Fatal(err)
	}
	followed := &lockedBuffer{}
	go func() {
		w := redactor.Writer(followed)
		_, _ = io.Copy(w, stream)
		_ = w.Close()
	}()
	eventually(t, "followed output", func() bool { return strings.Contains(followed.String(), "TOKEN=present") })
	_ = stream.Close()
	out := followed.String()
	if strings.Contains(out, secretValue) || !strings.Contains(out, "TOKEN=present value=[redacted]") ||
		!regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\S+Z UID=`).MatchString(out) {
		t.Fatalf("followed output:\n%s", out)
	}

	// Sleeping stops the app; waking starts it and waits until it is ready.
	running := func() bool {
		inspected, err := f.docker.ContainerInspect(ctx, artifact.ContainerID, client.ContainerInspectOptions{})
		return err == nil && inspected.Container.State != nil && inspected.Container.State.Running
	}
	if err := r.SleepPreview(ctx, req.PullRequestID, req.Generation, req.Revision); err != nil || running() {
		t.Fatalf("sleep: running=%v %v", running(), err)
	}
	if err := r.WakePreview(ctx, req.PullRequestID, req.Generation, req.Revision); err != nil || !running() {
		t.Fatalf("wake: running=%v %v", running(), err)
	}

	// A stop is reported once, with the app's last output, redacted.
	crashes := everyCrash{crashes: make(chan Crash, 4)}
	report := &memorySink{}
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go r.WatchCrashes(watchCtx, crashes, CrashLogFunc(func(_ context.Context, env, generation, revision int64, redactor *secrets.Redactor) (LogSink, error) {
		if env != environmentID || generation != req.Generation || revision != req.Revision {
			t.Errorf("crash reported for %d g%d r%d", env, generation, revision)
		}
		return redactingSink{inner: report, redactor: redactor}, nil
	}))
	time.Sleep(time.Second) // let the event stream start
	if _, err := f.docker.ContainerKill(ctx, artifact.ContainerID, client.ContainerKillOptions{Signal: "KILL"}); err != nil {
		t.Fatal(err)
	}
	crash := <-crashes.crashes
	if crash.ContainerID != artifact.ContainerID || crash.ExitCode != 137 || crash.EventNanos <= 0 {
		t.Fatalf("crash %+v", crash)
	}
	eventually(t, "the stop report", func() bool { return strings.Contains(report.String(), "TOKEN=present") })
	got := report.String()
	if !strings.Contains(got, "The app stopped at") || !strings.Contains(got, "exit code 137") || !strings.Contains(got, "App output") ||
		strings.Contains(got, secretValue) {
		t.Fatalf("stop report:\n%s", got)
	}
}
