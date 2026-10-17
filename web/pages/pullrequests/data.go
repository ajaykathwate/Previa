package pullrequests

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/ajaykathwatee/previa/web/components"
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
	// List only: when GitHub last changed the pull request (RFC 3339).
	UpdatedAt string
	// MergeUnknown: the bounded mergeability check gave up without an answer
	// from GitHub (State is then "unknown").
	MergeUnknown bool
	// NoWorker: no running worker syncs pull requests or checks mergeability.
	PublicationRetrying bool
	ExecutionRetrying   bool
	BuildWorkerMissing  bool
	NoWorker            bool
	// Detail only: display data the handler derives from persisted state.
	Description string
	// The head commit's message, author and time, when GitHub supplied them.
	CommitMessage, CommitAuthor, CommitAt string
	Headline                              Headline
	Waiting                               string // why the current request waits ("Waiting for capacity"), if it does
	WaitingNote                           string
	Failure                               string   // safe sentence for the latest failure of the current request
	FailureCode                           string   // its stored category, which Diagnosis explains in the owner's words
	FailureLog                            []string // the failed attempt's last output lines, already redacted
	RequestSource                         string   // "manual" or "automatic"
	RequestedBy                           string   // login of the owner who asked, for manual requests
	RequestedAt                           string   // RFC 3339
	StartedAt                             string
	CompletedAt                           string
	PreviewURL                            string // only for a published preview on a configured domain
	ExpiresAt                             string // RFC 3339; empty when the preview does not expire
	PublishedAt                           string
	// LastOpenedAt is when the router last passed the preview a request
	// (RFC 3339, to the minute); Requests counts them. Empty when unknown.
	LastOpenedAt string
	Requests     int64
	AsleepAt     string // RFC 3339 while the idle preview sleeps; a visit wakes it
	RemovedNote  string // why the preview was removed (owner, expiry, access)
	OlderPreview bool   // the preview serves an earlier commit than the latest
	Actions      Actions
	// Backend previews: what the live preview runs next to its app (one
	// line), its docs address, and its approved outbound connections.
	Backend          string
	DocsURL          string
	Outbound         int
	OutboundWithheld bool
	// Test data (docs/32-persistent-test-data.md): where the serving
	// deployment's databases started, why earlier sources were not used,
	// whether that was its previous build's data, and whether the repository
	// keeps data between builds.
	DataSource  string
	DataReasons []string
	DataKept    bool
	KeepsData   bool
	// ServicesOffered are the managed services a running worker offers
	// ("postgres 17, 16"); ExecutionWorker says whether one runs at all.
	ServicesOffered []string
	ExecutionWorker bool
	// OpenFindings counts the review's open findings, for the Review tab.
	OpenFindings int64
}

// ServicesLabel says what managed services previews may request here.
func (pr PR) ServicesLabel() string {
	switch {
	case !pr.ExecutionWorker:
		return "Unknown until a worker that builds is running."
	case len(pr.ServicesOffered) == 0:
		return "None: no service catalog is configured on the worker (PREVIEW_SERVICE_CATALOG)."
	}
	return strings.Join(pr.ServicesOffered, "; ")
}

// OutboundLabel describes the live preview's approved outbound connections.
func (pr PR) OutboundLabel() string {
	switch {
	case pr.OutboundWithheld:
		return "Withheld: the pull request comes from a fork."
	case pr.Outbound == 1:
		return "1 approved destination."
	case pr.Outbound > 1:
		return strconv.Itoa(pr.Outbound) + " approved destinations."
	}
	return ""
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
	// InProgress labels a disabled action while this commit is queued or building.
	InProgress string
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

// GitHubURL is the PR's page on GitHub; empty unless the address is HTTPS, so
// the link can never run script whatever the stored address holds.
func (pr PR) GitHubURL() string {
	if !strings.HasPrefix(pr.URL, "https://") {
		return ""
	}
	return pr.URL
}

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
	// PrivatePreviews: only GitHub users who can see the repository may
	// open its previews.
	PrivatePreviews           bool
	PrivatePreviewsReady      bool
	PrivatePreviewUnavailable bool
	Rows                      []PR
	Number, Pages, Total      int64
	Search, State             string
	WorkspaceQuery            string
	SelectedPR                *PR
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

// OpenURL is a PR's Open preview action: Previa records that the preview was
// opened from its own pages (the Setup checklist's last step), then goes there.
func OpenURL(id int64, number int32) string { return PRURL(id, number) + "/open" }

// PreviewHref is where an Open preview action leads: through Previa for people
// who can build, directly for Reviewers, whose navigation never opens
// repository pages.
func PreviewHref(ctx context.Context, id int64, number int32, preview string) string {
	if components.CanOperate(ctx) {
		return OpenURL(id, number)
	}
	return preview
}

// OutputURL is a PR's live app output page.
func OutputURL(id int64, number int32) string { return PRURL(id, number) + "/output" }
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
	FailureCode                                  string // Failure's stored category, for Diagnosis
	OlderPreview, Teardown                       bool
	RequestCurrent                               bool   // the request is for the latest commit
	RequestSource                                string // "manual" or "automatic"
	StartedAt, CompletedAt, PublishedAt          string // RFC 3339
	LastOpenedAt                                 string // RFC 3339; empty when unknown
	AsleepAt                                     string // RFC 3339 while the idle preview sleeps
	Revision                                     int64  // the build request's revision, for dismissing its failure
	// Attention is how a failed request is shown: "attention" (recent,
	// highlighted), "older" or "dismissed" (quiet, with AttentionNote saying
	// why), or "" for anything else.
	Attention, AttentionNote string
}

// PreviewStatus is a Previews row's status, in what reviewers care about: a
// preview that keeps serving while a newer build runs or after it failed is
// still live.
func (r ActivityRow) PreviewStatus() (label, tone string) {
	switch r.State {
	case "running":
		return "Live", "tone-ok"
	case "queued", "building", "publishing":
		return "Live · updating", "tone-info"
	case "failed":
		return "Live · update failed", "tone-warn"
	case "tearing_down":
		return "Removing", "tone-warn"
	}
	return Label(r.State), Tone(r.State)
}

// IconTone is the row icon's tone, matching its status badge.
func (r ActivityRow) IconTone(previews bool) string {
	if previews {
		_, tone := r.PreviewStatus()
		return tone
	}
	return r.StatusTone()
}

// StatusTone is the status badge's tone: a failure that no longer needs
// attention is shown, but not in red.
func (r ActivityRow) StatusTone() string {
	if r.Attention == "older" || r.Attention == "dismissed" {
		return "tone-neutral"
	}
	return Tone(r.State)
}

// CanDismiss reports whether the owner can dismiss this failure; Dismissed
// whether they can undo a dismissal.
func (r ActivityRow) CanDismiss() bool { return r.Attention == "attention" || r.Attention == "older" }
func (r ActivityRow) Dismissed() bool  { return r.Attention == "dismissed" }

// FailureURL is the pull request's failure action: "dismiss" or "restore".
func (r ActivityRow) FailureURL(action string) templ.SafeURL {
	return templ.SafeURL(string(PRURL(r.RepoID, r.Number)) + "/failure/" + action)
}

// TeardownDialog is the row's confirmation dialog ID.
func (r ActivityRow) TeardownDialog() string {
	return "teardown-" + strconv.FormatInt(r.RepoID, 10) + "-" + strconv.Itoa(int(r.Number))
}

// Activity is the Builds page (every pull request's current or latest build
// request) or, with Previews, the Previews page (environments that serve a
// commit).
type Activity struct {
	Rows                         []ActivityRow
	Total, Number, Pages, RepoID int64
	Previews                     bool
	// Failures narrows Builds: "attention", "older", "dismissed" or "" (all).
	Failures string
	// DismissedRepo and DismissedNumber name a failure just dismissed, so the
	// page can offer Undo even when the list no longer shows it.
	DismissedRepo   int64
	DismissedNumber int32
	// AttentionDays is how long a failure needs attention.
	AttentionDays int
	// Joined: a Reviewer arrived from accepting an invitation.
	Joined bool
}

// FailureEmpty is what an empty failure view says.
func FailureEmpty(failures string) (heading, text string) {
	switch failures {
	case "attention":
		return "No failures need attention.", "Recent failed builds that you have not dismissed appear here."
	case "older":
		return "No older failures.", "Failures that have waited a long time without a new build appear here, without being highlighted."
	}
	return "Nothing dismissed.", "Failures you dismiss appear here, where you can undo it."
}

// FailureFilter is one of the Builds page's failure filters.
type FailureFilter struct {
	Value, Label string
}

// FailureFilters are the Builds page's views of failures, all first.
func FailureFilters() []FailureFilter {
	return []FailureFilter{{"", "All"}, {"attention", "Needs attention"}, {"older", "Older failures"}, {"dismissed", "Dismissed"}}
}

func (a Activity) PageURL(n int64) string {
	path := "/builds"
	if a.Previews {
		path = "/previews"
	}
	v := url.Values{"page": {strconv.FormatInt(n, 10)}}
	if a.RepoID > 0 {
		v.Set("repo_id", strconv.FormatInt(a.RepoID, 10))
	}
	if a.Failures != "" {
		v.Set("failures", a.Failures)
	}
	return path + "?" + v.Encode()
}

// FilterURL is Builds with another failure filter, from its first page.
func (a Activity) FilterURL(failures string) templ.SafeURL {
	a.Failures = failures
	return templ.SafeURL(a.PageURL(1))
}

// DismissedPR is the pull request whose failure was just dismissed, for Undo.
func (a Activity) DismissedPR() (ActivityRow, bool) {
	if a.DismissedRepo <= 0 || a.DismissedNumber <= 0 {
		return ActivityRow{}, false
	}
	return ActivityRow{RepoID: a.DismissedRepo, Number: a.DismissedNumber}, true
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

// TrailParent is the header trail's previous level: the repository directory
// for a repository's list and the repository's list for a pull request, each
// keeping the filters the owner came from.
func (p Page) TrailParent() components.Crumb {
	if p.SelectedPR != nil {
		return components.Crumb{Label: p.RepositoryName(), Href: p.PageURL(p.Number)}
	}
	return components.Crumb{Label: "Repositories", Href: p.CloseURL()}
}

// TrailCurrent names this page in the header trail.
func (p Page) TrailCurrent() string {
	if p.SelectedPR != nil {
		return "#" + strconv.Itoa(int(p.SelectedPR.Number)) + " " + p.SelectedPR.Title
	}
	return p.RepositoryName()
}

// DocumentTitle names the browser tab.
func (p Page) DocumentTitle() string {
	if p.SelectedPR != nil {
		return "#" + strconv.Itoa(int(p.SelectedPR.Number)) + " " + p.SelectedPR.Title + " · " + p.RepositoryName()
	}
	return p.RepositoryName() + " · Pull requests"
}

// RequestCount says how many requests the router passed to a preview.
func RequestCount(n int64) string {
	switch {
	case n == 1:
		return "1 request"
	case n < 10000:
		return strconv.FormatInt(n, 10) + " requests"
	default:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k requests"
	}
}

// Running reports whether the PR's preview runs now, so it has live output.
func (pr PR) Running() bool { return pr.DeployedSHA != "" && pr.Environment == "running" }

func (p Page) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("repository", p.RepoID)
	for _, pr := range p.Rows {
		refs = append(refs, components.PRRefs(p.RepoID, pr.Number)...)
	}
	if p.SelectedPR != nil {
		refs = append(refs, components.PRRefs(p.RepoID, p.SelectedPR.Number)...)
	}
	return refs
}
func (a Activity) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("repository", a.RepoID)
	for _, r := range a.Rows {
		refs = append(refs, components.PRRefs(r.RepoID, r.Number)...)
	}
	if a.DismissedRepo > 0 {
		refs = append(refs, components.PRRefs(a.DismissedRepo, a.DismissedNumber)...)
	}
	return refs
}

// CommitSubject is the first line of the head commit's message.
func (pr PR) CommitSubject() string {
	subject, _, _ := strings.Cut(pr.CommitMessage, "\n")
	return strings.TrimSpace(subject)
}

// CommitBody is the rest of the head commit's message, after its subject line.
func (pr PR) CommitBody() string {
	_, body, _ := strings.Cut(pr.CommitMessage, "\n")
	return strings.TrimSpace(body)
}
