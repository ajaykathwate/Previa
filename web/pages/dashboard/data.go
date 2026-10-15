package dashboard

import "github.com/ajaykathwatee/previa/web/components"

import prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"

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
}

// Step is one item of the getting-started checklist.
type Step struct {
	Done                        bool
	Title, Detail, Href, Action string
}

// Setup is the getting-started checklist, from real saved state.
func (s Summary) Setup() []Step {
	return []Step{
		{true, "Sign in with GitHub", "Your Previa session is active.", "", ""},
		{s.Installations > 0, "Install the GitHub App", "Choose which repositories Previa may read.", "/repositories", "Open repositories"},
		{s.Repositories > 0, "Link repositories", "Verified repositories appear in your directory.", "/repositories?access=all", "View directory"},
		{s.OpenPRs > 0, "Track pull requests", "Open a repository and choose Sync Pull Requests; GitHub keeps the list current after that.", "/repositories?access=available", "Pick a repository"},
		{s.Live > 0 || len(s.Builds) > 0, "Build a preview", "Open a pull request and choose Build latest commit, or turn on Automatic builds.", "/repositories?access=available", "Pick a repository"},
	}
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
	var refs []components.ResourceRef
	for _, rows := range [][]prpages.ActivityRow{s.Builds, s.Previews} {
		for _, r := range rows {
			refs = append(refs, components.PRRefs(r.RepoID, r.Number)...)
		}
	}
	return refs
}
