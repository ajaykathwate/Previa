package sampledata

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// viewer signs in whoever the test names.
type viewer struct{ id int64 }

func (v *viewer) UserIDs(context.Context) (int64, int64, bool) { return v.id, 0, v.id > 0 }
func (v *viewer) Username(context.Context) (string, bool)      { return "person", true }
func (v *viewer) CSRFToken(context.Context) string             { return "csrf-token" }

// fixedLogs serves the same lines for every refresh.
type fixedLogs struct{ lines []LogLine }

func (l fixedLogs) LinesBefore(_ context.Context, _ int64, before int64, limit int, source, query string) ([]LogLine, error) {
	var out []LogLine
	for _, line := range l.lines {
		if line.Sequence < before && (source != "previa" || line.System) && strings.Contains(line.Text, query) {
			out = append(out, line)
		}
	}
	return out[max(0, len(out)-limit):], nil
}

func (l fixedLogs) LinesFrom(_ context.Context, _ int64, from int64, limit int) ([]LogLine, error) {
	var out []LogLine
	for _, line := range l.lines {
		if line.Sequence >= from && len(out) < limit {
			out = append(out, line)
		}
	}
	return out, nil
}

type tabClient struct {
	t      *testing.T
	server *httptest.Server
	viewer *viewer
	repo   int64
}

func newTabClient(t *testing.T, f *fixture) *tabClient {
	t.Helper()
	v := &viewer{}
	logs := fixedLogs{lines: []LogLine{
		{Sequence: 0, System: true, Text: "Building the test data baseline from branch main at commit 1111111.", At: time.Now()},
		{Sequence: 1, Text: "seeded 3 products", At: time.Now()},
	}}
	h := NewHandler(f.service, logs, v, nil, f.runner.logger)
	r := chi.NewRouter()
	h.Mount(r, func(next http.Handler) http.Handler { return next })
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return &tabClient{t: t, server: server, viewer: v, repo: f.repo}
}

func (c *tabClient) as(user int64) *tabClient {
	c.viewer.id = user
	return c
}

func (c *tabClient) do(method, path string, form url.Values) (int, string, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, c.server.URL+"/repositories/"+strconv.FormatInt(c.repo, 10)+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func TestTheTestDataTab(t *testing.T) {
	f := newFixture(t)
	c := newTabClient(t, f)
	if status, _, _ := c.as(f.reviewer).do("GET", "/test-data", nil); status != http.StatusNotFound {
		t.Fatalf("a Reviewer gets no tab: %d", status)
	}
	status, _, body := c.as(f.dev).do("GET", "/test-data", nil)
	// A Developer reads each setting's value: no switches, forms or disabled controls.
	if status != http.StatusOK || !strings.Contains(body, "Test data is off") || strings.Contains(body, `role="switch"`) || regexp.MustCompile(`<(button|input|select|textarea|fieldset)\\b[^>]*\\sdisabled(\\s|=|/?>)`).MatchString(body) ||
		strings.Contains(body, `/test-data/settings`) || strings.Contains(body, `id="turn-on-test-data"`) || strings.Contains(body, `id="refresh-baseline"`) {
		t.Fatalf("a Developer reads the tab without its controls: %d\n%s", status, body)
	}
	status, _, body = c.as(f.owner).do("GET", "/test-data", nil)
	if status != http.StatusOK || !strings.Contains(body, `id="turn-on-test-data"`) || !strings.Contains(body, `role="switch"`) {
		t.Fatalf("the Owner turns Test data on in a modal: %d", status)
	}
	on := url.Values{"csrf_token": {"x"}, "revision": {"0"}, "enabled": {"on"}, "keep_data": {"on"}, "use_baseline": {"on"}, "auto_refresh": {"on"}, "baseline_branch": {""}}
	status, location, _ := c.do("POST", "/test-data/settings", on)
	if status != http.StatusSeeOther || !strings.HasSuffix(location, "/test-data?saved=on") {
		t.Fatalf("saved: %d %s", status, location)
	}
	status, _, body = c.do("GET", "/test-data?saved=on", nil)
	if status != http.StatusOK || !strings.Contains(body, "Test data is on") || !strings.Contains(body, "Building the first baseline") ||
		!strings.Contains(body, `id="turn-off-test-data"`) || !strings.Contains(body, `id="refresh-baseline"`) {
		t.Fatalf("the tab shows the first refresh: %d\n%s", status, body)
	}
	// A stale form says so instead of overwriting.
	if status, location, _ := c.do("POST", "/test-data/settings", on); status != http.StatusSeeOther || !strings.HasSuffix(location, "notice=conflict") {
		t.Fatalf("stale: %d %s", status, location)
	}
	bad := url.Values{"revision": {"1"}, "enabled": {"on"}, "use_baseline": {"on"}, "baseline_branch": {"two words"}}
	status, _, body = c.do("POST", "/test-data/settings", bad)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "Branch names cannot contain spaces") || !strings.Contains(body, `value="two words"`) {
		t.Fatalf("an invalid branch is shown beside its field: %d", status)
	}
	// A Developer refreshes; only managers delete or turn off.
	if status, location, _ := c.as(f.dev).do("POST", "/test-data/refresh", url.Values{}); status != http.StatusSeeOther || !strings.HasSuffix(location, "saved=refresh") {
		t.Fatalf("refresh: %d %s", status, location)
	}
	if status, location, _ := c.do("POST", "/test-data/off", url.Values{"revision": {"1"}}); status != http.StatusSeeOther || !strings.HasSuffix(location, "notice=manage") {
		t.Fatalf("a Developer cannot turn it off: %d %s", status, location)
	}
	// The refresh's log, for builders.
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE baseline_refreshes SET log_lines = 2 WHERE repo_id=$1", f.repo)
	status, _, body = c.do("GET", "/test-data", nil)
	if status != http.StatusOK || !strings.Contains(body, "PostgreSQL 17") || !strings.Contains(body, "/test-data/refreshes/1/logs") || !strings.Contains(body, "Ready") {
		t.Fatalf("the baseline and its refresh: %d\n%s", status, body)
	}
	status, _, body = c.do("GET", "/test-data/refreshes/1/logs", nil)
	if status != http.StatusOK || !strings.Contains(body, "seeded 3 products") || !strings.Contains(body, "Baseline refresh #1") {
		t.Fatalf("log page: %d\n%s", status, body)
	}
	if status, _, body = c.do("GET", "/test-data/refreshes/1/logs?source=previa", nil); status != http.StatusOK || strings.Contains(body, "seeded 3 products") {
		t.Fatalf("filtered log page: %d", status)
	}
	status, _, body = c.do("GET", "/test-data/refreshes/1/logs/download", nil)
	if status != http.StatusOK || !strings.Contains(body, "seeded 3 products\n") {
		t.Fatalf("download: %d %q", status, body)
	}
	if status, _, _ := c.do("GET", "/test-data/refreshes/9/logs", nil); status != http.StatusNotFound {
		t.Fatalf("an unknown refresh: %d", status)
	}
	if status, _, _ := c.as(f.reviewer).do("GET", "/test-data/refreshes/1/logs", nil); status != http.StatusNotFound {
		t.Fatalf("a Reviewer reads no logs: %d", status)
	}
	// The Owner deletes the baseline, then turns Test data off.
	if status, location, _ := c.as(f.owner).do("POST", "/test-data/delete", url.Values{}); status != http.StatusSeeOther || !strings.HasSuffix(location, "saved=deleted") {
		t.Fatalf("delete: %d %s", status, location)
	}
	if status, location, _ := c.do("POST", "/test-data/off", url.Values{"revision": {"1"}}); status != http.StatusSeeOther || !strings.HasSuffix(location, "saved=off") {
		t.Fatalf("off: %d %s", status, location)
	}
}
