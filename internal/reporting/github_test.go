package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v91/github"
)

const (
	testAppID = 77
	testSlug  = "previa-test"
)

type fakeComment struct {
	ID     int64  `json:"id"`
	Number int    `json:"-"`
	Body   string `json:"body"`
	User   string `json:"-"`
	App    bool   `json:"-"`
}

type fakeCheck struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Title      string `json:"-"`
	Summary    string `json:"-"`
	Details    string `json:"details_url"`
}

// fakeGitHub serves the few REST endpoints reporting uses, with per-route
// failure injection.
type fakeGitHub struct {
	mu       sync.Mutex
	repoID   int64
	comments map[int64]*fakeComment
	checks   map[int64]*fakeCheck
	nextID   int64
	fail     map[string][]int // route -> statuses to return, in order
	calls    []string
	server   *httptest.Server
}

func newFakeGitHub(t *testing.T, repoID int64) *fakeGitHub {
	f := &fakeGitHub{repoID: repoID, comments: map[int64]*fakeComment{}, checks: map[int64]*fakeCheck{}, nextID: 1000, fail: map[string][]int{}}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) InstallationClient(int64) (*github.Client, error) {
	base := f.server.URL + "/"
	return github.NewClient(github.WithHTTPClient(f.server.Client()), github.WithURLs(&base, &base))
}

func (f *fakeGitHub) api(t *testing.T) *GitHubAPI {
	api, err := NewGitHubAPI(f, testAppID, testSlug)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func (f *fakeGitHub) failOnce(route string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[route] = append(f.fail[route], statuses...)
}

func (f *fakeGitHub) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) appComments(number int) []*fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []*fakeComment
	for _, c := range f.comments {
		if c.Number == number && c.App {
			found = append(found, c)
		}
	}
	return found
}

func (f *fakeGitHub) checksFor(sha string) []*fakeCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []*fakeCheck
	for _, c := range f.checks {
		if c.HeadSHA == sha {
			found = append(found, c)
		}
	}
	return found
}

func commentJSON(c *fakeComment) map[string]any {
	user := map[string]any{"login": c.User}
	out := map[string]any{"id": c.ID, "body": c.Body, "user": user}
	if c.App {
		out["performed_via_github_app"] = map[string]any{"id": testAppID, "slug": testSlug}
	}
	return out
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	route := r.Method + " " + routeName(parts)
	f.calls = append(f.calls, route)
	if statuses := f.fail[route]; len(statuses) > 0 {
		f.fail[route] = statuses[1:]
		writeFailure(w, statuses[0])
		return
	}
	decode := func(v any) { _ = json.NewDecoder(r.Body).Decode(v) }
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch routeName(parts) {
	case "repository":
		if parts[1] != strconv.FormatInt(f.repoID, 10) {
			writeFailure(w, 404)
			return
		}
		write(200, map[string]any{"id": f.repoID, "name": "app", "owner": map[string]any{"login": "octo"}})
	case "comment":
		id, _ := strconv.ParseInt(parts[5], 10, 64)
		c := f.comments[id]
		if c == nil {
			writeFailure(w, 404)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			delete(f.comments, id)
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPatch:
			var body struct{ Body string }
			decode(&body)
			c.Body = body.Body
		}
		write(200, commentJSON(c))
	case "comments":
		number, _ := strconv.Atoi(parts[4])
		if r.Method == http.MethodPost {
			var body struct{ Body string }
			decode(&body)
			f.nextID++
			c := &fakeComment{ID: f.nextID, Number: number, Body: body.Body, User: testSlug + "[bot]", App: true}
			f.comments[c.ID] = c
			write(201, commentJSON(c))
			return
		}
		var list []map[string]any
		for id := int64(0); id <= f.nextID; id++ {
			if c := f.comments[id]; c != nil && c.Number == number {
				list = append(list, commentJSON(c))
			}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		const per = 2 // small pages, so discovery has to paginate
		start, end := min((page-1)*per, len(list)), min(page*per, len(list))
		if end < len(list) {
			next := *r.URL
			q := next.Query()
			q.Set("page", strconv.Itoa(page+1))
			next.RawQuery = q.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.server.URL, next.RequestURI()))
		}
		write(200, list[start:end])
	case "check-runs":
		var body fakeCheckBody
		decode(&body)
		f.nextID++
		c := &fakeCheck{ID: f.nextID, HeadSHA: body.HeadSHA, Name: body.Name}
		body.apply(c)
		f.checks[c.ID] = c
		write(201, c)
	case "check-run":
		id, _ := strconv.ParseInt(parts[4], 10, 64)
		c := f.checks[id]
		if c == nil {
			writeFailure(w, 404)
			return
		}
		if r.Method == http.MethodPatch {
			var body fakeCheckBody
			decode(&body)
			body.apply(c)
		}
		write(200, c)
	case "commit-check-runs":
		var runs []*fakeCheck
		name, app := r.URL.Query().Get("check_name"), r.URL.Query().Get("app_id")
		for _, c := range f.checks {
			if c.HeadSHA == parts[4] && c.Name == name && app == strconv.Itoa(testAppID) {
				runs = append(runs, c)
			}
		}
		write(200, map[string]any{"total_count": len(runs), "check_runs": runs})
	default:
		writeFailure(w, 404)
	}
}

type fakeCheckBody struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Details    string `json:"details_url"`
	Output     struct{ Title, Summary string }
}

func (b fakeCheckBody) apply(c *fakeCheck) {
	c.ExternalID, c.Status, c.Conclusion, c.Details = b.ExternalID, b.Status, b.Conclusion, b.Details
	c.Title, c.Summary = b.Output.Title, b.Output.Summary
}

func routeName(parts []string) string {
	switch {
	case len(parts) == 2 && parts[0] == "repositories":
		return "repository"
	case len(parts) == 6 && parts[3] == "issues" && parts[4] == "comments":
		return "comment"
	case len(parts) == 6 && parts[3] == "issues" && parts[5] == "comments":
		return "comments"
	case len(parts) == 4 && parts[3] == "check-runs":
		return "check-runs"
	case len(parts) == 5 && parts[3] == "check-runs":
		return "check-run"
	case len(parts) == 6 && parts[3] == "commits" && parts[5] == "check-runs":
		return "commit-check-runs"
	}
	return strings.Join(parts, "/")
}

// writeFailure answers like GitHub: 429 and a 403 with exhausted quota are
// rate limits; another 403 is a missing permission.
func writeFailure(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	switch status {
	case 429:
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
	case 499: // primary rate limit
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	case 403:
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	default:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"failure"}`))
	}
}

func TestGitHubAPIComments(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t, 55)
	api := f.api(t)
	repo, err := api.Repository(ctx, 1, 55)
	if err != nil || repo != (Repo{Owner: "octo", Name: "app"}) {
		t.Fatalf("repo %+v %v", repo, err)
	}
	if _, err := api.Repository(ctx, 1, 56); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repository: %v", err)
	}
	// Human comments, including one quoting the marker, are never ours.
	f.comments[1] = &fakeComment{ID: 1, Number: 7, Body: "looks good", User: "alice"}
	f.comments[2] = &fakeComment{ID: 2, Number: 7, Body: "quoting " + Marker, User: "mallory"}
	f.comments[3] = &fakeComment{ID: 3, Number: 7, Body: "unrelated", User: "bob"}
	if _, ok, err := api.FindComment(ctx, 1, repo, 7, Marker, 5); err != nil || ok {
		t.Fatalf("found a human's comment: %v %v", ok, err)
	}
	id, err := api.CreateComment(ctx, 1, repo, 7, Marker+"\nhello")
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := api.FindComment(ctx, 1, repo, 7, Marker, 5)
	if err != nil || !ok || found.ID != id || !found.AppOwned {
		t.Fatalf("discovery across pages: %+v %v %v", found, ok, err)
	}
	if _, ok, _ := api.FindComment(ctx, 1, repo, 7, Marker, 1); ok {
		t.Fatal("discovery read past its page bound")
	}
	if err := api.UpdateComment(ctx, 1, repo, id, Marker+"\nupdated"); err != nil {
		t.Fatal(err)
	}
	if c, err := api.GetComment(ctx, 1, repo, id); err != nil || !strings.Contains(c.Body, "updated") || !c.AppOwned {
		t.Fatalf("get %+v %v", c, err)
	}
	if c, _ := api.GetComment(ctx, 1, repo, 1); c.AppOwned {
		t.Fatal("a human's comment counted as the App's")
	}
}

func TestGitHubAPIChecksAndFailureClasses(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t, 55)
	api := f.api(t)
	repo := Repo{Owner: "octo", Name: "app"}
	run := CheckRun{Name: "Previa preview", HeadSHA: headB, ExternalID: "previa:pr:1:b:r1", Status: "in_progress", Title: "Building", Summary: "s"}
	id, err := api.CreateCheckRun(ctx, 1, repo, run)
	if err != nil {
		t.Fatal(err)
	}
	if found, ok, err := api.FindCheckRun(ctx, 1, repo, headB, "Previa preview", run.ExternalID); err != nil || !ok || found != id {
		t.Fatalf("find check %v %v %v", found, ok, err)
	}
	run.Status, run.Conclusion = "completed", "success"
	if err := api.UpdateCheckRun(ctx, 1, repo, id, run); err != nil || f.checks[id].Conclusion != "success" {
		t.Fatalf("update check %v", err)
	}
	for status, check := range map[int]func(error) bool{
		499: func(err error) bool { var r *RateLimited; return errors.As(err, &r) && r.RetryAfter > time.Minute },
		429: func(err error) bool { var r *RateLimited; return errors.As(err, &r) && r.RetryAfter == 7*time.Second },
		403: func(err error) bool { return errors.Is(err, ErrPermissionDenied) },
		422: func(err error) bool { return errors.Is(err, ErrRejected) },
		500: func(err error) bool { return errors.Is(err, ErrUnavailable) },
		404: func(err error) bool { return errors.Is(err, ErrNotFound) },
	} {
		f.failOnce("PATCH check-run", status)
		if err := api.UpdateCheckRun(ctx, 1, repo, id, run); !check(err) {
			t.Fatalf("status %d classified as %v", status, err)
		}
	}
}
