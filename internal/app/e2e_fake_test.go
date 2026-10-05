package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v91/github"
)

const (
	e2eAppID    = 77
	e2eSlug     = "previa-e2e"
	e2eRepoID   = 100
	e2eOwner    = "octo"
	e2eRepo     = "app"
	e2ePRNumber = 7
)

// fakeGitHub is the slice of GitHub the pipeline uses, over TLS: the
// repository and PR (verification), exact-commit tarballs (source), and issue
// comments and check runs (reports).
type fakeGitHub struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	head     string
	updated  time.Time
	archives map[string][]byte // commit -> tar.gz
	fetched  map[string]int    // commit -> archive downloads (builds)
	comments map[int64]string
	checks   map[int64]*fakeRun
	nextID   int64
	// reportsDown makes comment and check-run calls fail with 502 (a partial
	// GitHub outage); rejected counts them.
	reportsDown bool
	rejected    int
}

type fakeRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, archives: map[string][]byte{}, fetched: map[string]int{}, comments: map[int64]string{}, checks: map[int64]*fakeRun{}, nextID: 5000}
	f.server = httptest.NewTLSServer(f)
	t.Cleanup(f.server.Close)
	return f
}

// Host is the TLS server's host:port, allowed for archive downloads.
func (f *fakeGitHub) Host() string {
	u, _ := url.Parse(f.server.URL)
	return u.Host
}

func (f *fakeGitHub) InstallationClient(int64) (*github.Client, error) {
	base := f.server.URL + "/"
	return github.NewClient(github.WithHTTPClient(f.server.Client()), github.WithURLs(&base, &base))
}

func (f *fakeGitHub) App(context.Context) (*github.App, error) {
	id, slug := int64(e2eAppID), e2eSlug
	return &github.App{ID: &id, Slug: &slug}, nil
}

// push makes commit the PR's head and serves files as its tarball.
func (f *fakeGitHub) push(commit string, files map[string]string) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	top := e2eOwner + "-" + e2eRepo + "-" + commit[:7] + "/"
	must := func(err error) {
		if err != nil {
			f.t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755}))
	for name, body := range files {
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX}))
		_, err := tw.Write([]byte(body))
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head, f.updated = commit, time.Now().UTC().Truncate(time.Second)
	f.archives[commit] = out.Bytes()
}

// setReportsDown starts or ends an outage of GitHub's comment and check-run API.
func (f *fakeGitHub) setReportsDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reportsDown = down
}

func (f *fakeGitHub) rejectedReports() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejected
}

func (f *fakeGitHub) livingComment() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.comments {
		return body
	}
	return ""
}

func (f *fakeGitHub) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments)
}

func (f *fakeGitHub) run(commit string, revision int64) *fakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.checks {
		if r.HeadSHA == commit && strings.HasSuffix(r.ExternalID, ":r"+strconv.FormatInt(revision, 10)) {
			copied := *r
			return &copied
		}
	}
	return nil
}

// downloads counts how often commit's source was fetched: once per build.
func (f *fakeGitHub) downloads(commit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched[commit]
}

// openRuns counts check runs on commit that are not completed.
func (f *fakeGitHub) openRuns(commit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.checks {
		if r.HeadSHA == commit && r.Status != "completed" {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	repoPath := "/repos/" + e2eOwner + "/" + e2eRepo
	comment := func(id int64) map[string]any {
		return map[string]any{"id": id, "body": f.comments[id], "user": map[string]any{"login": e2eSlug + "[bot]"},
			"performed_via_github_app": map[string]any{"id": e2eAppID, "slug": e2eSlug}}
	}
	path := r.URL.Path
	if f.reportsDown && (strings.Contains(path, "/comments") || strings.Contains(path, "/check-runs")) {
		f.rejected++
		write(http.StatusBadGateway, map[string]any{"message": "Server Error"})
		return
	}
	switch {
	case path == "/repositories/"+strconv.Itoa(e2eRepoID):
		write(200, map[string]any{"id": e2eRepoID, "name": e2eRepo, "owner": map[string]any{"login": e2eOwner}})
	case path == repoPath+"/pulls/"+strconv.Itoa(e2ePRNumber):
		write(200, map[string]any{"number": e2ePRNumber, "state": "open", "title": "Preview me", "html_url": "https://github.com/octo/app/pull/7",
			"user": map[string]any{"login": "contributor"}, "mergeable": true, "mergeable_state": "clean", "updated_at": f.updated.Format(time.RFC3339),
			"head": map[string]any{"sha": f.head, "ref": "feature", "repo": map[string]any{"id": e2eRepoID}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"id": e2eRepoID}}})
	case strings.HasPrefix(path, repoPath+"/tarball/"):
		commit := strings.TrimPrefix(path, repoPath+"/tarball/")
		w.Header().Set("Location", f.server.URL+"/archives/"+commit+".tar.gz")
		w.WriteHeader(http.StatusFound)
	case strings.HasPrefix(path, "/archives/"):
		commit := strings.TrimSuffix(strings.TrimPrefix(path, "/archives/"), ".tar.gz")
		data, ok := f.archives[commit]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.fetched[commit]++
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(data)
	case path == repoPath+"/issues/"+strconv.Itoa(e2ePRNumber)+"/comments":
		if r.Method == http.MethodPost {
			var body struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			f.comments[f.nextID] = body.Body
			write(201, comment(f.nextID))
			return
		}
		var list []map[string]any
		for id := range f.comments {
			list = append(list, comment(id))
		}
		write(200, list)
	case strings.HasPrefix(path, repoPath+"/issues/comments/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, repoPath+"/issues/comments/"), 10, 64)
		if _, ok := f.comments[id]; !ok {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			var body struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.comments[id] = body.Body
		case http.MethodDelete:
			delete(f.comments, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		write(200, comment(id))
	case path == repoPath+"/check-runs" && r.Method == http.MethodPost:
		run := &fakeRun{}
		_ = json.NewDecoder(r.Body).Decode(run)
		f.nextID++
		run.ID = f.nextID
		f.checks[run.ID] = run
		write(201, run)
	case strings.HasPrefix(path, repoPath+"/check-runs/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, repoPath+"/check-runs/"), 10, 64)
		run, ok := f.checks[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPatch {
			var update fakeRun
			_ = json.NewDecoder(r.Body).Decode(&update)
			run.Status, run.Conclusion, run.ExternalID = update.Status, update.Conclusion, update.ExternalID
		}
		write(200, run)
	case strings.HasPrefix(path, repoPath+"/commits/") && strings.HasSuffix(path, "/check-runs"):
		commit := strings.TrimSuffix(strings.TrimPrefix(path, repoPath+"/commits/"), "/check-runs")
		var runs []*fakeRun
		for _, run := range f.checks {
			if run.HeadSHA == commit {
				runs = append(runs, run)
			}
		}
		write(200, map[string]any{"total_count": len(runs), "check_runs": runs})
	default:
		f.t.Logf("fake GitHub: unhandled %s %s", r.Method, path)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) String() string { return fmt.Sprintf("fake GitHub at %s", f.server.URL) }
