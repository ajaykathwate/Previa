package builds

import (
	"context"
	"io"

	buildkit "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
)

// LogSink receives a build's output. *buildlogs.Writer implements it: output
// is redacted and bounded there, never stored here.
type LogSink interface {
	io.Writer
	System(message string)
}

// supersedable is implemented by sinks that know when a newer attempt has taken
// over; the build stops then.
type supersedable interface {
	Done() <-chan struct{}
}

// streamProgress renders BuildKit status as plain text, like
// `docker build --progress=plain`, into the log. It always drains the channel,
// so BuildKit never blocks on a slow or failed log.
func streamProgress(ctx context.Context, status chan *buildkit.SolveStatus, logs io.Writer) {
	defer func() {
		for range status {
		}
	}()
	display, err := progressui.NewDisplay(logs, progressui.PlainMode)
	if err != nil {
		return
	}
	_, _ = display.UpdateFrom(ctx, status)
}
