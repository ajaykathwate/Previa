package appoutput

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	outputpages "github.com/ajaykathwatee/previa/web/pages/appoutput"
	buildpages "github.com/ajaykathwatee/previa/web/pages/builds"
	"github.com/go-chi/chi/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// UserSession is the signed-in owner; the auth feature implements it.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// LiveEvents is the realtime hub surface the stream needs.
type LiveEvents interface {
	Subscribe(topic string, since realtime.Cursor) *realtime.Subscription
}

// Enqueuer starts followers on a worker; the server's River producer
// implements it.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

const (
	pageLines    = 500 // lines shown without JavaScript
	frameLines   = 200 // lines per live frame
	maxStreams   = 64
	streamsUser  = 4
	touchEvery   = 15 * time.Second
	frameTimeout = 10 * time.Second
)

// Topic is raised when a preview's app output changes
// (db/migrations/0025_app_output.sql).
func Topic(environmentID int64) string { return "appoutput:" + strconv.FormatInt(environmentID, 10) }

// Handler serves an owner's live app output: a page and its event stream.
// Viewing it starts a follower on a worker; nothing is followed or kept
// once nobody watches.
type Handler struct {
	store   *Store
	users   UserSession
	jobs    Enqueuer
	live    LiveEvents
	limiter *realtime.Limiter
	opts    realtime.StreamOptions
	logger  *slog.Logger
}

func NewHandler(store *Store, users UserSession, jobs Enqueuer, live LiveEvents, opts realtime.StreamOptions, logger *slog.Logger) *Handler {
	return &Handler{store: store, users: users, jobs: jobs, live: live, opts: opts, logger: logger, limiter: realtime.NewLimiter(maxStreams, streamsUser)}
}

func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}/pull-requests/{number}/output", h.page)
		r.Get("/repositories/{repoID}/pull-requests/{number}/output/events", h.events)
	})
}

func (h *Handler) preview(r *http.Request) (Preview, int64, error) {
	if err := realtime.CheckSession(r.Context()); err != nil {
		return Preview{}, 0, err
	}
	repoID, err := publicid.Param(r, "repoID", "repository")
	if err != nil || repoID <= 0 {
		return Preview{}, 0, ErrUnavailable
	}
	n, err := publicid.PRNumber(r, repoID)
	if err != nil || n <= 0 {
		return Preview{}, 0, ErrUnavailable
	}
	user, _, ok := h.users.UserIDs(r.Context())
	if !ok {
		return Preview{}, 0, ErrUnavailable
	}
	p, err := h.store.Preview(r.Context(), user, repoID, int32(n))
	return p, user, err
}

func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.logger.ErrorContext(r.Context(), "app output request failed", "error", err)
	http.Error(w, "App output is temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
}

// watch notes a viewer and starts a follower when one is needed. touch is
// a connected viewer's heartbeat; otherwise the viewer just opened it.
func (h *Handler) watch(ctx context.Context, environmentID int64, touch bool) error {
	request := h.store.Request
	if touch {
		request = h.store.Touch
	}
	needed, err := request(ctx, environmentID)
	if err != nil || !needed {
		return err
	}
	watch, err := h.store.Watch(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("read app output watch: %w", err)
	}
	args := FollowArgs{EnvironmentID: environmentID, Epoch: watch.Epoch}
	opts := args.InsertOpts()
	if _, err := h.jobs.Insert(ctx, args, &opts); err != nil {
		return fmt.Errorf("start app output follower: %w", err)
	}
	return nil
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	p, _, err := h.preview(r)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	view := outputpages.OutputPage{RepoID: p.RepoID, Repository: p.Repository, Number: p.Number, Title: p.Title, DeployedSHA: p.DeployedSHA}
	var watch Watch
	if p.Live {
		if watch, err = h.follow(r.Context(), p, &view); err != nil {
			h.unavailable(w, r, err)
			return
		}
	}
	st := describe(p, watch)
	view.StatusLabel, view.StatusTone, view.Summary, view.Notices, view.Empty = st.Label, st.Tone, st.Summary, st.Notices, st.Empty
	username, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, outputpages.Output(username, h.users.CSRFToken(r.Context()), view), view).ServeHTTP(w, r)
}

// follow starts (or keeps) a follower for a live preview and fills the page
// with the lines it stored and where live updates resume.
func (h *Handler) follow(ctx context.Context, p Preview, view *outputpages.OutputPage) (Watch, error) {
	if err := h.watch(ctx, p.EnvironmentID, false); err != nil {
		return Watch{}, err
	}
	watch, err := h.store.Watch(ctx, p.EnvironmentID)
	if err != nil {
		return Watch{}, err
	}
	view.Epoch, view.After = watch.Epoch, watch.NextSequence
	if h.live != nil {
		view.EventsURL = view.OutputURL() + "/events"
	}
	if watch.Epoch == 0 {
		return watch, nil
	}
	lines, err := h.store.Lines(ctx, p.EnvironmentID, watch.Epoch, max(watch.First, watch.NextSequence-pageLines), pageLines)
	if err != nil {
		return Watch{}, err
	}
	for _, l := range lines {
		view.Lines = append(view.Lines, buildpages.LogLine{Sequence: l.Sequence, Text: l.Text, At: l.At})
	}
	return watch, nil
}

// status matches the build log page's live "state" event, so its script
// renders both.
type status struct {
	Label   string                 `json:"label"`
	Tone    string                 `json:"tone"`
	Summary string                 `json:"summary"`
	Notices []buildpages.LogNotice `json:"notices"`
	SHA     string                 `json:"sha"`
	Empty   string                 `json:"empty"`
	Epoch   int64                  `json:"epoch"`
}

func describe(p Preview, w Watch) status {
	st := status{SHA: p.DeployedSHA, Epoch: w.Epoch, Notices: []buildpages.LogNotice{}}
	switch {
	case !p.Live:
		st.Label, st.Tone, st.Summary = "Not running", "tone-neutral", "This pull request has no running preview, so there is no app output. Its build log has the output of its last start."
		st.Empty = "No output."
	case w.State == StateFollowing:
		st.Label, st.Tone, st.Summary = "Live", "tone-success", "What the app prints, as it prints it. Lines appear here within a second."
		st.Empty = "The app has printed nothing yet."
	case w.State == StateStopped:
		st.Label, st.Tone, st.Summary = "Stopped", "tone-neutral", "Live output stopped."
		if w.Note != "" {
			st.Summary = w.Note
		}
		st.Empty = "No output."
	default:
		st.Label, st.Tone, st.Summary = "Connecting", "tone-info", "Connecting to the preview's output…"
		st.Empty = "Waiting for output…"
	}
	return st
}

type liveLine struct {
	Sequence int64  `json:"n"`
	Text     string `json:"text"`
	At       string `json:"at,omitempty"`
}

// events streams the output as Server-Sent Events with ids
// "<epoch>.<next>", like the build log stream: a new epoch (a new follower)
// is a "reset". While connected, the viewer keeps the follower running.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	p, user, err := h.preview(r)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	if !p.Live || h.live == nil {
		http.NotFound(w, r)
		return
	}
	release, ok := h.limiter.Acquire(strconv.FormatInt(user, 10))
	if !ok {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "Too many live output views are open. Close one and reload.", http.StatusServiceUnavailable)
		return
	}
	defer release()
	sub := h.live.Subscribe(Topic(p.EnvironmentID), realtime.Cursor{})
	defer sub.Close()
	if err := h.watch(r.Context(), p.EnvironmentID, true); err != nil {
		h.unavailable(w, r, err)
		return
	}
	sse, err := realtime.NewSSE(w)
	if err != nil {
		return
	}
	sse.WriteTimeout(frameTimeout)
	if err := sse.Retry(h.opts.Retry); err != nil {
		return
	}
	state := &streamState{cursor: parseCursor(r), authorize: func(ctx context.Context) error { _, _, err := h.preview(r.WithContext(ctx)); return err }}
	if err := h.sync(r.Context(), sse, p, state); err != nil {
		return
	}
	heartbeat, touch := time.NewTicker(h.opts.Heartbeat), time.NewTicker(touchEvery)
	defer heartbeat.Stop()
	defer touch.Stop()
	deadline := time.NewTimer(h.opts.MaxDuration)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return // The browser reconnects with its cursor.
		case <-heartbeat.C:
			if err := state.authorize(r.Context()); err != nil {
				return
			}
			if err := sse.Comment("keep-alive"); err != nil {
				return
			}
			continue
		case <-touch.C:
			if err := state.authorize(r.Context()); err != nil {
				return
			}
			if err := h.watch(r.Context(), p.EnvironmentID, true); err != nil {
				return
			}
		case _, open := <-sub.Ready():
			sub.Next()
			if !open {
				return
			}
		}
		if err := h.sync(r.Context(), sse, p, state); err != nil {
			return
		}
	}
}

type streamState struct {
	authorize func(context.Context) error
	cursor    struct{ epoch, next int64 }
	status    string
}

func (s *streamState) id() string {
	return strconv.FormatInt(s.cursor.epoch, 10) + "." + strconv.FormatInt(s.cursor.next, 10)
}

// sync sends what the browser is missing: a reset for a new follower, new
// lines in bounded frames, and the status when it changed.
func (h *Handler) sync(ctx context.Context, sse *realtime.SSE, p Preview, s *streamState) error {
	if s.authorize != nil {
		if err := s.authorize(ctx); err != nil {
			return err
		}
	}
	watch, err := h.store.Watch(ctx, p.EnvironmentID)
	if err != nil {
		return err
	}
	if watch.Epoch > 0 && watch.Epoch != s.cursor.epoch {
		s.cursor.epoch, s.cursor.next = watch.Epoch, 0
		if err := send(sse, "reset", s.id(), map[string]int64{"epoch": watch.Epoch}); err != nil {
			return err
		}
	}
	// Lines beyond the kept window are gone; continue from the oldest kept.
	s.cursor.next = max(s.cursor.next, watch.First)
	for watch.Epoch > 0 && ctx.Err() == nil {
		if s.authorize != nil {
			if err := s.authorize(ctx); err != nil {
				return err
			}
		}
		lines, err := h.store.Lines(ctx, p.EnvironmentID, watch.Epoch, s.cursor.next, frameLines)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			break
		}
		frame := struct {
			Epoch int64      `json:"epoch"`
			Lines []liveLine `json:"lines"`
		}{Epoch: watch.Epoch, Lines: make([]liveLine, len(lines))}
		for i, l := range lines {
			frame.Lines[i] = liveLine{Sequence: l.Sequence, Text: l.Text, At: l.At.UTC().Format(time.RFC3339Nano)}
		}
		s.cursor.next = lines[len(lines)-1].Sequence + 1
		if err := send(sse, "lines", s.id(), frame); err != nil {
			return err
		}
		if len(lines) < frameLines {
			break
		}
	}
	encoded, err := json.Marshal(describe(p, watch))
	if err != nil {
		return fmt.Errorf("encode output status: %w", err)
	}
	if string(encoded) != s.status {
		s.status = string(encoded)
		return sse.Send(realtime.Message{ID: s.id(), Event: "state", Data: s.status})
	}
	return nil
}

// parseCursor reads Last-Event-ID on reconnect, otherwise ?since.
func parseCursor(r *http.Request) struct{ epoch, next int64 } {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("since")
	}
	var c struct{ epoch, next int64 }
	epoch, next, found := strings.Cut(value, ".")
	e, err1 := strconv.ParseInt(epoch, 10, 64)
	n, err2 := strconv.ParseInt(next, 10, 64)
	if found && err1 == nil && err2 == nil && e >= 0 && n >= 0 {
		c.epoch, c.next = e, n
	}
	return c
}

func send(sse *realtime.SSE, event, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	return sse.Send(realtime.Message{ID: id, Event: event, Data: string(data)})
}
