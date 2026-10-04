package buildlogs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/go-chi/chi/v5"
)

const (
	testOwner   = int64(1)
	testRepo    = int64(10)
	testEnv     = int64(50)
	testPRPath  = "/repositories/10/pull-requests/7/logs"
	ownerHeader = "X-Test-User"
)

// fakeLogs is an in-memory Reader whose stream can change during a test.
type fakeLogs struct {
	mu     sync.Mutex
	target Target
	stream *Stream
	lines  []Line
}

func (f *fakeLogs) Target(_ context.Context, userID, repoID int64, number int32) (Target, error) {
	if userID != testOwner || repoID != testRepo || number != 7 {
		return Target{}, ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, nil
}

func (f *fakeLogs) Stream(_ context.Context, environmentID int64) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream == nil || environmentID != f.stream.EnvironmentID {
		return Stream{}, ErrNoStream
	}
	s := *f.stream
	s.Lines = int64(len(f.lines))
	return s, nil
}

func (f *fakeLogs) LinesFrom(_ context.Context, _, epoch, from int64, limit int) ([]Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream == nil || epoch != f.stream.Epoch || from >= int64(len(f.lines)) {
		return nil, nil
	}
	end := min(int(from)+limit, len(f.lines))
	return append([]Line(nil), f.lines[from:end]...), nil
}

func (f *fakeLogs) LinesBefore(_ context.Context, _, epoch, before int64, limit int) ([]Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream == nil || epoch != f.stream.Epoch {
		return nil, nil
	}
	before = min(before, int64(len(f.lines)))
	start := max(int(before)-limit, 0)
	return append([]Line(nil), f.lines[start:before]...), nil
}

func (f *fakeLogs) add(texts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, text := range texts {
		f.lines = append(f.lines, Line{Sequence: int64(len(f.lines)), Text: text})
	}
}

// restart replaces the stream with a new attempt, as admission does.
func (f *fakeLogs) restart(texts ...string) {
	f.mu.Lock()
	f.stream.Epoch++
	f.lines = nil
	f.mu.Unlock()
	f.add(texts...)
}

type testSession struct{}

func (testSession) UserIDs(ctx context.Context) (int64, int64, bool) {
	id, ok := ctx.Value(ownerKey{}).(int64)
	return id, 0, ok
}
func (testSession) Username(context.Context) (string, bool) { return "owner", true }
func (testSession) CSRFToken(context.Context) string        { return "csrf" }

type ownerKey struct{}

func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.Header.Get(ownerHeader), 10, 64)
		if err != nil {
			id = testOwner
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, id)))
	})
}

func newFakeLogs(lines int) *fakeLogs {
	f := &fakeLogs{
		target: Target{PullRequestID: 3, RepoID: testRepo, Number: 7, Title: "Add <b>bold</b> feature", Repository: "owner/app", Available: true, EnvironmentID: testEnv,
			Intent: &Intent{Generation: 1, Revision: 1, HeadSHA: "abcdef1234567890", State: "running"}},
		stream: &Stream{EnvironmentID: testEnv, Epoch: 4, Generation: 1, Revision: 1, HeadSHA: "abcdef1234567890"},
	}
	for i := 0; i < lines; i++ {
		f.add("line " + strconv.Itoa(i))
	}
	return f
}

func testServer(t *testing.T, logs Reader, configure ...func(*Handler)) (*httptest.Server, *realtime.Hub) {
	t.Helper()
	hub := realtime.NewHub()
	h := NewHandler(logs, testSession{}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithLiveUpdates(hub, realtime.StreamOptions{Retry: 100 * time.Millisecond, Heartbeat: time.Second, MaxDuration: time.Minute})
	for _, c := range configure {
		c(h)
	}
	r := chi.NewRouter()
	h.Mount(r, withUser)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	t.Cleanup(hub.Close) // Runs first, ending open streams.
	return server, hub
}

func get(t *testing.T, url string, user int64) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	req.Header.Set(ownerHeader, strconv.FormatInt(user, 10))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestLogPageIsOwnerScopedAndRendersOutputAsText(t *testing.T) {
	logs := newFakeLogs(0)
	logs.add(`<script>alert("pwned")</script>`, `<img src=x onerror=alert(1)>`)
	server, _ := testServer(t, logs)
	status, body := get(t, server.URL+testPRPath, testOwner)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, unsafe := range []string{`<script>alert("pwned")`, `<img src=x`, `<b>bold</b>`} {
		if strings.Contains(body, unsafe) {
			t.Fatalf("output rendered as markup: %s", unsafe)
		}
	}
	for _, escaped := range []string{`&lt;script&gt;alert(&#34;pwned&#34;)&lt;/script&gt;`, `&lt;img src=x onerror=alert(1)&gt;`, "Running", "abcdef123456", `data-log-cursor="4.2"`} {
		if !strings.Contains(body, escaped) {
			t.Fatalf("page missing %q", escaped)
		}
	}
	if status, _ := get(t, server.URL+testPRPath, 99); status != http.StatusNotFound {
		t.Fatalf("another user's logs: status %d", status)
	}
	if status, _ := get(t, server.URL+"/repositories/10/pull-requests/8/logs", testOwner); status != http.StatusNotFound {
		t.Fatalf("missing PR: status %d", status)
	}
	if status, _ := get(t, server.URL+testPRPath+"/events", 99); status != http.StatusNotFound {
		t.Fatalf("another user's stream: status %d", status)
	}
}

func TestLogPagePaginatesWithoutJavaScript(t *testing.T) {
	logs := newFakeLogs(1200)
	server, _ := testServer(t, logs)
	_, tail := get(t, server.URL+testPRPath, testOwner)
	if !strings.Contains(tail, `id="L701"`) || strings.Contains(tail, `id="L700"`) || !strings.Contains(tail, `id="L1200"`) {
		t.Fatal("tail page must show the newest 500 lines")
	}
	if !strings.Contains(tail, `href="/repositories/10/pull-requests/7/logs?before=700"`) || !strings.Contains(tail, "data-log-events") {
		t.Fatal("tail page needs an earlier-lines link and live updates")
	}
	_, earlier := get(t, server.URL+testPRPath+"?before=700", testOwner)
	if !strings.Contains(earlier, `id="L201"`) || !strings.Contains(earlier, `id="L700"`) || strings.Contains(earlier, `id="L701"`) {
		t.Fatal("earlier page window is wrong")
	}
	if !strings.Contains(earlier, `href="/repositories/10/pull-requests/7/logs?after=700"`) || strings.Contains(earlier, "data-log-events") {
		t.Fatal("an older page links forward and does not stream")
	}
	_, first := get(t, server.URL+testPRPath+"?after=0", testOwner)
	if !strings.Contains(first, `id="L1"`) || !strings.Contains(first, `id="L500"`) || !strings.Contains(first, "?after=500") {
		t.Fatal("first page window is wrong")
	}
	if status, _ := get(t, server.URL+testPRPath+"?after=-1", testOwner); status != http.StatusBadRequest {
		t.Fatalf("invalid position: status %d", status)
	}
}

func TestLogPageStatesAreAccurate(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeLogs)
		want  []string
	}{
		{"no build requested", func(f *fakeLogs) { f.target.Intent, f.stream = nil, nil }, []string{"No build requested", "No build output is stored"}},
		{"queued after an earlier attempt", func(f *fakeLogs) {
			f.target.Intent = &Intent{Generation: 2, Revision: 2, HeadSHA: "fedcba9876543210", State: "queued"}
		}, []string{"Queued", "has not started yet", "earlier attempt (commit abcdef123456)"}},
		{"truncated and lost", func(f *fakeLogs) {
			f.stream.Truncated, f.stream.OmittedLines, f.stream.LostLines = true, 1234, 5
		}, []string{"Logs truncated", "1234 later lines", "5 lines could not be stored"}},
		{"expired", func(f *fakeLogs) {
			f.stream.PrunedAt, f.stream.Finished = time.Now(), true
			f.target.Intent.State = "failed"
		}, []string{"Failed", "These logs expired", "Logs are kept for 14 days"}},
		{"waiting for output", func(f *fakeLogs) { f.lines = nil }, []string{"Waiting for output"}},
		{"revoked access", func(f *fakeLogs) { f.target.Available = false }, []string{"GitHub access to this repository was removed"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			logs := newFakeLogs(2)
			tt.setup(logs)
			server, _ := testServer(t, logs)
			status, body := get(t, server.URL+testPRPath, testOwner)
			if status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Fatalf("page missing %q", want)
				}
			}
		})
	}
}

type event struct{ id, name, data string }

// openStream connects like EventSource and delivers parsed events.
func openStream(t *testing.T, url, lastID string) (<-chan event, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		_ = res.Body.Close()
		t.Fatalf("stream status %d", res.StatusCode)
	}
	events := make(chan event, 64)
	go func() {
		defer close(events)
		defer func() { _ = res.Body.Close() }()
		reader := bufio.NewReader(res.Body)
		var e event
		for {
			text, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			text = strings.TrimRight(text, "\n")
			switch {
			case text == "":
				if e.name != "" {
					events <- e
				}
				e = event{}
			case strings.HasPrefix(text, "id: "):
				e.id = strings.TrimPrefix(text, "id: ")
			case strings.HasPrefix(text, "event: "):
				e.name = strings.TrimPrefix(text, "event: ")
			case strings.HasPrefix(text, "data: "):
				e.data += strings.TrimPrefix(text, "data: ")
			}
		}
	}()
	return events, cancel
}

func next(t *testing.T, events <-chan event, name string) event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatalf("stream ended waiting for %q", name)
			}
			if e.name == name {
				return e
			}
		case <-timeout:
			t.Fatalf("no %q event", name)
		}
	}
}

func lineTexts(t *testing.T, e event) []string {
	t.Helper()
	var frame struct {
		Lines []struct {
			N    int64  `json:"n"`
			Text string `json:"text"`
		} `json:"lines"`
	}
	if err := json.Unmarshal([]byte(e.data), &frame); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(frame.Lines))
	for i, l := range frame.Lines {
		out[i] = fmt.Sprintf("%d:%s", l.N, l.Text)
	}
	return out
}

func TestLiveLogsResumeWithoutDuplicatesAndResetForANewAttempt(t *testing.T) {
	logs := newFakeLogs(5)
	server, hub := testServer(t, logs)
	events, stop := openStream(t, server.URL+testPRPath+"/events?since=4.3", "")
	defer stop()
	first := next(t, events, "lines")
	if got := strings.Join(lineTexts(t, first), ","); got != "3:line 3,4:line 4" || first.id != "4.5" {
		t.Fatalf("resume frame %q id %q", got, first.id)
	}
	next(t, events, "state")
	logs.add("line 5")
	hub.Publish(Topic(testEnv), "build_logs")
	if got := strings.Join(lineTexts(t, next(t, events, "lines")), ","); got != "5:line 5" {
		t.Fatalf("live frame %q", got)
	}
	// A retry of the same revision replaces the logs: the old cursor must not
	// skip the new attempt's first lines.
	logs.restart("retry 0", "retry 1")
	hub.Publish(Topic(testEnv), "build_logs")
	if reset := next(t, events, "reset"); reset.id != "5.0" {
		t.Fatalf("reset id %q", reset.id)
	}
	if got := strings.Join(lineTexts(t, next(t, events, "lines")), ","); got != "0:retry 0,1:retry 1" {
		t.Fatalf("after reset %q", got)
	}
	stop()
	// Reconnecting with an old attempt's Last-Event-ID resets and replays.
	again, stopAgain := openStream(t, server.URL+testPRPath+"/events", "4.6")
	defer stopAgain()
	next(t, again, "reset")
	if got := strings.Join(lineTexts(t, next(t, again, "lines")), ","); got != "0:retry 0,1:retry 1" {
		t.Fatalf("replay %q", got)
	}
	// And with the current cursor, nothing is repeated.
	current, stopCurrent := openStream(t, server.URL+testPRPath+"/events", "5.2")
	defer stopCurrent()
	if e := next(t, current, "state"); e.id != "5.2" {
		t.Fatalf("state id %q", e.id)
	}
	select {
	case e := <-current:
		if e.name == "lines" || e.name == "reset" {
			t.Fatalf("duplicate %s after resume", e.name)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestLiveLogsCapConnectionsPerUser(t *testing.T) {
	logs := newFakeLogs(1)
	server, _ := testServer(t, logs, func(h *Handler) { h.limiter = realtime.NewLimiter(10, 2) })
	_, stop1 := openStream(t, server.URL+testPRPath+"/events", "")
	defer stop1()
	_, stop2 := openStream(t, server.URL+testPRPath+"/events", "")
	defer stop2()
	status, body := get(t, server.URL+testPRPath+"/events", testOwner)
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "Too many live log views") {
		t.Fatalf("third stream: %d %q", status, body)
	}
	stop1()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, stop := openStreamStatus(t, server.URL+testPRPath+"/events")
		if events == http.StatusOK {
			stop()
			break
		}
		stop()
		if time.Now().After(deadline) {
			t.Fatal("a closed stream did not release its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// openStreamStatus opens a stream and reports only its status.
func openStreamStatus(t *testing.T, url string) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return res.StatusCode, func() { cancel(); _ = res.Body.Close() }
}

// A client that stops reading must be dropped, freeing its slot, instead of
// holding the handler forever.
func TestLiveLogsDropClientsThatStopReading(t *testing.T) {
	logs := newFakeLogs(0)
	long := strings.Repeat("x", 8000)
	for i := 0; i < 4000; i++ {
		logs.add(long)
	}
	server, _ := testServer(t, logs, func(h *Handler) {
		h.limiter = realtime.NewLimiter(10, 1)
		h.frameTimeout = 200 * time.Millisecond
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s/events HTTP/1.1\r\nHost: test\r\n\r\n", testPRPath); err != nil {
		t.Fatal(err)
	}
	// Never read: the server's writes fill the socket buffers and then time out.
	waitForStatus := func(want int, within time.Duration, failure string) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			status, stop := openStreamStatus(t, server.URL+testPRPath+"/events")
			stop()
			if status == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(failure)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitForStatus(http.StatusServiceUnavailable, 3*time.Second, "the stalled client never held a stream slot")
	waitForStatus(http.StatusOK, 10*time.Second, "a stalled client kept its stream slot")
}
