package onboarding

import (
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// Step states, in the order a person meets them.
const (
	StateDone    = "done"    // true now, from saved state
	StateCurrent = "current" // the next thing to do
	StateWorking = "working" // Previa is on it (a check or build in flight)
	StateProblem = "problem" // something must be fixed first
	StateWaiting = "waiting" // after an earlier step
)

// Page is the Setup tab of one repository: a checklist for one open pull
// request, from connection to a preview opened from Previa.
type Page struct {
	RepoID   int64
	Name     string // owner/name
	CSRF     string
	LiveURL  string
	Notice   Notice
	Toast    string
	Mode     string // the repository's build mode, for the finished state
	Manager  bool   // Owner/Admin: connection and secrets controls apply
	Discover bool   // the server can ask GitHub (the App is configured)

	Options   []Option // open pull requests to choose from, newest first
	OpenCount int64    // every open pull request
	Selected  *PR      // nil: the repository has no open pull request
	Sync      Sync

	Steps []Step
	Facts Facts
	Check *CheckForm // nil when no check can be requested
	Build *BuildForm // nil when no build action applies
	Next  []Link     // after the preview is opened
	// RefreshAt is when a waiting state turns stale with no database change
	// to announce it (a check nobody picked up).
	RefreshAt string
}

// Notice explains the outcome of the last action, next to the checklist.
type Notice struct{ Tone, Text string }

// Option is one open pull request in the picker.
type Option struct {
	Number        int32
	Title, Branch string
}

// PR is the selected pull request.
type PR struct {
	Number        int32
	Title, Branch string
	SHA           string
	Fork          bool
}

// Sync is the state of the pull request list refresh, for a repository
// without an open pull request.
type Sync struct {
	Running bool
	Last    string // RFC 3339, the last completed refresh
}

// Step is one checklist row.
type Step struct {
	ID      string
	Title   string
	State   string
	Summary string
	When    string // RFC 3339 time behind WhenLabel ("Checked", "Published")
	Label   string
	Items   []Item
	Links   []Link
	Form    string // "check", "build" or "sync": the page form this step offers
	// Blocked is why the step's form cannot run now; it is then shown
	// disabled with this note.
	Blocked string
	// Failure is a failed build's stored category; Headline explains it in
	// the pull request page's words.
	Failure string
}

// Headline is the step's one-line summary.
func (s Step) Headline() string {
	if s.Failure != "" {
		d := prpages.Diagnose(s.Failure, nil)
		return d.Title + ". " + d.Summary
	}
	return s.Summary
}

// Item is one finding or fact inside a step.
type Item struct {
	Tone   string // ok, info, warn, danger
	Title  string
	Detail string
	Field  string // a .previewrc.yaml key, shown as code
	Line   int
}

// Link is an action that navigates.
type Link struct {
	Label    string
	Href     string
	Icon     string // kit, logs, open, repo, members, secrets, pr, settings
	Primary  bool
	External bool // opens in a new tab
}

// Facts are what the commit declares its app needs, beside the checklist.
type Facts struct {
	SHA     string // the commit they come from; empty before a check
	Rows    []Fact
	Secrets Secrets
	Fork    bool
}

// Fact is one label/value row. Note is muted text after the value.
type Fact struct {
	Label, Value, Note string
	Code               bool // the value is a path, command or key
}

// Secrets says which runtime values the repository has, or who manages them.
type Secrets struct {
	Manager    bool
	Configured bool // the server can store secrets
	Names      []string
	Managers   []Person
	Href       string
}

// Person is a teammate named on the page.
type Person struct{ Login, Name, Role string }

// CheckForm asks for a setup check of the selected commit.
type CheckForm struct {
	Label   string
	Primary bool
}

// BuildForm requests a build through the pull request's Build action.
type BuildForm struct {
	Label            string
	Action           string // build, retry or rebuild
	ExpectedRevision int64
	RequestKey       string
}

// URL is the Setup tab of a repository, optionally for one pull request.
func URL(repoID int64, number int32) string {
	u := "/repositories/" + strconv.FormatInt(repoID, 10) + "/setup"
	if number > 0 {
		u += "?pr=" + strconv.Itoa(int(number))
	}
	return u
}

// CheckURL is where the selected commit's check is requested.
func (p Page) CheckURL() string {
	return "/repositories/" + strconv.FormatInt(p.RepoID, 10) + "/setup/check?pr=" + strconv.Itoa(int(p.Selected.Number))
}

// BuildURL is the selected pull request's Build action.
func (p Page) BuildURL() string {
	return "/repositories/" + strconv.FormatInt(p.RepoID, 10) + "/pull-requests/" + strconv.Itoa(int(p.Selected.Number)) + "/build"
}

// SyncURL refreshes the repository's pull requests from GitHub.
func (p Page) SyncURL() string {
	return "/repositories/" + strconv.FormatInt(p.RepoID, 10) + "/refresh"
}

// ReturnQuery brings a form posted elsewhere back to this page and pull request.
func (p Page) ReturnQuery() string {
	if p.Selected == nil {
		return ""
	}
	return "pr=" + strconv.Itoa(int(p.Selected.Number))
}

// Owner is the account part of the repository's name.
func (p Page) Owner() string {
	owner, _, _ := strings.Cut(p.Name, "/")
	return owner
}

// Repository is the name without its account.
func (p Page) Repository() string {
	if _, name, ok := strings.Cut(p.Name, "/"); ok {
		return name
	}
	return p.Name
}

// Offers reports whether the picker lists a pull request.
func (p Page) Offers(number int32) bool {
	for _, o := range p.Options {
		if o.Number == number {
			return true
		}
	}
	return false
}

// Done counts finished steps.
func (p Page) Done() int {
	n := 0
	for _, s := range p.Steps {
		if s.State == StateDone {
			n++
		}
	}
	return n
}

// Complete reports whether every step is done.
func (p Page) Complete() bool { return len(p.Steps) > 0 && p.Done() == len(p.Steps) }

// Short is the first seven characters of a commit SHA.
func Short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// PublicReferences lists the resources the page addresses.
func (p Page) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("repository", p.RepoID)
	for _, o := range p.Options {
		refs = append(refs, components.PRRefs(p.RepoID, o.Number)...)
	}
	if p.Selected != nil {
		refs = append(refs, components.PRRefs(p.RepoID, p.Selected.Number)...)
	}
	return refs
}
