package appoutput

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/go-chi/chi/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type session struct{}

type userKey struct{}

func (session) UserIDs(ctx context.Context) (int64, int64, bool) {
	id, ok := ctx.Value(userKey{}).(int64)
	return id, 0, ok
}
func (session) Username(context.Context) (string, bool) { return "owner", true }
func (session) CSRFToken(context.Context) string        { return "csrf" }

type jobs struct{ inserted []FollowArgs }

func (j *jobs) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	j.inserted = append(j.inserted, args.(FollowArgs))
	return &rivertype.JobInsertResult{}, nil
}

// The owner's page starts a follower and shows the stored lines as text;
// other users get 404; a preview that is not running says so.
func TestOutputPageIsOwnerScopedAndStartsAFollower(t *testing.T) {
	f := newFixture(t)
	queue := &jobs{}
	hub := realtime.NewHub()
	t.Cleanup(hub.Close)
	h := NewHandler(f.store, session{}, queue, hub, realtime.DefaultStreamOptions, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.Mount(r, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, _ := strconv.ParseInt(req.Header.Get("X-User"), 10, 64)
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userKey{}, id)))
		})
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	get := func(user int64) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/repositories/"+strconv.FormatInt(f.repoID, 10)+"/pull-requests/7/output", nil)
		req.Header.Set("X-User", strconv.FormatInt(user, 10))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	status, body := get(f.owner)
	if status != http.StatusOK || len(queue.inserted) != 1 || queue.inserted[0].EnvironmentID != f.envID || !strings.Contains(body, "Connecting") || !strings.Contains(body, "data-log-events=") {
		t.Fatalf("first view: %d, %d jobs\n%s", status, len(queue.inserted), body)
	}
	epoch, _, err := f.store.Begin(t.Context(), f.envID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Append(t.Context(), f.envID, epoch, []Line{{At: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC), Text: "<script>alert(1)</script>"}}); err != nil {
		t.Fatal(err)
	}
	status, body = get(f.owner)
	if status != http.StatusOK || len(queue.inserted) != 1 || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(body, "<script>alert") || !strings.Contains(body, ">Live<") {
		t.Fatalf("following: %d, %d jobs\n%s", status, len(queue.inserted), body)
	}
	if status, _ := get(f.owner + 1); status != http.StatusNotFound {
		t.Fatalf("another user's output: %d", status)
	}
	f.exec(t, "UPDATE environments SET status='torn_down', container_id=NULL")
	if status, body := get(f.owner); status != http.StatusOK || !strings.Contains(body, "Not running") || strings.Contains(body, "data-log-events=") {
		t.Fatalf("not running: %d\n%s", status, body)
	}
}
