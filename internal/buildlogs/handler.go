package buildlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	buildpages "github.com/ajaykathwatee/previa/web/pages/builds"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	"github.com/go-chi/chi/v5"
)

// Reader is the storage the log pages need; *Store implements it.
type Reader interface {
	Target(ctx context.Context, userID, repoID int64, number int32) (Target, error)
	Stream(ctx context.Context, environmentID int64) (Stream, error)
	LinesFrom(ctx context.Context, environmentID, epoch, from int64, limit int) ([]Line, error)
	LinesBefore(ctx context.Context, environmentID, epoch, before int64, limit int) ([]Line, error)
	Delete(ctx context.Context, environmentID, epoch int64) error
}

type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// LiveEvents is the realtime hub surface the live log stream needs.
type LiveEvents interface {
	Subscribe(topic string, since realtime.Cursor) *realtime.Subscription
}

const (
	pageLines  = 500 // lines per page without JavaScript
	frameLines = 200 // lines per live frame
	// MaxStreams and MaxStreamsPerUser cap open live log connections.
	MaxStreams        = 256
	MaxStreamsPerUser = 8
	frameTimeout      = 10 * time.Second
	ownershipRecheck  = 30 * time.Second
)

// Topic is the realtime topic raised when an environment's log stream changes
// (db/migrations/0013_build_log_streams.sql).
func Topic(environmentID int64) string { return "buildlog:" + strconv.FormatInt(environmentID, 10) }

// repositoryTopic is raised by the repository triggers of
// db/migrations/0011_realtime_events.sql, including build intent changes.
func repositoryTopic(repoID int64) string { return "repository:" + strconv.FormatInt(repoID, 10) }

type Handler struct {
	logs         Reader
	users        UserSession
	live         LiveEvents
	limiter      *realtime.Limiter
	opts         realtime.StreamOptions
	frameTimeout time.Duration
	retention    time.Duration
	logger       *slog.Logger
}

func NewHandler(logs Reader, users UserSession, logger *slog.Logger) *Handler {
	return &Handler{logs: logs, users: users, logger: logger, limiter: realtime.NewLimiter(MaxStreams, MaxStreamsPerUser), opts: realtime.DefaultStreamOptions, frameTimeout: frameTimeout, retention: DefaultRetention}
}

// WithRetention states how long finished logs are kept (BUILD_LOG_RETENTION_DAYS).
func (h *Handler) WithRetention(d time.Duration) *Handler {
	if d > 0 {
		h.retention = d
	}
	return h
}

// WithLiveUpdates enables streaming. Without it the page still works through
// ordinary reloads and pagination.
func (h *Handler) WithLiveUpdates(live LiveEvents, opts realtime.StreamOptions) *Handler {
	h.live, h.opts = live, opts
	return h
}

func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}/pull-requests/{number}/logs", h.page)
		r.Get("/repositories/{repoID}/pull-requests/{number}/logs/events", h.events)
		r.Get("/repositories/{repoID}/pull-requests/{number}/logs/download", h.download)
		r.Post("/repositories/{repoID}/pull-requests/{number}/logs/delete", h.delete)
	})
}

func (h *Handler) identity(r *http.Request) (user, repoID int64, number int32, ok bool) {
	repoID, err := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
	if err != nil || repoID <= 0 {
		return 0, 0, 0, false
	}
	n, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 32)
	if err != nil || n <= 0 {
		return 0, 0, 0, false
	}
	user, _, ok = h.users.UserIDs(r.Context())
	return user, repoID, int32(n), ok
}

func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.logger.ErrorContext(r.Context(), "build log request failed", "error", err)
	http.Error(w, "Build logs are temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, repoID, number, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, err := h.logs.Target(r.Context(), user, repoID, number)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	stream, err := h.logs.Stream(r.Context(), target.EnvironmentID)
	hasStream := err == nil
	if err != nil && !errors.Is(err, ErrNoStream) {
		h.unavailable(w, r, err)
		return
	}
	st := describe(target, stream, hasStream, h.retention)
	view := buildpages.LogPage{
		RepoID: target.RepoID, Repository: target.Repository, Number: target.Number, Title: target.Title,
		StatusLabel: st.Label, StatusTone: st.Tone, Summary: st.Summary, StreamSHA: st.SHA, Notices: st.Notices,
		Empty: st.Empty, Epoch: st.Epoch, HasOutput: hasStream && stream.Lines > 0 && stream.PrunedAt.IsZero(),
		CanDelete: deletable(target, stream, hasStream),
	}
	view.Tail = !hasStream
	if hasStream {
		if err := h.fillLines(r, &view, stream); errors.Is(err, errBadPage) {
			http.Error(w, "Invalid log position.", http.StatusBadRequest)
			return
		} else if err != nil {
			h.unavailable(w, r, err)
			return
		}
	}
	if view.Tail && h.live != nil {
		view.EventsURL = pages.LogsURL(target.RepoID, target.Number) + "/events"
	}
	username, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	templ.Handler(buildpages.Logs(username, h.users.CSRFToken(r.Context()), view)).ServeHTTP(w, r)
}

var errBadPage = errors.New("invalid log page")

// fillLines adds the requested window of lines and where it sits in the
// stream, for paging and for resuming the live tail.
func (h *Handler) fillLines(r *http.Request, view *buildpages.LogPage, stream Stream) error {
	lines, tail, more, err := h.window(r, stream)
	if err != nil {
		return err
	}
	view.Tail, view.More = tail, more
	for _, l := range lines {
		view.Lines = append(view.Lines, buildpages.LogLine{Sequence: l.Sequence, System: l.System, Text: l.Text, At: l.At})
	}
	switch {
	case len(lines) > 0:
		view.Before, view.After = lines[0].Sequence, lines[len(lines)-1].Sequence+1
	case tail:
		view.After = stream.Lines
	}
	return nil
}

// download sends the current attempt's whole output as a text file, in
// bounded reads. Lines were redacted before they were stored.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	user, repoID, number, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, err := h.logs.Target(r.Context(), user, repoID, number)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	stream, err := h.logs.Stream(r.Context(), target.EnvironmentID)
	if errors.Is(err, ErrNoStream) {
		http.Error(w, "No build output is stored for this pull request.", http.StatusNotFound)
		return
	}
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	_, name, _ := strings.Cut(target.Repository, "/")
	file := fmt.Sprintf("%s-pr-%d-build.log", downloadName.ReplaceAllString(name, "-"), target.Number)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for next := int64(0); next < stream.Lines; {
		lines, err := h.logs.LinesFrom(r.Context(), stream.EnvironmentID, stream.Epoch, next, downloadBatch)
		if err != nil {
			h.logger.WarnContext(r.Context(), "build log download ended early", "error", err)
			return
		}
		if len(lines) == 0 {
			return
		}
		var b strings.Builder
		for _, l := range lines {
			if !l.At.IsZero() {
				b.WriteString(l.At.UTC().Format(downloadTime))
				b.WriteByte(' ')
			}
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
		if _, err := w.Write([]byte(b.String())); err != nil {
			return
		}
		next = lines[len(lines)-1].Sequence + 1
	}
}

// delete removes a finished attempt's logs for the PR's owner, then returns
// to the log page, which says they were deleted. The form names the attempt
// (epoch) the owner saw, so a newer attempt's logs are never deleted.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	user, repoID, number, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	epoch, err := strconv.ParseInt(r.PostFormValue("epoch"), 10, 64)
	if err != nil || epoch <= 0 {
		http.Error(w, "Invalid build attempt.", http.StatusBadRequest)
		return
	}
	target, err := h.logs.Target(r.Context(), user, repoID, number)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	if target.EnvironmentID <= 0 {
		http.NotFound(w, r)
		return
	}
	switch err := h.logs.Delete(r.Context(), target.EnvironmentID, epoch); {
	case errors.Is(err, ErrNotDeletable):
		http.Error(w, "These logs can't be deleted now: the build is still running, a newer attempt replaced them, or they are already gone. Reload the page.", http.StatusConflict)
		return
	case err != nil:
		h.unavailable(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "build logs deleted by owner", "pr_id", target.PullRequestID, "epoch", epoch)
	http.Redirect(w, r, pages.LogsURL(target.RepoID, target.Number), http.StatusSeeOther)
}

// downloadBatch lines are read per query; downloadName keeps a file name
// safe; downloadTime prefixes each line with when it was received (UTC).
const (
	downloadBatch = 1000
	downloadTime  = "2006-01-02T15:04:05.000Z"
)

var downloadName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// window selects the lines for one page: the newest by default, or the page
// before/after a sequence number.
func (h *Handler) window(r *http.Request, s Stream) (lines []Line, tail, more bool, err error) {
	q := r.URL.Query()
	position := func(key string) (int64, error) {
		n, err := strconv.ParseInt(q.Get(key), 10, 64)
		if err != nil || n < 0 {
			return 0, errBadPage
		}
		return n, nil
	}
	switch {
	case q.Has("after"):
		after, err := position("after")
		if err != nil {
			return nil, false, false, err
		}
		lines, err = h.logs.LinesFrom(r.Context(), s.EnvironmentID, s.Epoch, after, pageLines+1)
		if err != nil {
			return nil, false, false, err
		}
		if len(lines) > pageLines {
			return lines[:pageLines], false, true, nil
		}
		return lines, after+int64(len(lines)) >= s.Lines, false, nil
	case q.Has("before"):
		before, err := position("before")
		if err != nil {
			return nil, false, false, err
		}
		lines, err = h.logs.LinesBefore(r.Context(), s.EnvironmentID, s.Epoch, before, pageLines)
		return lines, false, before < s.Lines, err
	default:
		lines, err = h.logs.LinesBefore(r.Context(), s.EnvironmentID, s.Epoch, s.Lines, pageLines)
		return lines, true, false, err
	}
}

// liveCursor is the browser's position: attempt epoch and next sequence.
type liveCursor struct{ epoch, next int64 }

func (c liveCursor) String() string {
	return strconv.FormatInt(c.epoch, 10) + "." + strconv.FormatInt(c.next, 10)
}

// parseCursor reads Last-Event-ID on reconnect, otherwise ?since. A missing or
// invalid value starts from nothing, which forces a reset and full replay.
func parseCursor(r *http.Request) liveCursor {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("since")
	}
	epoch, next, found := strings.Cut(value, ".")
	e, err1 := strconv.ParseInt(epoch, 10, 64)
	n, err2 := strconv.ParseInt(next, 10, 64)
	if !found || err1 != nil || err2 != nil || e < 0 || n < 0 {
		return liveCursor{}
	}
	return liveCursor{epoch: e, next: n}
}

// liveState is one connection's view of what the browser already has.
type liveState struct {
	user, repoID int64
	number       int32
	target       Target
	checked      time.Time
	cursor       liveCursor
	status       string
}

// events streams the current attempt's lines as Server-Sent Events. Event ids
// are "<epoch>.<next>", so a reconnect resumes losslessly; a different epoch
// means a new attempt replaced the logs, and the browser receives "reset" before
// the new attempt's lines from sequence 0. Hub notifications only wake the
// stream; lines are always read from the database with short queries.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	user, repoID, number, ok := h.identity(r)
	if !ok || h.live == nil {
		http.NotFound(w, r)
		return
	}
	target, err := h.logs.Target(r.Context(), user, repoID, number)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	release, ok := h.limiter.Acquire(strconv.FormatInt(user, 10))
	if !ok {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "Too many live log views are open. Close one and reload.", http.StatusServiceUnavailable)
		return
	}
	defer release()
	// Subscribe before reading, so a commit between the read and the
	// subscription still wakes this stream.
	repoSub := h.live.Subscribe(repositoryTopic(repoID), realtime.Cursor{})
	defer repoSub.Close()
	var logSub *realtime.Subscription
	subscribeLogs := func(environmentID int64) {
		if logSub == nil && environmentID > 0 {
			logSub = h.live.Subscribe(Topic(environmentID), realtime.Cursor{})
		}
	}
	subscribeLogs(target.EnvironmentID)
	defer func() {
		if logSub != nil {
			logSub.Close()
		}
	}()

	sse, err := realtime.NewSSE(w)
	if err != nil {
		return
	}
	sse.WriteTimeout(h.frameTimeout)
	if err := sse.Retry(h.opts.Retry); err != nil {
		return
	}
	state := &liveState{user: user, repoID: repoID, number: number, target: target, checked: time.Now(), cursor: parseCursor(r)}
	// Re-read the target now that the subscriptions exist, so an environment
	// created after the first read is watched from the start.
	if err := h.sync(r.Context(), sse, state, true); err != nil {
		return
	}
	subscribeLogs(state.target.EnvironmentID)
	heartbeat := time.NewTicker(h.opts.Heartbeat)
	defer heartbeat.Stop()
	deadline := time.NewTimer(h.opts.MaxDuration)
	defer deadline.Stop()
	for {
		var logReady <-chan struct{}
		if logSub != nil {
			logReady = logSub.Ready()
		}
		recheck := false
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return // The browser reconnects through authentication with its cursor.
		case <-heartbeat.C:
			if err := sse.Comment("keep-alive"); err != nil {
				return
			}
			continue
		case _, open := <-repoSub.Ready():
			repoSub.Next()
			if !open {
				return
			}
			recheck = true // Intent, environment or access changed.
		case _, open := <-logReady:
			logSub.Next()
			if !open {
				return
			}
		}
		if err := h.sync(r.Context(), sse, state, recheck); err != nil {
			return
		}
		subscribeLogs(state.target.EnvironmentID)
	}
}

var errStopStream = errors.New("stop log stream")

// sync sends whatever the browser is missing: a reset when the attempt changed,
// new lines in bounded frames, and the status when it changed.
func (h *Handler) sync(ctx context.Context, sse *realtime.SSE, s *liveState, recheck bool) error {
	if recheck || time.Since(s.checked) >= ownershipRecheck {
		target, err := h.logs.Target(ctx, s.user, s.repoID, s.number)
		if err != nil {
			return errStopStream // Ownership lost or storage failing: let the browser reconnect.
		}
		s.target, s.checked = target, time.Now()
	}
	stream, err := h.logs.Stream(ctx, s.target.EnvironmentID)
	hasStream := err == nil
	if err != nil && !errors.Is(err, ErrNoStream) {
		return err
	}
	if hasStream && stream.Epoch != s.cursor.epoch {
		s.cursor = liveCursor{epoch: stream.Epoch}
		if err := send(sse, "reset", s.cursor.String(), map[string]int64{"epoch": stream.Epoch}); err != nil {
			return err
		}
	}
	for hasStream && stream.PrunedAt.IsZero() && ctx.Err() == nil {
		lines, err := h.logs.LinesFrom(ctx, stream.EnvironmentID, stream.Epoch, s.cursor.next, frameLines)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			break
		}
		frame := struct {
			Epoch int64      `json:"epoch"`
			Lines []liveLine `json:"lines"`
		}{Epoch: stream.Epoch, Lines: make([]liveLine, len(lines))}
		for i, l := range lines {
			frame.Lines[i] = newLiveLine(l)
		}
		s.cursor.next = lines[len(lines)-1].Sequence + 1
		if err := send(sse, "lines", s.cursor.String(), frame); err != nil {
			return err
		}
		if len(lines) < frameLines {
			break
		}
	}
	st := describe(s.target, stream, hasStream, h.retention)
	encoded, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode log status: %w", err)
	}
	if string(encoded) != s.status {
		s.status = string(encoded)
		return sse.Send(realtime.Message{ID: s.cursor.String(), Event: "state", Data: s.status})
	}
	return nil
}

type liveLine struct {
	Sequence int64  `json:"n"`
	System   bool   `json:"system,omitempty"`
	Text     string `json:"text"`
	At       string `json:"at,omitempty"` // RFC 3339, UTC
}

func newLiveLine(l Line) liveLine {
	ll := liveLine{Sequence: l.Sequence, System: l.System, Text: l.Text}
	if !l.At.IsZero() {
		ll.At = l.At.UTC().Format(time.RFC3339Nano)
	}
	return ll
}

func send(sse *realtime.SSE, event, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	return sse.Send(realtime.Message{ID: id, Event: event, Data: string(data)})
}
