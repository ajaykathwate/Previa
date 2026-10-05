package reporting

import (
	"fmt"
	"github.com/ajaykathwatee/previa/internal/environments"
	"time"
)

// Phase is what a PR's preview is doing, as reported on GitHub.
type Phase string

const (
	PhaseNone     Phase = "none"     // nothing requested for the latest commit, no live preview
	PhaseWaiting  Phase = "waiting"  // Automatic quiet period
	PhaseQueued   Phase = "queued"   // eligible, waiting for a worker
	PhaseBlocked  Phase = "blocked"  // GitHub is checking mergeability, or conflicts
	PhaseCapacity Phase = "capacity" // waiting for a build slot, preview capacity or disk space
	PhaseBuilding Phase = "building" // building, starting or publishing
	PhaseReady    Phase = "ready"    // the latest request's preview is live
	PhaseFailed   Phase = "failed"   // the latest request failed
	PhaseRemoving Phase = "removing" // teardown started
	PhaseRemoved  Phase = "removed"  // closed, torn down by its owner, or revoked
	PhaseExpired  Phase = "expired"  // torn down by expiry
)

// Snapshot is the persisted truth a report is rendered from.
type Snapshot struct {
	PullRequestID int64
	RepoID        int64 // Previa repository ID (dashboard URLs)
	Number        int32
	HeadSHA       string
	State         string // open, closed, merged
	Generation    int64
	BuildStatus   string // open, checking, conflicts
	NeedsRefresh  bool
	Fork          bool
	SecretCount   int64
	// A local teardown requested at the current generation, and why.
	TeardownRequested bool
	TeardownReason    string

	Intent *Intent // the PR's build request, if any

	Environment *Environment // the lifetime environment, if provisioned
}

// Intent is the PR's single build request.
type Intent struct {
	Generation  int64
	Revision    int64
	HeadSHA     string
	State       string // pending, queued, running, succeeded, failed, cancelled, superseded
	AvailableAt time.Time
	Waiting     string // "capacity", "disk" or "paused" while this request waits; empty otherwise
}

// Environment is the PR's lifetime environment.
type Environment struct {
	Status            string // queued, publishing, running, failed, tearing_down, torn_down
	Hostname          string // DNS label
	DeployedSHA       string // commit the active preview runs; empty when none
	AppliedGeneration int64
	AppliedRevision   int64
	LastErrorCode     string
	ExpiresAt         time.Time // zero: the preview does not expire
}

// View is what the living comment and the check run say.
type View struct {
	Phase      Phase
	HeadSHA    string
	Revision   int64     // the current request's revision, when there is one
	Deployed   string    // commit the live preview runs, when live
	PreviewURL string    // only for a live, published preview
	LogsURL    string    // only when a log page exists and the dashboard URL is known
	Failure    string    // safe sentence for a failed request
	Blocked    string    // why a requested build cannot start
	Waiting    string    // "capacity", "disk" or "paused" while the request waits
	Conflicts  bool      // blocked by merge conflicts (no build will run)
	EligibleAt time.Time // when an Automatic request becomes eligible
	ExpiresAt  time.Time // when the live preview expires; zero when it does not
	Notices    []string
	// Comment says whether the living comment should exist at all.
	Comment bool
	// Check is the check run for the current request; nil leaves checks alone.
	Check *Check
}

// Check is one check run, identified by the commit and request revision.
type Check struct {
	HeadSHA    string
	Revision   int64
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, neutral, cancelled (completed only)
	Title      string
	Summary    string
	DetailsURL string
}

// Settings say how reports link back to Previa.
type Settings struct {
	AppBaseURL        string // dashboard origin; empty: no log links
	PreviewBaseDomain string // empty: no preview URLs
	CheckName         string
}

// Derive turns persisted truth into what GitHub should show. It never invents
// a queue position or a URL: a preview URL appears only for a published
// preview, a log link only when the PR has a log page.
func Derive(s Snapshot, settings Settings, hasComment bool, now time.Time) View {
	v := View{Phase: PhaseNone, HeadSHA: s.HeadSHA}
	env := s.Environment
	live := env != nil && env.DeployedSHA != "" && (env.Status == "running" || env.Status == "failed" || env.Status == "publishing")
	if live {
		v.Deployed, v.ExpiresAt = env.DeployedSHA, env.ExpiresAt
		if settings.PreviewBaseDomain != "" && env.Hostname != "" {
			v.PreviewURL = "https://" + env.Hostname + "." + settings.PreviewBaseDomain
		}
	}
	if env != nil && settings.AppBaseURL != "" {
		v.LogsURL = fmt.Sprintf("%s/repositories/%d/pull-requests/%d/logs", settings.AppBaseURL, s.RepoID, s.Number)
	}
	current := s.Intent
	if current != nil && (current.Generation != s.Generation || current.HeadSHA != s.HeadSHA) {
		current = nil // a request for an older commit: nothing requested for the latest one
	}
	active := current != nil && current.State != "cancelled" && current.State != "superseded"
	v.Comment = hasComment || active || env != nil

	switch {
	case s.State != "open" || s.TeardownRequested:
		v.Phase = removalPhase(s, env)
		v.Deployed, v.PreviewURL, v.ExpiresAt = "", "", time.Time{}
		if v.Phase == PhaseRemoving && live {
			v.Deployed = env.DeployedSHA
		}
		return withNotices(v, s)
	case !active:
		if live {
			v.Phase = PhaseReady
		}
		return withNotices(v, s)
	}
	v.Revision = current.Revision
	switch current.State {
	case "pending", "queued", "running":
		if current.State == "running" && current.Waiting == "" {
			v.Phase = PhaseBuilding
			break
		}
		if reason := blockedReason(s); reason != "" && current.State != "running" {
			v.Phase, v.Blocked, v.Conflicts = PhaseBlocked, reason, !s.NeedsRefresh && s.BuildStatus == "conflicts"
		} else if current.Waiting != "" {
			v.Phase, v.Waiting = PhaseCapacity, current.Waiting
		} else if current.State == "pending" && current.AvailableAt.After(now) {
			v.Phase, v.EligibleAt = PhaseWaiting, current.AvailableAt
		} else {
			v.Phase = PhaseQueued
		}
	case "succeeded":
		switch {
		case env != nil && env.Status == "running" && env.AppliedGeneration == current.Generation && env.AppliedRevision == current.Revision:
			v.Phase = PhaseReady
		case env != nil && env.Status == "failed" && env.AppliedGeneration == current.Generation && env.AppliedRevision == current.Revision:
			v.Phase, v.Failure = PhaseFailed, environments.FailureMessage(env.LastErrorCode)
		default:
			v.Phase = PhaseBuilding // built; publication not confirmed yet
		}
	case "failed":
		v.Phase = PhaseFailed
		code := ""
		if env != nil && env.AppliedGeneration == current.Generation && env.AppliedRevision == current.Revision {
			code = env.LastErrorCode
		}
		v.Failure = environments.FailureMessage(code)
	}
	v.Check = checkFor(v, settings)
	return withNotices(v, s)
}

func removalPhase(s Snapshot, env *Environment) Phase {
	if env != nil && env.Status == "tearing_down" {
		return PhaseRemoving
	}
	if s.TeardownRequested && s.TeardownReason == "expired" {
		return PhaseExpired
	}
	return PhaseRemoved
}

func blockedReason(s Snapshot) string {
	switch {
	case s.NeedsRefresh:
		return "Previa is confirming this pull request's latest state with GitHub."
	case s.BuildStatus == "checking":
		return "GitHub is still checking whether this pull request can be merged."
	case s.BuildStatus == "conflicts":
		return "This pull request has merge conflicts. Resolve them to build a preview."
	}
	return ""
}

func withNotices(v View, s Snapshot) View {
	if s.Fork && s.SecretCount > 0 && v.Phase != PhaseRemoved && v.Phase != PhaseExpired {
		v.Notices = append(v.Notices, "Repository secrets are not given to previews of pull requests from forks.")
	}
	if v.Deployed != "" {
		switch v.Phase {
		case PhaseReady:
			if v.Deployed != v.HeadSHA {
				v.Notices = append(v.Notices, "The preview runs an earlier commit; newer commits have not been built.")
			}
		case PhaseWaiting, PhaseQueued, PhaseCapacity, PhaseBuilding, PhaseBlocked, PhaseFailed:
			// Also true of a rebuild of the same commit.
			v.Notices = append(v.Notices, "The previous preview keeps serving until a newer one is ready.")
		}
	}
	return v
}

// checkFor maps the current request to its check run.
func checkFor(v View, settings Settings) *Check {
	c := &Check{HeadSHA: v.HeadSHA, Revision: v.Revision, DetailsURL: v.LogsURL}
	switch v.Phase {
	case PhaseWaiting:
		c.Status, c.Title = "queued", "Waiting for the quiet period"
		c.Summary = "Automatic builds start five minutes after the latest push; this one is eligible at " + v.EligibleAt.UTC().Format("15:04 MST") + "."
	case PhaseQueued:
		c.Status, c.Title, c.Summary = "queued", "Queued", "The build will start when a worker is free."
	case PhaseCapacity:
		c.Status, c.Title, c.Summary = "queued", environments.WaitingTitle(v.Waiting), environments.WaitingMessage(v.Waiting)
	case PhaseBlocked:
		if v.Conflicts {
			c.Status, c.Conclusion, c.Title = "completed", "neutral", "Not built: merge conflicts"
		} else {
			c.Status, c.Title = "queued", "Waiting for GitHub"
		}
		c.Summary = v.Blocked
	case PhaseBuilding:
		c.Status, c.Title, c.Summary = "in_progress", "Building", "Previa is building and starting the preview for this commit."
	case PhaseReady:
		c.Status, c.Conclusion, c.Title, c.Summary = "completed", "success", "Preview ready", "The preview for this commit is live."
		if v.PreviewURL != "" {
			c.Summary += "\n\n" + v.PreviewURL
		}
	case PhaseFailed:
		c.Status, c.Conclusion, c.Title, c.Summary = "completed", "failure", "Preview failed", v.Failure
	default:
		return nil
	}
	if settings.CheckName == "" {
		return nil
	}
	return c
}
