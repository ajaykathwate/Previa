package realtime

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readFrame returns the fields of the next SSE frame, skipping comments.
func readFrame(t *testing.T, r *bufio.Reader) map[string]string {
	t.Helper()
	frame := map[string]string{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if len(frame) > 0 {
				return frame
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			frame["comment"] = line
			continue
		}
		key, value, _ := strings.Cut(line, ": ")
		frame[key] = value
	}
}

func TestStreamSendsReadyChangesAndEndsWithHub(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := hub.Subscribe("repository:1", RequestCursor(r))
		defer sub.Close()
		if err := Stream(w, r, sub, StreamOptions{Retry: time.Second, Heartbeat: 50 * time.Millisecond, MaxDuration: time.Minute}); err != nil {
			t.Errorf("stream: %v", err)
		}
	}))
	server.Config.WriteTimeout = 100 * time.Millisecond // Stream must lift this deadline.
	defer server.Close()

	res, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	r := bufio.NewReader(res.Body)
	if f := readFrame(t, r); f["retry"] != "1000" {
		t.Fatalf("retry frame = %v", f)
	}
	ready := readFrame(t, r)
	if ready["event"] != "ready" || ready["id"] != hub.Cursor().String() {
		t.Fatalf("ready frame = %v", ready)
	}
	time.Sleep(150 * time.Millisecond) // Past the server write timeout.
	hub.Publish("repository:2", "pull_requests")
	hub.Publish("repository:1", "pr_syncs")
	change := readFrame(t, r)
	for change["event"] == "" {
		change = readFrame(t, r) // Heartbeats may arrive first.
	}
	if change["event"] != "change" || change["id"] != hub.Cursor().String() || change["data"] != `{"topic":"repository:1","source":"pr_syncs"}` {
		t.Fatalf("change frame = %v", change)
	}
	hub.Close()
	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, r); ended <- err }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("stream did not end cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream kept running after hub close")
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	hub := NewHub()
	since := hub.Cursor()
	hub.Publish("repository:1", "pull_requests")
	req := httptest.NewRequest("GET", "/events?since=ignored", nil)
	req.Header.Set("Last-Event-ID", since.String())
	if got := RequestCursor(req); got != since {
		t.Fatalf("RequestCursor = %+v; header should win", got)
	}
	req = httptest.NewRequest("GET", "/events?since=not-a-cursor", nil)
	if got := RequestCursor(req); got.Instance != "invalid" {
		t.Fatalf("malformed cursor = %+v; want forced resync", got)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := hub.Subscribe("repository:1", RequestCursor(r))
		defer sub.Close()
		_ = Stream(w, r, sub, StreamOptions{Retry: time.Second, Heartbeat: time.Minute, MaxDuration: 200 * time.Millisecond})
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "?since=" + since.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	r := bufio.NewReader(res.Body)
	readFrame(t, r)
	if f := readFrame(t, r); f["event"] != "change" || f["id"] != hub.Cursor().String() {
		t.Fatalf("catch-up frame = %v", f)
	}
}

func TestSSEFramesMultilineDataAndStripsIDNewlines(t *testing.T) {
	w := httptest.NewRecorder()
	s, err := NewSSE(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(Message{ID: "a\nb", Event: "log", Data: "one\ntwo"}); err != nil {
		t.Fatal(err)
	}
	if got, want := w.Body.String(), "id: ab\nevent: log\ndata: one\ndata: two\n\n"; got != want {
		t.Fatalf("frame = %q; want %q", got, want)
	}
	if w.Header().Get("X-Accel-Buffering") != "no" || w.Header().Get("Cache-Control") == "" {
		t.Fatal("stream headers missing")
	}
}
