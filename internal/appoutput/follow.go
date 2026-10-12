package appoutput

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/textlines"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// Queue runs followers; MaxFollowers bounds them on one worker.
	Queue        = "app_output"
	MaxFollowers = 16
	// maxFollow ends a follower even while watched; reloading resumes.
	maxFollow = 2 * time.Hour
	// tailLines is how much earlier output a new follower starts with.
	tailLines = "200"
	lineBytes = 8 << 10
)

// How often a follower stores lines and checks that someone still watches
// (variables so tests can shorten them).
var (
	flushEvery     = 500 * time.Millisecond
	heartbeatEvery = 10 * time.Second
)

// Notes viewers read when following stops.
const (
	noteNotRunning  = "The preview is not running, so there is no output to follow."
	noteUnreadable  = "The app's output could not be read. Reload to try again."
	noteEnded       = "The app's output ended: it stopped, restarted or was replaced by a newer version. Its last lines are in the build log; reload to follow it again."
	noteInterrupted = "Live output was interrupted. Reload to resume."
	noteMaxDuration = "Live output pauses after two hours. Reload to resume."
)

// Source follows a live preview's app container: its last tailLines lines
// and everything after, with Docker's timestamps, and the redactor for what
// it may print; *runtime.Runtime implements it.
type Source interface {
	FollowApp(ctx context.Context, environmentID int64, containerID, tail string) (io.ReadCloser, *secrets.Redactor, error)
}

// FollowArgs starts a follower for one preview.
type FollowArgs struct {
	EnvironmentID int64 `json:"environment_id"`
}

func (FollowArgs) Kind() string { return "app_output_follow" }

// InsertOpts keeps one waiting follower per preview. A running one does not
// block another: Begin lets only one follow, and a follower whose worker
// stopped is replaced without waiting for River to rescue its job.
func (FollowArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: Queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		},
	}}
}

// FollowWorker follows a preview's output into the store while someone
// watches it.
type FollowWorker struct {
	river.WorkerDefaults[FollowArgs]
	Store  *Store
	Source Source
	Logger *slog.Logger
}

func (w *FollowWorker) Timeout(*river.Job[FollowArgs]) time.Duration { return maxFollow + time.Minute }

func (w *FollowWorker) Work(ctx context.Context, job *river.Job[FollowArgs]) error {
	environmentID := job.Args.EnvironmentID
	epoch, began, err := w.Store.Begin(ctx, environmentID)
	if err != nil || !began {
		return err
	}
	stop := func(note string, forget bool) error {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return w.Store.Stop(stopCtx, environmentID, epoch, note, forget)
	}
	containerID, live, err := w.Store.LiveContainer(ctx, environmentID)
	if err != nil {
		return errors.Join(err, stop(noteUnreadable, false))
	}
	if !live {
		return stop(noteNotRunning, false)
	}
	stream, redactor, err := w.Source.FollowApp(ctx, environmentID, containerID, tailLines)
	if err != nil {
		w.Logger.WarnContext(ctx, "app output not followed", "environment_id", environmentID, "error", err)
		return stop(noteUnreadable, false)
	}
	note, forget := w.follow(ctx, environmentID, epoch, stream, redactor)
	return stop(note, forget)
}

// follow copies the stream into the store until it ends, nobody watches,
// another follower takes over or the time limit passes. It returns the
// note for viewers and whether to delete the lines.
func (w *FollowWorker) follow(ctx context.Context, environmentID, epoch int64, stream io.ReadCloser, redactor *secrets.Redactor) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, maxFollow)
	defer cancel()
	defer func() { _ = stream.Close() }()
	var mu sync.Mutex
	var pending []Line
	emit := func(text string) {
		at := time.Now()
		// Docker puts its RFC 3339 timestamp before each line.
		if stamp, rest, ok := strings.Cut(text, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				at, text = t, rest
			}
		}
		mu.Lock()
		pending = append(pending, Line{At: at, Text: text})
		if len(pending) > keepLines {
			pending = pending[len(pending)-keepLines:]
		}
		mu.Unlock()
	}
	splitter := &textlines.Splitter{MaxBytes: lineBytes, Emit: emit}
	redacting := redactor.Writer(splitter)
	input := &textlines.Stripper{Dst: redacting}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(input, stream)
		_ = redacting.Close()
		splitter.Flush()
		ended <- err
	}()
	flush := func() error {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		return w.Store.Append(ctx, environmentID, epoch, batch)
	}
	flushTicker, heartbeat := time.NewTicker(flushEvery), time.NewTicker(heartbeatEvery)
	defer flushTicker.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-ended:
			_ = flush()
			return noteEnded, false
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return noteMaxDuration, false
			}
			return noteInterrupted, false
		case <-flushTicker.C:
			if err := flush(); errors.Is(err, errFenced) {
				return "", false
			} else if err != nil {
				w.Logger.WarnContext(ctx, "app output lines not stored", "environment_id", environmentID, "error", err)
			}
		case <-heartbeat.C:
			watched, err := w.Store.Heartbeat(ctx, environmentID, epoch)
			switch {
			case errors.Is(err, errFenced):
				return "", false
			case err == nil && !watched:
				return "", true // Nobody watches: keep nothing.
			}
		}
	}
}
