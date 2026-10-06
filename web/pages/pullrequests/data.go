package pullrequests

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/web/components/button"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

type PR struct {
	Number                                                                                          int32
	Title, Author, Branch, SHA, State, Build, URL, RequestedSHA, DeployedSHA, Environment, Deadline string
	Available, Fork, NeedsRefresh                                                                   bool
	// List only: the request is for the latest commit, and the PR's preview
	// address when one is live.
	RequestCurrent bool
	// MergeUnknown: the bounded mergeability check gave up without an answer
	// from GitHub (State is then "unknown").
	MergeUnknown bool
	// NoWorker: no running worker syncs pull requests or checks mergeability.
	NoWorker bool
	// Detail only: display data the handler derives from persisted state.
	Headline      Headline
	Waiting       string // why the current request waits ("Waiting for capacity"), if it does
	WaitingNote   string
	Failure       string   // safe sentence for the latest failure of the current request
	FailureLog    []string // the failed attempt's last output lines, already redacted
	RequestSource string   // "manual" or "automatic"
	RequestedBy   string   // login of the owner who asked, for manual requests
	RequestedAt   string   // RFC 3339
	StartedAt     string
	CompletedAt   string
	PreviewURL    string // only for a published preview on a configured domain
	ExpiresAt     string // RFC 3339; empty when the preview does not expire
	PublishedAt   string
	RemovedNote   string // why the preview was removed (owner, expiry, access)
	OlderPreview  bool   // the preview serves an earlier commit than the latest
	Actions       Actions
}

// Headline answers "what is happening with this PR's preview?" in one line.
type Headline struct{ Tone, Title, Detail string }

// Actions are what the owner may do on a PR now, with the form values that
// make each request idempotent and revision-checked.
type Actions struct {
	Build, BuildNow, Retry, Rebuild, Teardown bool
	// CancelOnly: nothing is deployed, so Teardown only cancels unfinished work.
	CancelOnly       bool
	ExpectedRevision int64
	RequestKey       string // one per rendered page: a duplicate submit coalesces
	// Unavailable explains why no build action is offered (no worker can
	// build, the repository is unavailable, mergeability, ...), and Hint what
	// turns it back on.
	Unavailable, Hint string
	// Blocked is the label of the build action shown disabled next to
	// Unavailable; empty when no build action applies (closed PRs).
	Blocked string
}

// HasBuildAction reports whether any build action is offered.
func (a Actions) HasBuildAction() bool { return a.Build || a.BuildNow || a.Retry || a.Rebuild }

// TeardownLabel names the teardown control for what it would do now.
func (a Actions) TeardownLabel() string {
	if a.CancelOnly {
		return "Cancel build"
	}
	return "Tear down preview"
}

// Live reports whether the PR has a preview that is published now.
func (pr PR) Live() bool { return pr.DeployedSHA != "" && pr.PreviewURL != "" }

// CommitURL links a commit of this PR's repository on GitHub, derived from the
// PR's own GitHub address; empty when that address is unknown.
func (pr PR) CommitURL(sha string) string {
	base, _, found := strings.Cut(pr.URL, "/pull/")
	if !found || sha == "" || !strings.HasPrefix(base, "https://") {
		return ""
	}
	return base + "/commit/" + sha
}

// teardownTitle and teardownText describe the teardown confirmation for what
// it would do now: remove a preview, or only cancel unfinished work.
func teardownTitle(a Actions) string {
	if a.CancelOnly {
		return "Cancel this build?"
	}
	return "Tear down this preview?"
}

func teardownText(a Actions) string {
	if a.CancelOnly {
		return "The unfinished build stops and nothing is published. Nothing builds again until you ask, or a new commit arrives with Automatic builds."
	}
	return "The preview stops serving and its containers are removed. Its address stays reserved: build again to bring it back. Nothing rebuilds it until you ask, or a new commit arrives with Automatic builds."
}

// secondaryWhen makes a build button secondary while Open preview is the
// page's primary action.
func secondaryWhen(secondary bool) button.Variant {
	if secondary {
		return button.VariantOutline
	}
	return button.VariantDefault
}

var errorLine = regexp.MustCompile(`(?i)\b(error|failed|fatal|panic)\b`)

// LooksLikeError marks build output lines worth the reader's eye. It only
// changes their colour; the text is shown as is.
func LooksLikeError(line string) bool { return errorLine.MatchString(line) }

// Took is the duration between two RFC 3339 times ("3 min 12 s"); empty when
// either is missing.
func Took(start, end string) string {
	from, err1 := time.Parse(time.RFC3339, start)
	to, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil || to.Before(from) {
		return ""
	}
	d := to.Sub(from).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour && int(d.Seconds())%60 != 0:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case int(d.Minutes())%60 != 0:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
}

// Gate describes the latest commit's mergeability for the commit card.
func Gate(state string) string {
	switch state {
	case "open":
		return "Ready to build"
	case "checking":
		return "Checking mergeability"
	case "conflicts":
		return "Has merge conflicts"
	case "unknown":
		return "GitHub did not report mergeability"
	case "closed":
		return "Pull request closed"
	case "merged":
		return "Pull request merged"
	default:
		return Label(state)
	}
}

type Page struct {
	RepoID                                          int64
	Name, Mode, SyncState, SyncMessage, LastRefresh string
	Fetched                                         int32
	Available, DiscoveryEnabled                     bool
	Rows                                            []PR
	Number, Pages, Total                            int64
	Search, State                                   string
	WorkspaceQuery                                  string
	SelectedPR                                      *PR
	// LiveURL is the repository event stream, including the resume cursor read
	// before this page's state. Empty disables live updates.
	LiveURL string
	// CSRF protects forms rendered inside partial results (the empty state's
	// Sync).
	CSRF string
	// WorkerMissing: no running worker syncs pull requests or checks
	// mergeability, so queued syncs and checks wait until one starts.
	WorkerMissing bool
	// SyncStale: the requested sync has not finished in time; Sync starts a
	// new one. SyncStaleAt is when a running sync would become stale (RFC 3339).
	SyncStale   bool
	SyncStaleAt string
}

// Syncing reports whether a sync is requested and not finished.
func (p Page) Syncing() bool { return p.SyncState == "queued" || p.SyncState == "syncing" }

// SyncDisabled reports whether Sync Pull Requests would do nothing now.
func (p Page) SyncDisabled() bool {
	return !p.Available || !p.DiscoveryEnabled || p.Syncing() && !p.SyncStale
}

func RepoURL(id int64) string { return fmt.Sprintf("/repositories/%d", id) }
func PRURL(id int64, number int32) string {
	return repositorypages.WorkspaceURL(id, url.Values{"pr": {strconv.Itoa(int(number))}})
}

// LogsURL is a PR's current-build log page.
func LogsURL(id int64, number int32) string { return PRURL(id, number) + "/logs" }
func (p Page) PageURL(n int64) string {
	v, _ := url.ParseQuery(p.WorkspaceQuery)
	v.Set("pr_page", strconv.FormatInt(n, 10))
	v.Set("pr_state", p.State)
	v.Set("pr_q", p.Search)
	v.Del("pr")
	return repositorypages.WorkspaceURL(p.RepoID, v)
}

func (p Page) DetailURL(n int32) string {
	location, _ := url.Parse(p.PageURL(p.Number))
	v := location.Query()
	v.Set("pr", strconv.Itoa(int(n)))
	return repositorypages.WorkspaceURL(p.RepoID, v)
}

func (p Page) CurrentURL() string {
	if p.SelectedPR != nil {
		return p.DetailURL(p.SelectedPR.Number)
	}
	return p.PageURL(p.Number)
}

func (p Page) Query() string {
	_, query, _ := strings.Cut(p.CurrentURL(), "?")
	v, _ := url.ParseQuery(query)
	if p.SelectedPR != nil {
		v.Set("pr", strconv.Itoa(int(p.SelectedPR.Number)))
	}
	return v.Encode()
}

func Label(value string) string {
	switch value {
	case "conflicts":
		return "Conflicts"
	case "checking":
		return "Checking"
	case "open":
		return "Open"
	case "closed":
		return "Closed"
	case "merged":
		return "Merged"
	case "all":
		return "All"
	case "manual":
		return "Manual"
	case "automatic":
		return "Automatic"
	case "pending":
		return "Waiting"
	case "queued":
		return "Queued"
	case "running":
		return "Running"
	case "succeeded":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	case "superseded":
		return "Superseded"
	case "torn_down":
		return "Removed"
	case "tearing_down":
		return "Removing"
	case "publishing":
		return "Publishing"
	case "building":
		return "Building"
	case "unknown":
		return "Unknown"
	default:
		return value
	}
}
func Short(s string) string {
	if s == "" {
		return "—"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type ActivityRow struct {
	RepoID                                       int64
	Number                                       int32
	Name, Title                                  string
	Latest, Requested, Deployed, State, Deadline string
	Available                                    bool
	Waiting, Failure, PreviewURL, ExpiresAt      string
	OlderPreview, Teardown                       bool
	RequestCurrent                               bool   // the request is for the latest commit
	RequestSource                                string // "manual" or "automatic"
	StartedAt, CompletedAt, PublishedAt          string // RFC 3339
}

// TeardownDialog is the row's confirmation dialog ID.
func (r ActivityRow) TeardownDialog() string {
	return "teardown-" + strconv.FormatInt(r.RepoID, 10) + "-" + strconv.Itoa(int(r.Number))
}

type Activity struct {
	Rows                         []ActivityRow
	Total, Number, Pages, RepoID int64
	Environments                 bool
}

func (a Activity) PageURL(n int64) string {
	path := "/builds"
	if a.Environments {
		path = "/environments"
	}
	v := url.Values{"page": {strconv.FormatInt(n, 10)}}
	if a.RepoID > 0 {
		v.Set("repo_id", strconv.FormatInt(a.RepoID, 10))
	}
	return path + "?" + v.Encode()
}

func (p Page) DirectoryFilters() map[string]string {
	v, _ := url.ParseQuery(p.WorkspaceQuery)
	values := make(map[string]string)
	for _, key := range []string{"q", "access", "page"} {
		if v.Has(key) {
			values[key] = v.Get(key)
		}
	}
	return values
}

// Tone maps a saved state to a presentation colour class; unknown or empty
// states (for example "Not requested") stay neutral.
func Tone(state string) string {
	switch state {
	case "open", "succeeded":
		return "tone-ok"
	case "queued", "running", "publishing", "pending", "checking", "building":
		return "tone-info"
	case "conflicts", "tearing_down", "unknown":
		return "tone-warn"
	case "failed":
		return "tone-danger"
	case "merged":
		return "tone-merged"
	default:
		return "tone-neutral"
	}
}
func TimeLabel(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("2 Jan 2006, 15:04 UTC")
}

func (p Page) Selected() PR { return *p.SelectedPR }

func (p Page) Owner() string { owner, _, _ := strings.Cut(p.Name, "/"); return owner }
func (p Page) RepositoryName() string {
	_, name, found := strings.Cut(p.Name, "/")
	if !found {
		return p.Name
	}
	return name
}
func (p Page) CloseURL() string {
	v := url.Values{}
	for key, value := range p.DirectoryFilters() {
		v.Set(key, value)
	}
	return repositorypages.WorkspaceURL(0, v)
}
func (p Page) PolicyMode(modal bool) string {
	if !modal && p.Mode == "automatic" {
		return "manual"
	}
	return "automatic"
}
func (p Page) PolicyAction(modal bool) string {
	if p.PolicyMode(modal) == "manual" {
		return "Turn off automatic"
	}
	return "Enable automatic"
}
