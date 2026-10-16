package onboarding

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/onboarding/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	pages "github.com/ajaykathwatee/previa/web/pages/onboarding"
	"github.com/jackc/pgx/v5/pgtype"
)

// view is everything one Setup page is decided from. The service loads it;
// derive turns it into the checklist, so every step reflects current state.
type view struct {
	now           time.Time
	viewer        int64
	repo          dbgen.SetupRepositoryRow
	pr            *dbgen.SetupPullRequestRow // nil without an open pull request
	opened        *dbgen.LatestOpenRow       // the latest open of the deployed commit
	caps          workers.Capabilities
	fingerprint   string // the platform checks are made against now
	secrets       []string
	secretsOn     bool // the server can store secrets
	managers      []pages.Person
	discover      bool // the server has the GitHub App
	production    bool
	previewDomain string
}

// Step identities, in checklist order.
const (
	stepConnection    = "connection"
	stepConfiguration = "configuration"
	stepRequirements  = "requirements"
	stepPreview       = "preview"
	stepOpened        = "opened"
)

func (v view) connected() bool {
	return v.discover && v.repo.Accessible && v.repo.InstallationStatus == "active"
}

func (v view) result() (Result, bool) {
	if v.pr == nil || v.pr.ResultSha == "" {
		return Result{}, false
	}
	return decodeResult(v.pr.Result)
}

// live reports whether the pull request has a preview deployed, healthy or not.
func (v view) live() bool {
	return v.pr != nil && v.pr.DeployedSha != "" && !v.pr.TeardownRequested &&
		(v.pr.EnvironmentStatus == environments.StatusRunning || v.pr.EnvironmentStatus == environments.StatusFailed || v.pr.EnvironmentStatus == "publishing")
}

// liveAtHead reports whether the preview serves the pull request's head.
func (v view) liveAtHead() bool { return v.live() && v.pr.DeployedSha == v.pr.HeadSha }

// builtHead reports whether the head commit already built and published,
// which proves its configuration valid without a separate check.
func (v view) builtHead() bool {
	return v.liveAtHead() || v.pr != nil && v.pr.RequestCurrent && v.pr.BuildState == "succeeded"
}

// requested reports whether someone asked to build the head commit.
func (v view) requested() bool {
	if v.pr == nil || !v.pr.RequestCurrent {
		return v.liveAtHead()
	}
	switch v.pr.BuildState {
	case "pending", "queued", "running", "succeeded", "failed":
		return true
	}
	return v.liveAtHead()
}

// checkInFlight reports a check of the head that is queued or running, and
// whether it waited past CheckStaleAfter.
func (v view) checkInFlight() (inFlight, stale bool) {
	if v.pr == nil || v.pr.CheckSha != v.pr.HeadSha || v.pr.CheckState != "queued" && v.pr.CheckState != "running" {
		return false, false
	}
	return true, v.pr.CheckRequestedAt.Valid && v.now.Sub(v.pr.CheckRequestedAt.Time) > CheckStaleAfter
}

// configValid reports whether the head commit's configuration is known good:
// it built, or a current check passed against today's platform.
func (v view) configValid() bool {
	if v.builtHead() {
		return true
	}
	r, ok := v.result()
	return ok && r.Passed() && v.pr.ResultSha == v.pr.HeadSha && v.pr.CheckState == "complete" && v.pr.CheckPlatform == v.fingerprint
}

// derive builds the page's checklist, facts and forms.
func (v view) derive(p *pages.Page) {
	p.Manager = v.repo.CanManage
	p.Discover = v.discover
	connection := v.connectionStep()
	p.Steps = append(p.Steps, connection)
	if v.pr == nil {
		p.Steps = append(p.Steps,
			pages.Step{ID: stepConfiguration, Title: "Check the configuration", State: pages.StateWaiting, Summary: "Waiting for an open pull request."},
			pages.Step{ID: stepRequirements, Title: "Review what the app needs", State: pages.StateWaiting, Summary: "After the configuration check."},
			pages.Step{ID: stepPreview, Title: "Publish a preview", State: pages.StateWaiting, Summary: "After the configuration check."},
			pages.Step{ID: stepOpened, Title: "Open it from Previa", State: pages.StateWaiting, Summary: "After the preview is published."})
		return
	}
	p.Selected = &pages.PR{Number: v.pr.Number, Title: v.pr.Title, Branch: v.pr.HeadBranch, SHA: v.pr.HeadSha, Fork: v.pr.IsFork}
	p.Steps = append(p.Steps, v.configurationStep(connection.State == pages.StateDone, p))
	p.Facts = v.facts()
	p.Steps = append(p.Steps, v.requirementsStep(p))
	p.Steps = append(p.Steps, v.previewStep(p))
	p.Steps = append(p.Steps, v.openedStep())
	if p.Steps[len(p.Steps)-1].State == pages.StateDone {
		p.Next = v.nextLinks()
	}
	// A step after an unfinished one waits for it, whatever it found.
	for i := 1; i < len(p.Steps); i++ {
		if before := p.Steps[i-1].State; before != pages.StateDone && p.Steps[i].State != pages.StateDone {
			p.Steps[i].State = pages.StateWaiting
			p.Steps[i].Form, p.Steps[i].Blocked = "", ""
			p.Steps[i].Links = nil
		}
	}
	// Forms are offered only where a step shows them.
	check, build := false, false
	for _, s := range p.Steps {
		check = check || s.Form == "check"
		build = build || s.Form == "build"
	}
	if !check {
		p.Check = nil
	}
	if !build {
		p.Build = nil
	}
}

func (v view) connectionStep() pages.Step {
	s := pages.Step{ID: stepConnection, Title: "Connect the repository"}
	switch {
	case !v.discover:
		s.State = pages.StateProblem
		s.Summary = "Previa cannot read repositories right now."
		if !v.production {
			s.Items = []pages.Item{{Tone: "warn", Title: "This server has no GitHub App configured.", Detail: "Set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH on the server and the worker, then restart them."}}
		}
	case v.connected():
		s.State = pages.StateDone
		s.Summary = "Previa can read this repository, and your role can build its previews."
	default:
		s.State = pages.StateProblem
		s.Summary = "GitHub access to this repository is unavailable."
		if v.repo.CanManage {
			s.Items = []pages.Item{{Tone: "warn", Title: "The GitHub App can no longer read it.", Detail: "Choose the repository again in the GitHub App's settings; Previa picks the change up by itself."}}
			s.Links = []pages.Link{{Label: "Unavailable repositories", Href: "/repositories?access=unavailable", Icon: "repo", Primary: true}}
		} else {
			s.Items = []pages.Item{{Tone: "warn", Title: "An Owner or Admin can restore access.", Detail: peopleSentence(v.managers)}}
		}
	}
	return s
}

func (v view) configurationStep(connected bool, p *pages.Page) pages.Step {
	s := pages.Step{ID: stepConfiguration, Title: "Check the configuration"}
	head := pages.Short(v.pr.HeadSha)
	p.Check = &pages.CheckForm{Label: "Check setup", Primary: true}
	s.Form = "check"
	if !connected {
		s.State, s.Summary = pages.StateWaiting, "After the repository is connected."
		return s
	}
	if v.builtHead() {
		s.State = pages.StateDone
		s.Summary = "Commit " + head + " built, so its configuration is valid."
		s.Form = ""
		return s
	}
	inFlight, stale := v.checkInFlight()
	switch {
	case inFlight && !stale:
		s.State = pages.StateWorking
		s.Summary = "Checking " + head + "…"
		s.When, s.Label = stamp(v.pr.CheckRequestedAt), "Requested"
		s.Form = ""
		p.RefreshAt = v.pr.CheckRequestedAt.Time.Add(CheckStaleAfter).UTC().Format(time.RFC3339)
		if !v.caps.Discovery {
			s.Items = []pages.Item{{Tone: "info", Title: v.delayedTitle(), Detail: v.delayedDetail()}}
		}
		return s
	case inFlight:
		s.State = pages.StateProblem
		s.Summary = "The check of " + head + " did not finish."
		s.Items = []pages.Item{{Tone: "warn", Title: v.delayedTitle(), Detail: v.delayedDetail()}}
		p.Check.Label = "Check again"
		return s
	case v.pr.CheckSha == v.pr.HeadSha && v.pr.CheckState == "failed" && v.pr.CheckFailure != FailStaleCommit:
		s.State = pages.StateProblem
		title, detail := failureText(v.pr.CheckFailure)
		s.Summary = "The check of " + head + " could not finish."
		s.When, s.Label = stamp(v.pr.CheckFinishedAt), "Tried"
		s.Items = []pages.Item{{Tone: "warn", Title: title, Detail: detail}}
		if r, ok := v.result(); ok && v.pr.ResultSha == v.pr.HeadSha {
			outcome := "it passed"
			if !r.Passed() {
				outcome = "it found problems"
			}
			s.Items = append(s.Items, pages.Item{Tone: "info", Title: "Last completed check " + ago(v.now, v.pr.CheckedAt) + ": " + outcome + ".",
				Detail: "That result is kept for reference; check again to confirm it."})
		}
		p.Check.Label = "Try again"
		return s
	}
	r, ok := v.result()
	switch {
	case !ok || v.pr.ResultSha != v.pr.HeadSha:
		s.State = pages.StateCurrent
		s.Summary = "Not checked yet. Previa reads .previewrc.yaml and your Dockerfile at " + head + "; nothing is built or run."
		if ok {
			s.Items = []pages.Item{{Tone: "info", Title: "The last check was for an earlier commit, " + pages.Short(v.pr.ResultSha) + "."}}
		}
	case v.pr.CheckPlatform != v.fingerprint:
		s.State = pages.StateCurrent
		s.Summary = "The services this server offers changed since " + head + " was checked. Check it again."
		p.Check.Label = "Check again"
	case r.Passed():
		// Its commit cannot change, so a passing check needs no repeat.
		s.State, s.Form = pages.StateDone, ""
		s.When, s.Label = stamp(v.pr.CheckedAt), "Checked"
		s.Summary = "Valid .previewrc.yaml and Dockerfile at " + head + "."
		if path := dockerfilePath(r); path != "Dockerfile" {
			s.Summary = "Valid .previewrc.yaml, and the Dockerfile " + quote(path) + " exists at " + head + "."
		}
		s.Items = passedNotes(r)
	default:
		s.State = pages.StateProblem
		s.When, s.Label = stamp(v.pr.CheckedAt), "Checked"
		s.Summary = head + " can't build yet. Fix these on the pull request's branch, push, then check again."
		s.Items, s.Links = problems(r, head)
		// The fix comes first; checking the same commit again changes nothing.
		p.Check = &pages.CheckForm{Label: "Check again"}
	}
	if !v.caps.Discovery {
		s.Blocked = v.delayedTitle()
	}
	return s
}

func (v view) requirementsStep(p *pages.Page) pages.Step {
	s := pages.Step{ID: stepRequirements, Title: "Review what the app needs"}
	if !v.configValid() {
		s.State, s.Summary = pages.StateWaiting, "After the configuration check."
		return s
	}
	if _, checked := v.result(); checked && v.pr.ResultSha == v.pr.HeadSha {
		s.Summary = "Confirm the port, health check, services and secrets beside this list match what the app expects, then build."
	} else {
		s.Summary = "This commit built before, so it can build again; check the configuration to list what it declares."
	}
	if v.pr.IsFork {
		s.Items = append(s.Items, pages.Item{Tone: "warn", Title: "This pull request comes from a fork.", Detail: "Its preview gets no repository secrets and no outbound connections, so features that need them won't work in it."})
	}
	if v.repo.CanManage && v.secretsOn && len(v.secrets) == 0 && !v.pr.IsFork {
		s.Items = append(s.Items, pages.Item{Tone: "info", Title: "No secrets are stored for this repository.", Detail: "If the app needs API keys or a database URL, add them on the Secrets tab before building."})
	}
	s.Items = append(s.Items, pages.Item{Tone: "info", Title: "Checked during the build", Detail: "Whether the app starts, listens on its port and answers its health check, and whether setup commands and services work."})
	if v.requested() {
		s.State = pages.StateDone
		s.Summary = "Confirmed by building " + pages.Short(v.pr.HeadSha) + "."
		s.Items = nil
		return s
	}
	s.State = pages.StateCurrent
	s.Form = "build"
	p.Build = &pages.BuildForm{Label: "Build preview", Action: string(environments.BuildLatest), ExpectedRevision: v.pr.RequestRevision, RequestKey: requestKey()}
	s.Blocked = v.buildBlocked()
	return s
}

func (v view) previewStep(p *pages.Page) pages.Step {
	s := pages.Step{ID: stepPreview, Title: "Publish a preview"}
	head := pages.Short(v.pr.HeadSha)
	pr := "/repositories/" + strconv.FormatInt(v.repo.ID, 10) + "/pull-requests/" + strconv.Itoa(int(v.pr.Number))
	logs := pages.Link{Label: "Build log", Href: pr + "/logs", Icon: "logs"}
	switch {
	case v.liveAtHead():
		s.State = pages.StateDone
		s.When, s.Label = stamp(v.pr.PublishedAt), "Published"
		s.Summary = "Live at " + v.previewURL() + ", serving " + head + "."
		if v.pr.EnvironmentStatus == environments.StatusFailed {
			s.State = pages.StateProblem
			s.Summary = "The preview of " + head + " stopped."
			s.Links = []pages.Link{{Label: "Pull request", Href: pr, Icon: "pr", Primary: true}, logs}
		}
		return s
	case !v.requested():
		s.State, s.Summary = pages.StateWaiting, "After you build."
		if v.live() {
			s.Summary = "A preview of an earlier commit, " + pages.Short(v.pr.DeployedSha) + ", is live; build " + head + " to update it."
		}
		return s
	}
	switch v.pr.BuildState {
	case "pending", "queued":
		s.State = pages.StateWorking
		s.Summary = "Queued to build " + head + "."
		if v.pr.WaitingReason != "" {
			s.Summary = environments.WaitingTitle(v.pr.WaitingReason) + ". " + environments.WaitingMessage(v.pr.WaitingReason)
		}
		s.Links = []pages.Link{logs}
	case "running":
		s.State = pages.StateWorking
		s.When, s.Label = stamp(v.pr.BuildStartedAt), "Started"
		s.Summary = "Building " + head + ". The checklist moves on when it is live."
		s.Links = []pages.Link{{Label: "Follow the build log", Href: pr + "/logs", Icon: "logs", Primary: true}}
	case "succeeded":
		s.State = pages.StateWorking
		s.Summary = "Built " + head + "; publishing its address."
	case "failed":
		s.State = pages.StateProblem
		s.When, s.Label = stamp(v.pr.BuildCompletedAt), "Failed"
		s.Failure = buildFailure(v.pr.ExecutionErrorCode, v.pr.EnvironmentError, v.pr.FailureCurrent)
		s.Links = []pages.Link{{Label: "What went wrong", Href: pr, Icon: "pr"}, logs}
		s.Form = "build"
		p.Build = &pages.BuildForm{Label: "Retry build", Action: string(environments.RetryBuild), ExpectedRevision: v.pr.RequestRevision, RequestKey: requestKey()}
		s.Blocked = v.buildBlocked()
	}
	return s
}

func (v view) openedStep() pages.Step {
	s := pages.Step{ID: stepOpened, Title: "Open it from Previa"}
	if !v.liveAtHead() || v.pr.EnvironmentStatus == environments.StatusFailed {
		s.State, s.Summary = pages.StateWaiting, "After the preview is published."
		return s
	}
	open := OpenURL(v.repo.ID, v.pr.Number)
	if v.opened == nil {
		s.State = pages.StateCurrent
		s.Summary = "Open the preview from here to finish. Previa records that it was opened, not whether it works."
		s.Links = []pages.Link{{Label: "Open preview", Href: open, Icon: "open", Primary: true, External: true}}
		return s
	}
	who := v.opened.DisplayName
	if v.opened.UserID == v.viewer {
		who = "you"
	}
	s.State = pages.StateDone
	s.When, s.Label = stamp(v.opened.OpenedAt), "Opened"
	s.Summary = "Opened by " + who + ". The preview is ready for review."
	s.Links = []pages.Link{{Label: "Open preview", Href: open, Icon: "open", External: true}}
	return s
}

// nextLinks are what to do once the first preview is open: write the
// testing instructions on the pull request's review page and bring in
// reviewers.
func (v view) nextLinks() []pages.Link {
	pr := "/repositories/" + strconv.FormatInt(v.repo.ID, 10) + "/pull-requests/" + strconv.Itoa(int(v.pr.Number))
	links := []pages.Link{{Label: "Write testing instructions", Href: pr + "/review", Icon: "review", Primary: true}, {Label: "Pull request", Href: pr, Icon: "pr"}}
	if v.repo.CanManage {
		links = append(links, pages.Link{Label: "Invite a reviewer", Href: "/workspaces/" + strconv.FormatInt(v.repo.WorkspaceID, 10) + "/members", Icon: "members"})
	}
	return links
}

// facts are what the checked commit declares, for the side panel.
func (v view) facts() pages.Facts {
	f := pages.Facts{Fork: v.pr.IsFork, Secrets: pages.Secrets{Manager: v.repo.CanManage, Configured: v.secretsOn, Names: v.secrets, Managers: v.managers,
		Href: "/repositories/" + strconv.FormatInt(v.repo.ID, 10) + "/secrets"}}
	r, ok := v.result()
	if !ok || v.pr.ResultSha != v.pr.HeadSha || r.Config != ConfigFound {
		return f
	}
	f.SHA = v.pr.ResultSha
	health := r.HealthCheckPath
	healthNote := "checked during the build"
	if health == "" {
		health, healthNote = "Not set", "the build waits for the port to answer"
	}
	context := pages.Fact{Label: "Build context", Value: r.Context, Code: true}
	if r.Context == "." {
		context = pages.Fact{Label: "Build context", Value: "Repository root"}
	}
	f.Rows = []pages.Fact{
		{Label: "Port", Value: strconv.Itoa(r.Port), Code: true},
		{Label: "Health check", Value: health, Note: healthNote, Code: r.HealthCheckPath != ""},
		context,
		{Label: "Dockerfile", Value: dockerfilePath(r), Code: true},
		{Label: "Size", Value: tierName(r.ResourceTier)},
	}
	if len(r.Services) > 0 {
		f.Rows = append(f.Rows, pages.Fact{Label: "Services", Value: servicesSentence(r.Services)})
	}
	if len(r.Setup) > 0 {
		f.Rows = append(f.Rows, pages.Fact{Label: "Setup command", Value: strings.Join(r.Setup, " "), Note: "runs before the app starts", Code: true})
	}
	for _, process := range r.Processes {
		f.Rows = append(f.Rows, pages.Fact{Label: "Process " + process.Name, Value: strings.Join(process.Command, " "), Code: true})
	}
	if r.DocsPath != "" {
		f.Rows = append(f.Rows, pages.Fact{Label: "Docs", Value: r.DocsPath, Code: true})
	}
	if len(r.EnvNames) > 0 {
		f.Rows = append(f.Rows, pages.Fact{Label: "Environment", Value: strings.Join(r.EnvNames, ", "), Note: "values stay in .previewrc.yaml", Code: true})
	}
	if r.BuildTimeout != "" {
		f.Rows = append(f.Rows, pages.Fact{Label: "Build deadline", Value: minutes(r.BuildTimeout)})
	}
	return f
}

// buildBlocked says why a build cannot start now, or "".
func (v view) buildBlocked() string {
	if err := environments.BuildBlockReason(v.pr.BuildStatus, v.pr.NeedsRefresh); err != nil {
		switch {
		case errors.Is(err, environments.ErrPRConflicts):
			return "Resolve this pull request's merge conflicts on GitHub first."
		case errors.Is(err, environments.ErrPRChecking) && v.pr.MergeabilityUnresolved:
			return "GitHub has not said whether this pull request can merge. Sync pull requests on the Pull requests tab, or push a commit."
		case errors.Is(err, environments.ErrPRChecking):
			return "GitHub is checking whether this pull request can merge; building is offered once it answers."
		default:
			return "Sync pull requests on the Pull requests tab before building."
		}
	}
	switch {
	case v.caps.Execution:
		return ""
	case v.production:
		return "Build capacity is temporarily unavailable. Try again in a few minutes."
	case v.caps.Paused:
		return "Builds are paused by the operator (BUILDS_PAUSED)."
	case v.caps.Discovery:
		return "The running worker cannot build previews: set PREVIEW_BASE_DOMAIN on it, start Docker and the router, then restart it."
	default:
		return "No worker that can build is running. Start one (make run-worker during development)."
	}
}

func (v view) delayedTitle() string {
	if v.production || v.caps.Discovery {
		return "Setup checks are delayed."
	}
	return "No worker is running to check it."
}

func (v view) delayedDetail() string {
	if v.production || v.caps.Discovery {
		return "This page updates by itself when the check finishes. If it does not, check again in a few minutes."
	}
	return "Start the worker (make run-worker during development); waiting checks then run by themselves."
}

func (v view) previewURL() string {
	if v.previewDomain == "" || v.pr.Hostname == "" {
		return "its address"
	}
	return "https://" + v.pr.Hostname + "." + v.previewDomain
}

// OpenURL is a pull request's Open preview action, which records the open.
func OpenURL(repoID int64, number int32) string {
	return "/repositories/" + strconv.FormatInt(repoID, 10) + "/pull-requests/" + strconv.Itoa(int(number)) + "/open"
}

// buildFailure is the stored category of a failed request, as the PR page reads it.
func buildFailure(execution, environment string, current bool) string {
	if execution != "" {
		return execution
	}
	if current && environment != "" {
		return environment
	}
	return "unknown"
}

// failureText explains why a check could not finish.
func failureText(code string) (title, detail string) {
	switch code {
	case FailRateLimited:
		return "GitHub asked Previa to slow down.", "Its limit resets within the hour; try again in a few minutes."
	case FailGitHubDown:
		return "GitHub could not be reached.", "Try again shortly."
	case FailAccess:
		return "Previa could not read this commit on GitHub.", "The GitHub App may have lost access to the repository. Check its access, then try again."
	case FailClosed:
		return "The pull request was closed.", "Only open pull requests are checked."
	case FailApp:
		return "This worker has no GitHub App credentials.", "Configure the GitHub App on the worker, then try again."
	default:
		return "The check stopped unexpectedly.", "Try again; if it keeps failing, the worker's log has the cause."
	}
}

// problems lists what blocks a build, with the fix.
func problems(r Result, head string) ([]pages.Item, []pages.Link) {
	kits := pages.Link{Label: "Open Starter kits", Href: "/starter-kits", Icon: "kit"}
	switch r.Config {
	case ConfigMissing:
		item := pages.Item{Tone: "danger", Title: "No .previewrc.yaml at " + head + ".", Detail: "Previa reads the port, health check and services from this file at the repository root. A Starter kit writes one for your stack."}
		if r.RootDockerfile {
			item.Detail = "Previa reads the port, health check and services from this file at the repository root. Your Dockerfile is already there: the Own Dockerfile kit writes just this file."
			kits = pages.Link{Label: "Create it with a Starter kit", Href: "/starter-kits?stack=custom", Icon: "kit", Primary: true}
		} else {
			kits.Label, kits.Primary = "Create it with a Starter kit", true
		}
		return []pages.Item{item}, []pages.Link{kits}
	case ConfigInvalid:
		items := make([]pages.Item, 0, len(r.Issues))
		for _, issue := range r.Issues {
			field := issue.Field
			if field == "$" {
				field = ""
			}
			items = append(items, pages.Item{Tone: "danger", Title: issue.Message, Field: field, Line: issue.Line})
		}
		kits.Label = "Check it in the Starter kits editor"
		return items, []pages.Link{kits}
	}
	var items []pages.Item
	switch r.ContextState {
	case PathMissing:
		items = append(items, pages.Item{Tone: "danger", Title: "The build context " + quote(r.Context) + " does not exist at " + head + ".", Field: "context", Detail: "Point context at the folder that holds your app, relative to the repository root."})
	case PathNotDir:
		items = append(items, pages.Item{Tone: "danger", Title: "The build context " + quote(r.Context) + " is a file, not a folder.", Field: "context"})
	}
	switch r.DockerfileState {
	case PathMissing:
		items = append(items, pages.Item{Tone: "danger", Title: "No Dockerfile at " + quote(dockerfilePath(r)) + ".", Field: "dockerfile", Detail: "Add one there, or set dockerfile to its path inside the build context. A Starter kit writes one for your stack."})
		kits.Label, kits.Primary = "Get a Dockerfile from a Starter kit", true
	case PathNotFile:
		items = append(items, pages.Item{Tone: "danger", Title: quote(dockerfilePath(r)) + " is not a file.", Field: "dockerfile"})
	}
	if r.Submodules {
		items = append(items, pages.Item{Tone: "danger", Title: "Git submodules are not included in builds.", Detail: "GitHub's archive leaves their folders empty. Copy their files into the repository, or choose a build context without them."})
	}
	return items, []pages.Link{kits}
}

// passedNotes are what a passing check could not settle or adjusted.
func passedNotes(r Result) []pages.Item {
	var items []pages.Item
	if r.DockerfileState == PathUnverified || r.ContextState == PathUnverified {
		items = append(items, pages.Item{Tone: "info", Title: "A link in the build context or Dockerfile path is resolved by the build."})
	}
	for _, n := range r.Notices {
		items = append(items, pages.Item{Tone: "info", Title: n.Message, Field: n.Field})
	}
	return items
}

// minutes reads a stored duration ("12m0s") the way people say it.
func minutes(stored string) string {
	d, err := time.ParseDuration(stored)
	if err != nil || d <= 0 {
		return stored
	}
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return d.String()
}

func servicesSentence(services []ManagedService) string {
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, serviceName(s.Kind)+" "+s.Version)
	}
	return strings.Join(names, ", ")
}

func serviceName(kind string) string {
	switch kind {
	case "postgres":
		return "PostgreSQL"
	case "mysql":
		return "MySQL"
	case "redis":
		return "Redis"
	}
	return kind
}

func tierName(tier string) string {
	switch tier {
	case "small":
		return "Small"
	case "large":
		return "Large"
	}
	return "Standard"
}

func dockerfilePath(r Result) string {
	if r.Context == "" || r.Context == "." {
		return r.Dockerfile
	}
	return r.Context + "/" + r.Dockerfile
}

func quote(path string) string { return "“" + path + "”" }

// peopleSentence names who to ask.
func peopleSentence(people []pages.Person) string {
	if len(people) == 0 {
		return "Ask an Owner or Admin of this workspace."
	}
	names := make([]string, 0, len(people))
	for _, p := range people {
		names = append(names, p.Name+" (@"+p.Login+")")
	}
	return "Ask " + strings.Join(names, " or ") + "."
}

func stamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// ago is a rough age for a sentence; pages show exact times separately.
func ago(now time.Time, t pgtype.Timestamptz) string {
	if !t.Valid {
		return "earlier"
	}
	d := now.Sub(t.Time)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

// requestKey is one build form's idempotency key.
func requestKey() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
