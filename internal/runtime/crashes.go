package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/runtime/dbgen"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// Crash is one stop of a live preview's container.
type Crash struct {
	ContainerID                         string
	EventNanos                          int64
	EnvironmentID, Generation, Revision int64
	ExitCode                            int
}

// CrashRecorder records a crash once; *TargetStore implements it. It reports
// false for a container the environment no longer serves, a preview being
// torn down, or a crash already recorded (by another worker).
type CrashRecorder interface {
	RecordCrash(ctx context.Context, c Crash) (bool, error)
}

// CrashLog opens the build log of the attempt that built a live preview, to
// add its output after a crash; *buildlogs.Store implements it (as an
// adapter in the app package).
type CrashLog interface {
	OpenFinished(ctx context.Context, environmentID, generation, revision int64, redactor *secrets.Redactor) (LogSink, error)
}

// CrashLogFunc adapts a function to CrashLog.
type CrashLogFunc func(ctx context.Context, environmentID, generation, revision int64, redactor *secrets.Redactor) (LogSink, error)

func (f CrashLogFunc) OpenFinished(ctx context.Context, environmentID, generation, revision int64, redactor *secrets.Redactor) (LogSink, error) {
	return f(ctx, environmentID, generation, revision, redactor)
}

// RecordCrash implements CrashRecorder.
func (s *TargetStore) RecordCrash(ctx context.Context, c Crash) (bool, error) {
	_, err := dbgen.New(s.pool).RecordPreviewCrash(ctx, dbgen.RecordPreviewCrashParams{
		ContainerID: c.ContainerID, EventNanos: c.EventNanos, ExitCode: int32(c.ExitCode),
		EnvironmentID: c.EnvironmentID, Generation: c.Generation, Revision: c.Revision,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record preview crash: %w", err)
	}
	return true, nil
}

// crashRetry is how long WatchCrashes waits after losing Docker's events.
const crashRetry = 5 * time.Second

// WatchCrashes follows Docker's events for this instance's preview apps and
// processes. When one of a live preview stops, it copies the container's
// last output (redacted) into the build log of the attempt that built it,
// with why it stopped and whether Docker restarts it. It runs until ctx ends.
func (r *Runtime) WatchCrashes(ctx context.Context, crashes CrashRecorder, logs CrashLog) {
	for ctx.Err() == nil {
		err := r.followStops(ctx, crashes, logs)
		if ctx.Err() != nil {
			return
		}
		r.logger.WarnContext(ctx, "preview crash watch interrupted; resuming", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(crashRetry):
		}
	}
}

func (r *Runtime) followStops(ctx context.Context, crashes CrashRecorder, logs CrashLog) error {
	filters := client.Filters{}.Add("type", "container").Add("event", "die").
		Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance)
	stream := r.docker.Events(ctx, client.EventsListOptions{Filters: filters})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stream.Err:
			if err == nil {
				err = errors.New("docker closed the event stream")
			}
			return err
		case message := <-stream.Messages:
			if err := r.stopped(ctx, message, crashes, logs); err != nil {
				r.logger.WarnContext(ctx, "preview crash not recorded", "container_id", message.Actor.ID, "error", err)
			}
		}
	}
}

// stopped handles one "die" event.
func (r *Runtime) stopped(ctx context.Context, message events.Message, crashes CrashRecorder, logs CrashLog) error {
	labels := message.Actor.Attributes
	kind := labels[dockerx.LabelKind]
	if kind != dockerx.KindPreview && kind != dockerx.KindProcess {
		return nil
	}
	crash := Crash{ContainerID: message.Actor.ID, EventNanos: message.TimeNano}
	var err error
	crash.EnvironmentID, err = strconv.ParseInt(labels[dockerx.LabelEnvironment], 10, 64)
	if err != nil {
		return nil // Not a published preview's container.
	}
	if crash.Generation, err = strconv.ParseInt(labels[dockerx.LabelGeneration], 10, 64); err != nil {
		return nil
	}
	if crash.Revision, err = strconv.ParseInt(labels[dockerx.LabelRevision], 10, 64); err != nil {
		return nil
	}
	crash.ExitCode, _ = strconv.Atoi(labels["exitCode"])
	if crash.EventNanos <= 0 {
		crash.EventNanos = time.Now().UnixNano()
	}
	recorded, err := crashes.RecordCrash(ctx, crash)
	if err != nil || !recorded {
		return err
	}
	inspected, err := r.docker.ContainerInspect(ctx, crash.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect stopped preview container: %w", err)
	}
	redactor, err := outputRedactor(inspected.Container)
	if err != nil {
		return err
	}
	sink, err := logs.OpenFinished(ctx, crash.EnvironmentID, crash.Generation, crash.Revision, redactor)
	if err != nil {
		return nil // Its logs are gone (expired, deleted or replaced): nothing to add to.
	}
	defer r.closeLogs(ctx, sink)
	name, title := "The app", "App output"
	if kind == dockerx.KindProcess {
		process := labels[dockerx.LabelComponent]
		name, title = "The "+process+" process", "The "+process+" process's output"
	}
	sink.System(stopNotice(name, crash, inspected.Container, time.Unix(0, crash.EventNanos)))
	r.captureOutputOf(ctx, crash.ContainerID, title, sink)
	r.logger.InfoContext(ctx, "preview container stopped", "environment_id", crash.EnvironmentID, "container_id", crash.ContainerID, "exit_code", crash.ExitCode)
	return nil
}

// stopNotice says why a container stopped and what happens next.
func stopNotice(name string, crash Crash, c container.InspectResponse, at time.Time) string {
	when := at.UTC().Format("2 Jan 2006, 15:04:05 UTC")
	var why string
	switch {
	case c.State != nil && c.State.OOMKilled:
		why = fmt.Sprintf("%s stopped at %s: it ran out of memory (exit code %d).", name, when, crash.ExitCode)
	default:
		why = fmt.Sprintf("%s stopped at %s with exit code %d.", name, when, crash.ExitCode)
	}
	switch {
	case c.State != nil && (c.State.Restarting || c.State.Running):
		return why + fmt.Sprintf(" Docker is restarting it (restart %d of %d).", min(c.RestartCount+1, maxRestarts), maxRestarts)
	case crash.ExitCode == 0:
		return why + " It exited on its own, so it is not restarted; build again to bring it back."
	case c.RestartCount >= maxRestarts:
		return why + fmt.Sprintf(" It was restarted %d times and is no longer restarted; build again after fixing it.", maxRestarts)
	default:
		return why
	}
}

// publicEnv are the variables Previa sets that never hold secrets.
var publicEnv = map[string]bool{
	"PORT": true, "HOME": true, "PATH": true, "HOSTNAME": true, "NO_PROXY": true, "no_proxy": true, "NODE_USE_ENV_PROXY": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true,
}

// outputRedactor redacts what a running preview container may print, as the
// startup log did: the values of the variables its LabelRedactEnv names,
// and the password inside any of them that is a URL. A container created
// before that label hides every variable except Previa's public ones.
func outputRedactor(c container.InspectResponse) (*secrets.Redactor, error) {
	if c.Config == nil {
		return secrets.NewRedactor()
	}
	env := map[string]string{}
	for _, entry := range c.Config.Env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	var hide []string
	if names, labelled := c.Config.Labels[dockerx.LabelRedactEnv]; labelled {
		for name := range strings.SplitSeq(names, ",") {
			if value, ok := env[name]; ok && name != "" {
				hide = append(hide, value)
			}
		}
	} else {
		for name, value := range env {
			if !publicEnv[name] {
				hide = append(hide, value)
			}
		}
	}
	for _, value := range hide {
		if u, err := url.Parse(value); err == nil && u.User != nil {
			if password, ok := u.User.Password(); ok {
				hide = append(hide, password)
			}
		}
	}
	redactor, err := secrets.NewRedactor(hide...)
	if err != nil {
		return nil, fmt.Errorf("prepare output redaction: %w", err)
	}
	return redactor, nil
}

// FollowApp follows the output of a live preview's app container: its last
// tail lines and everything it prints after, each line prefixed with
// Docker's RFC 3339 timestamp, until the container stops or ctx ends. The
// container must be this instance's app container of environmentID. The
// redactor hides what the startup log hid.
func (r *Runtime) FollowApp(ctx context.Context, environmentID int64, containerID, tail string) (io.ReadCloser, *secrets.Redactor, error) {
	inspected, err := r.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("inspect preview container: %w", err)
	}
	c := inspected.Container
	if c.Config == nil || c.Config.Labels[dockerx.LabelManaged] != "true" || c.Config.Labels[dockerx.LabelInstance] != r.instance ||
		c.Config.Labels[dockerx.LabelKind] != dockerx.KindPreview || c.Config.Labels[dockerx.LabelEnvironment] != strconv.FormatInt(environmentID, 10) {
		return nil, nil, errors.New("the container is not this preview's app")
	}
	redactor, err := outputRedactor(c)
	if err != nil {
		return nil, nil, err
	}
	logs, err := r.docker.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true, Tail: tail, Timestamps: true})
	if err != nil {
		return nil, nil, fmt.Errorf("follow preview output: %w", err)
	}
	reader, writer := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(writer, writer, logs)
		_ = writer.CloseWithError(err)
	}()
	return &followed{PipeReader: reader, logs: logs}, redactor, nil
}

// followed closes Docker's stream with the pipe that demultiplexes it.
type followed struct {
	*io.PipeReader
	logs io.Closer
}

func (f *followed) Close() error {
	return errors.Join(f.logs.Close(), f.PipeReader.Close())
}
