package dashboard

import (
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// HighlightRows is how many recent builds and live previews the Overview lists.
const HighlightRows = 5

// Summary contains database-derived workspace counts, scoped to the signed-in
// user, and what the running workers can do.
type Summary struct {
	Installations, Repositories, Available int64
	// Open pull requests, and their current requests and previews. Failed
	// counts failures that need attention (recent, not dismissed);
	// FailedOlder the undismissed ones older than AttentionDays.
	OpenPRs, Waiting, Building, Failed, Live int64
	FailedOlder                              int64
	AttentionDays                            int
	// Capabilities of the workers that are running now.
	Workers Workers
	// The latest build requests and published previews, newest first.
	Builds, Previews []prpages.ActivityRow
	// DeveloperTools: the queue diagnostics page is served (not in production).
	DeveloperTools bool
	// Journey is where the signed-in person's first preview stands.
	Journey Journey
	// Joined: the person arrived from accepting an invitation.
	Joined bool
}

// Journey is the first-preview path for the signed-in person, across the
// workspace's repositories they can build in.
type Journey struct {
	Manager                    bool  // Owner or Admin: can connect repositories
	Repositories, Available    int64 // repositories this person can build in
	Checked, Published, Opened bool
	FocusID                    int64 // the repository whose Setup tab continues the path
	FocusName                  string
	Managers                   []Person // whom a Developer without repositories asks
}

// Person is a teammate named on the Overview.
type Person struct{ Login, Name string }

// Step is one item of the getting-started checklist.
type Step struct {
	Done                        bool
	Title, Detail, Href, Action string
}

// Setup is the path to a first usable preview, from real saved state and the
// person's role: Owners and Admins connect code; Developers are given access.
// Each later step continues on a repository's Setup tab.
func (s Summary) Setup() []Step {
	j := s.Journey
	setup, where := "/repositories?access=available", "Choose a repository"
	if j.FocusID > 0 {
		setup, where = "/repositories/"+strconv.FormatInt(j.FocusID, 10)+"/setup", "Open setup"
	}
	first := Step{Done: j.Repositories > 0, Title: "Connect a repository", Detail: "Install the GitHub App on the repositories that should get previews.", Href: "/repositories", Action: "Open repositories"}
	if !j.Manager {
		first = Step{Done: j.Repositories > 0, Title: "Get access to a repository", Detail: "An Owner or Admin chooses which repositories you can build. " + ask(j.Managers)}
	}
	check := "Pick an open pull request; Previa checks its configuration and Dockerfile without building."
	if j.FocusName != "" && !j.Checked {
		check = "Continue in " + j.FocusName + ": pick an open pull request and check its configuration, without building."
	}
	return []Step{
		first,
		{Done: j.Checked, Title: "Check a pull request's setup", Detail: check, Href: setup, Action: where},
		{Done: j.Published, Title: "Publish a preview", Detail: "Build it from the checklist once its setup check passes.", Href: setup, Action: where},
		{Done: j.Opened, Title: "Open it from Previa", Detail: "Open the live preview from Previa; it is then ready to share for review.", Href: setup, Action: where},
	}
}

// ask names the people a Developer asks for repository access.
func ask(people []Person) string {
	if len(people) == 0 {
		return "Ask an Owner or Admin of this workspace."
	}
	names := make([]string, 0, len(people))
	for _, p := range people {
		names = append(names, p.Name+" (@"+p.Login+")")
	}
	return "Ask " + strings.Join(names, " or ") + "."
}

// SetupDone hides the checklist once every step is done.
func (s Summary) SetupDone() bool {
	for _, step := range s.Setup() {
		if !step.Done {
			return false
		}
	}
	return true
}

// WorkerNote explains what the running workers can do and, when they cannot
// build, what fixes it.
func (s Summary) WorkerNote() (text string, attention bool) {
	w := s.Workers
	switch {
	case w.Execution:
		return "A running worker builds, publishes and reports previews. Nothing on this page is simulated.", false
	case w.Paused:
		return "Builds are paused by the operator (BUILDS_PAUSED). Teardown, cleanup and reports keep running; queued requests start when builds resume.", true
	case w.Discovery || w.Reporting || w.Housekeeping:
		return "A worker is running but cannot build previews: it has no preview domain. Set PREVIEW_BASE_DOMAIN on the worker (preview.localtest.me works on one machine), start Docker and the router (make egress-image router-up), then restart the worker.", true
	default:
		return "No worker that can build previews is running. Start one (make run-worker during development) with the GitHub App, Docker and a preview domain configured. Build requests wait until it does.", true
	}
}

// Workers says which pipeline stages a running worker provides.
type Workers struct{ Discovery, Execution, Reporting, Housekeeping, Paused bool }

// Unavailable counts linked repositories whose access or installation is inactive.
func (s Summary) Unavailable() int64 { return max(0, s.Repositories-s.Available) }

// FailuresURL is where the Overview's failure figures lead: the failures that
// need attention, else the older ones, else every build.
func (s Summary) FailuresURL() string {
	switch {
	case s.Failed > 0:
		return "/builds?failures=attention"
	case s.FailedOlder > 0:
		return "/builds?failures=older"
	}
	return "/builds"
}

func (s Summary) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("repository", s.Journey.FocusID)
	for _, rows := range [][]prpages.ActivityRow{s.Builds, s.Previews} {
		for _, r := range rows {
			refs = append(refs, components.PRRefs(r.RepoID, r.Number)...)
		}
	}
	return refs
}
