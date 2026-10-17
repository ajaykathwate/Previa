package reviews

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// PageSize is how many findings one page lists.
const PageSize = 50

// LatestReplies is how many of a finding's newest replies a list shows; its
// own link shows them all.
const LatestReplies = 3

// Finding states.
const (
	StateOpen     = "open"
	StateResolved = "resolved"
	StateRemoved  = "removed"
	StateAll      = "all"
)

// Moderation reasons, in the order the removal dialog offers them.
var Reasons = []Reason{
	{"spam", "Spam or off-topic"},
	{"abuse", "Abusive or harassing"},
	{"sensitive", "Contains secrets or personal data"},
	{"mistake", "Posted by mistake"},
}

// Reason is one moderation reason.
type Reason struct{ Value, Label string }

// ReasonLabel names a stored moderation reason.
func ReasonLabel(value string) string {
	for _, r := range Reasons {
		if r.Value == value {
			return r.Label
		}
	}
	return value
}

// Page is one pull request's review page.
type Page struct {
	RepoID                     int64
	Repository                 string // owner/name
	Number                     int32
	Title, Author, Branch      string
	State                      string // open, closed or merged
	GitHubURL                  string
	Description                string // imported from GitHub; rendered as escaped text
	CanOperate                 bool   // may write instructions, moderate and resolve any finding
	ViewerID                   int64
	CSRF, LiveURL, AbsoluteURL string // AbsoluteURL: this page's shareable address

	Preview      Preview
	Instructions Instructions
	Findings     Findings
	OpenFindings int64
	// Reset is the Reset test data form, when the viewer may rebuild the
	// serving commit now; nil otherwise.
	Reset      *ResetForm
	TestAccess TestAccess

	Toast  string
	Notice Notice
	// Draft is a form shown again with what the person wrote, after a
	// validation error or a conflict.
	Draft *Draft
	// Edit opens an edit form without JavaScript: "instructions" or a
	// message as "finding.position".
	Edit string
	// Remove asks to confirm a removal without JavaScript.
	Remove *RemoveTarget
}

// Notice explains the outcome of the last action.
type Notice struct{ Tone, Text string }

// Closed reports a closed or merged pull request, whose review is read-only.
func (p Page) Closed() bool { return p.State != "open" }

// URL is the review page's address.
func (p Page) URL() string { return URL(p.RepoID, p.Number) }

// URL is a pull request's review page.
func URL(repoID int64, number int32) string { return prpages.PRURL(repoID, number) + "/review" }

// ActionURL is where a review form posts.
func (p Page) ActionURL(suffix string) string { return p.URL() + suffix }

// FindingAction is where a form about one finding posts.
func (p Page) FindingAction(number int32, suffix string) string {
	return p.URL() + "/findings/" + strconv.Itoa(int(number)) + suffix
}

// MessageAction is where a form about one message posts.
func (p Page) MessageAction(number, position int32, suffix string) string {
	return p.FindingAction(number, "/messages/"+strconv.Itoa(int(position))+suffix)
}

// FindingURL is a finding's own, stable address.
func (p Page) FindingURL(number int32) string {
	return p.URL() + "?finding=" + strconv.Itoa(int(number)) + "#" + FindingAnchor(number)
}

// FindingAnchor names a finding on the page.
func FindingAnchor(number int32) string { return "finding-" + strconv.Itoa(int(number)) }

// MessageAnchor names a message on the page.
func MessageAnchor(number, position int32) string {
	return "finding-" + strconv.Itoa(int(number)) + "-" + strconv.Itoa(int(position))
}

// PreviewHref opens the preview (at a relative path) through Previa.
func (p Page) PreviewHref(path string) string {
	href := prpages.OpenURL(p.RepoID, p.Number)
	if path != "" {
		href += "?path=" + url.QueryEscape(path)
	}
	return href
}

// EditURL shows an edit form without JavaScript.
func (p Page) EditURL(target, anchor string) string {
	return p.Findings.Filter.with("edit", target).url(p.URL()) + "#" + anchor
}

// RemoveURL asks to confirm a removal without JavaScript.
func (p Page) RemoveURL(target string) string {
	return p.Findings.Filter.with("remove", target).url(p.URL())
}

// Editing reports whether target's edit form starts open.
func (p Page) Editing(target string) bool {
	return p.Edit == target || p.Draft != nil && p.Draft.Form == "edit" && p.Draft.Target() == target ||
		(target == "instructions" || target == "test-access") && p.Draft != nil && p.Draft.Form == target
}

// Preview is what the pull request's preview serves now.
type Preview struct {
	// State: live, asleep, previous (an update failed; the previous
	// deployment still serves), removed, expired or none.
	State     string
	Address   string // the preview's origin, while one is running
	DocsPath  string // the deployment's declared docs page
	Latest    string // the newest commit on the pull request
	Deployed  string // the commit the preview serves; empty when none
	ExpiresAt string
	// Building is a newer commit being built or waiting to; Failed says the
	// latest commit's build failed.
	Building string
	Failed   bool
	Current  *Deployment // the newest deployment; nil before the first
	// The serving deployment's setup: declared, and how long it took.
	SetupDeclared bool
	SetupTook     string
	// SetupFailed: the newest commit's build failed in its setup command.
	SetupFailed bool
	// Data: the deployment runs managed databases, so it has test data.
	Data bool
	// DataSource is where its databases started (docs/32): kept, baseline
	// or empty; SeedDeclared, whether the repository declares seed.
	DataSource   string
	SeedDeclared bool
}

// DataLine is the reviewer's one-line version of where the test data
// started.
func (p Preview) DataLine() string {
	switch p.DataSource {
	case "kept":
		return "Kept from the previous version. A refresh or sleep keeps it; Reset test data replaces it."
	case "baseline":
		return "Reset to the repository baseline for this deployment. A new commit or Rebuild keeps what testers add."
	case "empty":
		if p.SeedDeclared {
			return "Fresh seeded data for this deployment."
		}
		return "Fresh from setup for this deployment."
	}
	return "Fresh from setup for each deployment. A refresh or sleep keeps it; a new commit, Rebuild or Reset replaces it."
}

// ResetForm rebuilds the serving commit to start again from the seed.
type ResetForm struct {
	ExpectedRevision int64
	RequestKey       string
	Commit           string
	// Baseline names the repository baseline a Reset may start from
	// ("main@a1b2c3"); empty when only empty databases apply.
	Baseline string
}

// TestAccess is how reviewers sign in to the repository's previews.
type TestAccess struct {
	Account        string // shown to everyone with access
	HasSecret      bool   // a password or token is stored (never shown)
	Revision       int64
	UpdatedBy      Person
	UpdatedAt      string
	LastRevealedBy Person
	LastRevealedAt string
	CanEdit        bool // Owners and Admins
	// Sealing says whether this server can store a secret at all.
	Sealing bool
	// Revealed is the secret, only in the response to a reveal.
	Revealed string
}

// Set reports test access worth showing.
func (t TestAccess) Set() bool { return t.Account != "" || t.HasSecret }

// Running reports whether the preview can be opened now.
func (p Preview) Running() bool {
	return p.Address != "" && (p.State == "live" || p.State == "asleep" || p.State == "previous")
}

// Outdated reports a newer commit than the deployed one.
func (p Preview) Outdated() bool { return p.Deployed != "" && p.Latest != "" && p.Deployed != p.Latest }

// Deployment is one publication of the pull request's preview.
type Deployment struct {
	Sequence    int32
	SHA         string
	PublishedAt string
	Backfilled  bool // already live when reviews were introduced
}

// Label names a deployment for people.
func (d Deployment) Label() string { return "Deployment " + strconv.Itoa(int(d.Sequence)) }

// Short is its commit, shortened.
func (d Deployment) Short() string {
	if len(d.SHA) > 7 {
		return d.SHA[:7]
	}
	return d.SHA
}

// Instructions are the pull request's testing instructions.
type Instructions struct {
	Summary, Steps, Expected string
	StartPath, DocsPath      string
	Revision                 int64
	UpdatedBy                Person
	UpdatedAt                string
}

// Empty reports instructions that say nothing.
func (i Instructions) Empty() bool {
	return strings.TrimSpace(i.Summary+i.Steps+i.Expected+i.StartPath+i.DocsPath) == ""
}

// StepList is the steps, one per non-blank line.
func (i Instructions) StepList() []string {
	var steps []string
	for _, line := range strings.Split(i.Steps, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			steps = append(steps, line)
		}
	}
	return steps
}

// Findings is the pull request's feedback conversation.
type Findings struct {
	Filter      Filter
	Open        int64
	Resolved    int64
	All         int64
	Total       int64 // matching the current filter
	Page, Pages int64
	Threads     []Thread
	Authors     []Person
	Deployments []Deployment
	// Focus shows one finding by its own link; FocusMissing says it does not
	// exist.
	Focus        int32
	FocusMissing bool
	// Full says the pull request reached its finding limit.
	Full  bool
	Limit int
	// ComposerKey makes a repeated submission of the new-finding form create
	// one finding.
	ComposerKey string
}

// Filtered reports whether any filter beyond the state narrows the list.
func (f Findings) Filtered() bool {
	return f.Filter.Author != 0 || f.Filter.Deployment != 0 || f.Filter.Search != ""
}

// Filter narrows the findings list.
type Filter struct {
	State      string
	Author     int64
	AuthorKey  string // the author's public ID, as addresses carry it
	Deployment int32
	Search     string
	Page       int64
	extra      url.Values
}

func (f Filter) with(key, value string) Filter {
	extra := url.Values{}
	for k, v := range f.extra {
		extra[k] = v
	}
	extra.Set(key, value)
	f.extra = extra
	return f
}

// Query is the filter as an address query, without the page.
func (f Filter) Query() url.Values {
	v := url.Values{}
	if f.State != "" && f.State != StateOpen {
		v.Set("state", f.State)
	}
	if f.AuthorKey != "" {
		v.Set("author", f.AuthorKey)
	}
	if f.Deployment > 0 {
		v.Set("deployment", strconv.Itoa(int(f.Deployment)))
	}
	if f.Search != "" {
		v.Set("q", f.Search)
	}
	return v
}

func (f Filter) url(base string) string {
	v := f.Query()
	if f.Page > 1 {
		v.Set("page", strconv.FormatInt(f.Page, 10))
	}
	for k, values := range f.extra {
		for _, value := range values {
			v.Add(k, value)
		}
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// StateURL lists findings in another state, keeping the other filters.
func (p Page) StateURL(state string) string {
	f := p.Findings.Filter
	f.State, f.Page = state, 1
	return f.url(p.URL())
}

// PageURL is another page of the current list.
func (p Page) PageURL(n int64) string {
	f := p.Findings.Filter
	f.Page = n
	return f.url(p.URL())
}

// ClearURL lists the current state without other filters.
func (p Page) ClearURL() string { return Filter{State: p.Findings.Filter.State}.url(p.URL()) }

// AllURL leaves a finding's own view for the whole list.
func (p Page) AllURL() string { return p.URL() + "?state=all" }

// ReturnQuery is the list a form returns to after it saves.
func (p Page) ReturnQuery() string {
	v := p.Findings.Filter.Query()
	if p.Findings.Filter.Page > 1 {
		v.Set("page", strconv.FormatInt(p.Findings.Filter.Page, 10))
	}
	if p.Findings.Focus > 0 {
		v.Set("finding", strconv.Itoa(int(p.Findings.Focus)))
	}
	return v.Encode()
}

// Thread is one finding and its conversation.
type Thread struct {
	Number         int32
	State          string
	Revision       int64
	Author         Person
	CreatedAt      string
	Deployment     *Deployment // what it was recorded against; nil before any deployment
	ResolvedOn     *Deployment // the deployment live when it was resolved
	StateChangedBy Person
	StateChangedAt string
	RemovalReason  string // set when a moderator removed it
	Messages       []Message
	// Earlier is how many replies the list leaves out; the finding's own
	// link shows them.
	Earlier int
	// What the viewer may do.
	CanResolve, CanReply, CanRemove, Moderate bool
	// ReplyKey makes a repeated submission of its reply form create one reply.
	ReplyKey string
}

// Path is the page the finding is about, from its opening message.
func (t Thread) Path() string { return t.Opening().Path }

// RemovedText says who removed a removed finding, and a moderator's reason.
func (t Thread) RemovedText() string {
	switch {
	case t.RemovalReason != "":
		return "Removed by " + t.StateChangedBy.Display() + " · " + ReasonLabel(t.RemovalReason)
	case t.StateChangedBy.ID == t.Author.ID:
		return "Removed by its author"
	}
	return "Removed by " + t.StateChangedBy.Display()
}

// Resolution says who resolved the finding and what they had in front of them.
func (t Thread) Resolution() string {
	text := "Resolved by " + t.StateChangedBy.Display()
	if t.ResolvedOn != nil {
		return text + " on " + t.ResolvedOn.Label()
	}
	return text + " while no preview was live"
}

// Opening is the message that opened the finding.
func (t Thread) Opening() Message {
	if len(t.Messages) == 0 {
		return Message{}
	}
	return t.Messages[0]
}

// Replies are the messages after the opening one that the list shows.
func (t Thread) Replies() []Message {
	if len(t.Messages) < 2 {
		return nil
	}
	return t.Messages[1:]
}

// Title is the finding's first line, for its heading.
func (t Thread) Title() string {
	o := t.Opening()
	if o.Removed {
		return "Removed finding"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(o.Body), "\n")
	if len([]rune(line)) > 120 {
		line = string([]rune(line)[:117]) + "…"
	}
	return line
}

// Older reports a finding recorded against an earlier deployment than the
// current one.
func (t Thread) Older(current *Deployment) bool {
	return t.Deployment != nil && current != nil && t.Deployment.Sequence < current.Sequence
}

// Message is one message of a finding.
type Message struct {
	Position             int32
	Author               Person
	Body, Path, Details  string
	Revision             int64
	CreatedAt, EditedAt  string
	Removed              bool
	RemovedBy            Person
	RemovalReason        string // a moderator's reason
	Deployment           *Deployment
	CanEdit, CanRemove   bool
	Moderate             bool // removing it is moderation
	DifferentDeployment  bool // written against another deployment than its finding
	RemovedByItsAuthor   bool
	RemovedWithItsThread bool
}

// RemovedText says who removed a message, and a moderator's reason.
func (m Message) RemovedText() string {
	switch {
	case m.RemovalReason != "":
		return "Message removed by " + m.RemovedBy.Display() + " · " + ReasonLabel(m.RemovalReason)
	case m.RemovedByItsAuthor:
		return "Message removed by its author"
	}
	return "Message removed by " + m.RemovedBy.Display()
}

// Person is someone in a review.
type Person struct {
	ID          int64
	Login, Name string
	Avatar      string
	Former      bool // no longer an active member
	You         bool
}

// Display is how the person is named.
func (p Person) Display() string {
	switch {
	case p.ID == 0:
		return "Previa"
	case p.Name != "":
		return p.Name
	case p.Login != "":
		return p.Login
	}
	return "Former member"
}

// Draft is a review form shown again with what was written.
type Draft struct {
	Form             string // finding, reply, edit or instructions
	Finding          int32  // reply and edit
	Position         int32  // edit
	Body, Path       string
	Details          string
	Instructions     Instructions
	RequestKey       string
	Revision         int64
	Deployment       int32 // the deployment the draft was written against
	Errors           map[string]string
	Error            string
	Conflict         *Conflict
	AcknowledgeOlder bool
}

// Target names the message an edit draft belongs to.
func (d Draft) Target() string {
	return strconv.Itoa(int(d.Finding)) + "." + strconv.Itoa(int(d.Position))
}

// For reports whether the draft belongs to form (and the finding or
// message, when it has one).
func (d *Draft) For(form string, finding, position int32) bool {
	return d != nil && d.Form == form && d.Finding == finding && d.Position == position
}

// FieldError is the message for one field of the draft.
func (d *Draft) FieldError(name string) string {
	if d == nil {
		return ""
	}
	return d.Errors[name]
}

// Conflict explains why a draft could not be saved as it was.
type Conflict struct {
	// Kind: version (a newer deployment was published), edited (the message
	// changed meanwhile) or instructions (someone saved them meanwhile).
	Kind    string
	Tested  *Deployment // the deployment the draft was written against
	Current *Deployment
	Saved   Instructions
}

// RemoveTarget is a removal waiting for confirmation without JavaScript.
type RemoveTarget struct {
	Finding  int32
	Position int32 // 0 removes the whole finding
	Moderate bool
	Action   string
}

// PublicReferences lists the resources the page links to by public ID.
func (p Page) PublicReferences() []components.ResourceRef {
	refs := []components.ResourceRef{{Kind: "repository", ID: p.RepoID}, {Kind: "pull-request", ID: int64(p.Number), Parent: p.RepoID}}
	for _, a := range p.Findings.Authors {
		refs = append(refs, components.ResourceRef{Kind: "user", ID: a.ID})
	}
	return refs
}

// DocumentTitle is the browser tab's title.
func (p Page) DocumentTitle() string {
	return "Review · #" + strconv.Itoa(int(p.Number)) + " " + p.Title
}

// RepositoryName is the repository's name without its owner.
func (p Page) RepositoryName() string {
	if _, name, found := strings.Cut(p.Repository, "/"); found {
		return name
	}
	return p.Repository
}

// TrailCurrent names the page in the header trail.
func (p Page) TrailCurrent() string { return "#" + strconv.Itoa(int(p.Number)) + " " + p.Title }

// CurrentURL is this page with its list: what a live refresh fetches and
// where Cancel returns.
func (p Page) CurrentURL() string {
	if q := p.ReturnQuery(); q != "" {
		return p.URL() + "?" + q
	}
	return p.URL()
}

// FindingLink is a finding's shareable, absolute address.
func (p Page) FindingLink(number int32) string {
	if p.AbsoluteURL == "" {
		return ""
	}
	return p.AbsoluteURL + "?finding=" + strconv.Itoa(int(number)) + "#" + FindingAnchor(number)
}

// ShownDeployment is the deployment new findings, replies and resolutions
// are recorded against: the newest one (0 before the first).
func (p Page) ShownDeployment() int32 {
	if p.Preview.Current == nil {
		return 0
	}
	return p.Preview.Current.Sequence
}

// ClosedWord says how the pull request ended.
func (p Page) ClosedWord() string {
	if p.State == "merged" {
		return "merged"
	}
	return "closed"
}

// Status is the preview's label and tone.
func (p Preview) Status() (string, string) {
	switch p.State {
	case "live":
		return "Live", "tone-ok"
	case "asleep":
		return "Asleep", "tone-neutral"
	case "previous":
		return "Previous deployment", "tone-warn"
	case "expired":
		return "Expired", "tone-neutral"
	case "removed":
		return "Removed", "tone-neutral"
	}
	return "Not deployed", "tone-neutral"
}

// Note explains how the preview relates to the newest commit, when it
// differs.
func (p Preview) Note() string {
	short := func(sha string) string {
		if len(sha) > 7 {
			return sha[:7]
		}
		return sha
	}
	failed := "failed to build"
	if p.SetupFailed {
		failed = "failed in its setup command"
	}
	switch {
	case p.Building != "":
		return "Commit " + short(p.Building) + " is being built; this page updates when it is live."
	case (p.State == "previous" || p.Failed) && p.Deployed != "":
		return "The newest commit " + short(p.Latest) + " " + failed + ", so this is the previous deployment."
	case p.Failed:
		return "The newest commit " + short(p.Latest) + " " + failed + "; nothing is deployed."
	case p.Outdated():
		return "The newest commit " + short(p.Latest) + " is not deployed yet."
	case p.State == "expired":
		return "The preview expired. Findings stay here; a new build brings it back."
	case p.State == "removed":
		return "The preview was removed. Findings stay here."
	case p.State == "asleep":
		return "Asleep while idle; opening it wakes it in a few seconds."
	}
	return ""
}
