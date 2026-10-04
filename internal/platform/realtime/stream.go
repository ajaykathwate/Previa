package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one Server-Sent Events frame.
type Message struct {
	ID    string
	Event string
	Data  string
}

// SSE writes Server-Sent Events frames. Streams that replay their own records
// (for example build logs keyed by sequence number) can use it directly.
type SSE struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// NewSSE prepares w for streaming: event-stream headers, no proxy buffering and
// no server write deadline for this response only.
func NewSSE(w http.ResponseWriter) (*SSE, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, fmt.Errorf("clear stream write deadline: %w", err)
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	s := &SSE{w: w, rc: rc}
	if err := s.flush(); err != nil {
		return nil, err
	}
	return s, nil
}

// WriteTimeout bounds each later frame, so a client that stops reading is
// dropped instead of holding the handler. Zero (the default) disables it.
func (s *SSE) WriteTimeout(d time.Duration) { s.timeout = d }

// Retry sets how long the browser waits before reconnecting.
func (s *SSE) Retry(d time.Duration) error {
	return s.write("retry: " + strconv.FormatInt(d.Milliseconds(), 10) + "\n\n")
}

// Comment sends a keep-alive line that EventSource ignores.
func (s *SSE) Comment(text string) error {
	return s.write(": " + strings.ReplaceAll(text, "\n", " ") + "\n\n")
}

// Send writes one message and flushes it.
func (s *SSE) Send(m Message) error {
	var b strings.Builder
	if m.ID != "" {
		b.WriteString("id: " + singleLine(m.ID) + "\n")
	}
	if m.Event != "" {
		b.WriteString("event: " + singleLine(m.Event) + "\n")
	}
	for _, line := range strings.Split(m.Data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return s.write(b.String())
}

func (s *SSE) write(frame string) error {
	if s.timeout > 0 {
		if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return fmt.Errorf("set stream write deadline: %w", err)
		}
	}
	if _, err := s.w.Write([]byte(frame)); err != nil {
		return fmt.Errorf("write event stream: %w", err)
	}
	return s.flush()
}

func (s *SSE) flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush event stream: %w", err)
	}
	return nil
}

func singleLine(value string) string { return strings.NewReplacer("\n", "", "\r", "").Replace(value) }

// StreamOptions bound one streaming response.
type StreamOptions struct {
	// Retry is the reconnect delay suggested to the browser.
	Retry time.Duration
	// Heartbeat keeps idle proxies and load balancers from closing the stream.
	Heartbeat time.Duration
	// MaxDuration ends the stream so the browser reconnects through
	// authentication again; the cursor makes the reconnect lossless.
	MaxDuration time.Duration
}

// DefaultStreamOptions suit dashboard invalidation streams.
var DefaultStreamOptions = StreamOptions{Retry: 2 * time.Second, Heartbeat: 25 * time.Second, MaxDuration: 5 * time.Minute}

// RequestCursor reads the resume position: the browser's Last-Event-ID on an
// automatic reconnect, otherwise the "since" query parameter. A present but
// unparsable value yields a cursor that forces a resync.
func RequestCursor(r *http.Request) Cursor {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("since")
	}
	if value == "" {
		return Cursor{}
	}
	if c, ok := ParseCursor(value); ok {
		return c
	}
	return Cursor{Instance: "invalid"}
}

// Stream sends sub as Server-Sent Events until the client leaves, the
// subscription closes or MaxDuration elapses. The first frame is "ready" (or a
// catch-up "change"/"resync"); later frames are "change" or "resync" with JSON
// data {"topic": ...} and the cursor as the event id.
func Stream(w http.ResponseWriter, r *http.Request, sub *Subscription, opts StreamOptions) error {
	stream, err := NewSSE(w)
	if err != nil {
		return err
	}
	if err := stream.Retry(opts.Retry); err != nil {
		return err
	}
	if event, ok := sub.Next(); ok {
		if err := send(stream, event); err != nil {
			return err
		}
	} else if err := stream.Send(Message{ID: sub.Start().String(), Event: "ready", Data: "{}"}); err != nil {
		return err
	}
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()
	deadline := time.NewTimer(opts.MaxDuration)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-deadline.C:
			return nil
		case <-heartbeat.C:
			if err := stream.Comment("keep-alive"); err != nil {
				return err
			}
		case _, open := <-sub.Ready():
			if event, ok := sub.Next(); ok {
				if err := send(stream, event); err != nil {
					return err
				}
			}
			if !open {
				return nil
			}
		}
	}
}

func send(stream *SSE, e Event) error {
	data, err := json.Marshal(struct {
		Topic  string `json:"topic"`
		Source string `json:"source,omitempty"`
	}{e.Topic, e.Source})
	if err != nil {
		return fmt.Errorf("encode realtime event: %w", err)
	}
	return stream.Send(Message{ID: e.Cursor.String(), Event: string(e.Kind), Data: string(data)})
}
