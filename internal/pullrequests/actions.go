package pullrequests

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	workspacescope "github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// Builds records owner build requests and their durable wake-ups; the
// server's *environments.Broker (NewRequestBroker) implements it.
type Builds interface {
	RequestBuild(ctx context.Context, command environments.IntentCommand) (environments.IntentOutcome, error)
}

// Verifier reads a PR fresh from GitHub and records what it found;
// *SyncService implements it.
type Verifier interface {
	VerifyUpstream(ctx context.Context, pullRequestID int64) (environments.UpstreamSnapshot, error)
}

// Teardowns starts an owner's teardown; *environments.Repository implements it.
type Teardowns interface {
	RequestTeardown(ctx context.Context, pullRequestID int64, reason environments.TeardownReason) (bool, error)
}

// Capabilities says what the running workers can do; *workers.Reader
// implements it.
type Capabilities interface {
	Current(ctx context.Context) (workers.Capabilities, error)
}

// LogTail reads the last lines of a PR's current build output when it belongs
// to the given request generation and revision; *buildlogs.Store implements it.
type LogTail interface {
	Tail(ctx context.Context, environmentID, generation, revision int64, limit int) ([]string, error)
}

// failureLogLines is how much of a failed build's output the PR page shows.
const failureLogLines = 12

// Actions are the owner's build and teardown controls. Without them the
// controls are shown as unavailable and their endpoints refuse.
type Actions struct {
	Builds       Builds
	Verifier     Verifier
	Teardowns    Teardowns
	Capabilities Capabilities
	// PreviewBaseDomain renders preview addresses; empty shows none.
	PreviewBaseDomain string
}

// WithActions enables Build, Retry, Rebuild and teardown.
func (h *Handler) WithActions(a Actions) *Handler {
	h.previewDomain = a.PreviewBaseDomain
	if a.Builds != nil && a.Verifier != nil && a.Teardowns != nil && a.Capabilities != nil {
		h.actions = &a
	}
	return h
}

const buildsPaused = "Builds are paused by the operator. Requests already queued start when builds resume; try again then."

// WithLogTail lets the PR page quote the end of a failed build's output.
func (h *Handler) WithLogTail(t LogTail) *Handler {
	h.logs = t
	return h
}

// blocked says why builds cannot run now and what turns them back on.
type blocked struct{ reason, hint string }

// executionAvailable reports whether a worker can build now; when not, the
// reason and what fixes it.
func (h *Handler) executionAvailable(ctx context.Context) (bool, blocked, error) {
	if h.actions == nil {
		return false, blocked{"Build execution is not configured on this server.",
			"The server needs the GitHub App (GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH). Configure it, then restart the server."}, nil
	}
	caps, err := h.actions.Capabilities.Current(ctx)
	if err != nil {
		return false, blocked{}, err
	}
	switch {
	case caps.Execution:
		return true, blocked{}, nil
	case caps.Paused:
		return false, blocked{buildsPaused,
			"An operator resumes builds by restarting the worker without BUILDS_PAUSED. Teardown and cleanup keep working meanwhile."}, nil
	case caps.Discovery || caps.Reporting || caps.Housekeeping:
		// A worker with the GitHub App but without a preview domain tracks
		// pull requests and cannot build (with a domain and no Docker it
		// refuses to start).
		return false, blocked{"The running worker cannot build previews: it has no preview domain.",
			"Set PREVIEW_BASE_DOMAIN on the worker (preview.localtest.me works on one machine), start Docker and the router (make egress-image router-up), restart the worker, then reload this page."}, nil
	default:
		return false, blocked{"No worker that can build previews is running.",
			"Start the worker (make run-worker during development) with the GitHub App, Docker and a preview domain configured, then reload this page."}, nil
	}
}

// requestKey is one form's idempotency key: submitting the same form twice
// (a double click, a retried request) records one request.
func requestKey() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// detailPR maps an owned PR to its detail view: a headline, three lifecycle
// columns, why work waits or failed (with the end of its output), the
// preview's address and expiry, and the actions the owner may take now.
func (h *Handler) detailPR(ctx context.Context, row dbgen.OwnedPRRow) (pages.PR, error) {
	pr := pages.PR{Number: row.Number, Title: row.Title, Description: row.Description, Author: row.Author,
		Branch: row.HeadBranch, SHA: row.HeadSha, State: DisplayStatus(row.State, row.BuildStatus), Build: row.BuildState,
		URL: row.HtmlUrl, RequestedSHA: row.RequestedSha, DeployedSHA: row.DeployedSha,
		Environment: row.EnvironmentStatus, Available: row.Available.Bool,
		Deadline: timestamp(row.AvailableAt), Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh,
		PublishedAt: timestamp(row.PublishedAt), RequestCurrent: row.RequestCurrent,
		RequestSource: row.RequestSource, RequestedBy: row.RequestedByLogin,
		RequestedAt: timestamp(row.RequestedAt), StartedAt: timestamp(row.StartedAt), CompletedAt: timestamp(row.CompletedAt),
		OpenFindings: row.OpenFindings}
	switch {
	case previewDeployed(row.DeployedSha, row.EnvironmentStatus):
		h.describePreview(&pr, row)
	case row.EnvironmentStatus != environments.StatusTearingDown && row.EnvironmentStatus != environments.StatusTornDown && row.EnvironmentStatus != "publishing":
		// A build in progress or a failed first build has nothing deployed;
		// the request column carries its state.
		pr.Environment = "Not deployed"
	}
	if row.BuildState == "failed" && pr.Environment == "publishing" {
		pr.Environment = "Not deployed"
		if pr.DeployedSHA != "" {
			pr.Environment = environments.StatusRunning
		}
	}
	if row.RequestCurrent && row.WaitingReason != "" {
		pr.Waiting, pr.WaitingNote = environments.WaitingTitle(row.WaitingReason), environments.WaitingMessage(row.WaitingReason)
	}
	if c := h.headCommit(ctx, row); c.Message != "" {
		pr.CommitMessage, pr.CommitAuthor = c.Message, c.Author
		if !c.At.IsZero() {
			pr.CommitAt = c.At.UTC().Format(time.RFC3339)
		}
	}
	pr.ExecutionRetrying = row.ExecutionErrorCode == "worker_retrying"
	pr.PublicationRetrying = row.EnvironmentStatus == "publishing" && row.PublicationErrorCode != ""
	h.describeFailure(ctx, &pr, row)
	pr.RemovedNote = removedNote(row)
	if pr.State == "checking" && row.MergeabilityUnresolvedAt.Valid {
		pr.State, pr.MergeUnknown = "unknown", true
	}
	if h.actions != nil {
		caps, err := h.actions.Capabilities.Current(ctx)
		if err != nil {
			return pages.PR{}, err
		}
		pr.NoWorker = !caps.Discovery
		pr.BuildWorkerMissing = !caps.Execution && !caps.Paused
		pr.ServicesOffered, pr.ExecutionWorker = caps.Services, caps.Execution || caps.Paused
	}
	actions, err := h.actionsFor(ctx, row, pr.NoWorker)
	if err != nil {
		return pages.PR{}, err
	}
	pr.Actions = actions
	pr.Headline = headline(pr, row, h.now())
	return pr, nil
}

// previewDeployed reports whether a PR has a preview deployed, healthy or
// not, from its deployed commit and environment status.
func previewDeployed(deployedSHA, environmentStatus string) bool {
	return deployedSHA != "" && (environmentStatus == environments.StatusRunning ||
		environmentStatus == environments.StatusFailed || environmentStatus == "publishing")
}

// previewURL is a preview's address, or "" without a preview domain.
func (h *Handler) previewURL(hostname string) string {
	if h.previewDomain == "" || hostname == "" {
		return ""
	}
	return "https://" + hostname + "." + h.previewDomain
}

// describePreview adds the deployed preview: whether it serves an older
// commit, its address and expiry, and what runs next to its app.
func (h *Handler) describePreview(pr *pages.PR, row dbgen.OwnedPRRow) {
	pr.OlderPreview = row.DeployedSha != row.HeadSha
	pr.PreviewURL = h.previewURL(row.Hostname)
	pr.LastOpenedAt, pr.Requests, pr.AsleepAt = timestamp(row.LastVisitedAt), row.Visits, timestamp(row.AsleepAt)
	if row.ExpiresAt.Valid {
		pr.ExpiresAt = row.ExpiresAt.Time.UTC().Format(time.RFC3339)
	}
	backend := environments.ParseBackend(row.BackendServices, row.BackendProcesses, row.BackendSetup, row.BackendDocsPath, row.BackendOutbound, row.BackendOutboundWithheld)
	pr.Backend, pr.Outbound, pr.OutboundWithheld = backend.Summary(), backend.Outbound, backend.OutboundWithheld
	if row.BackendSeeded {
		pr.Backend += " · seed ran"
	}
	data := environments.ParseStartingData(row.BackendDataSource, row.BackendDataDetail, row.BackendSeeded)
	pr.DataSource, pr.DataReasons, pr.KeepsData = data.Title(), data.Reasons(), row.KeepsTestData
	pr.DataKept = data.Source == "kept"
	if pr.PreviewURL != "" && backend.DocsPath != "" {
		pr.DocsURL = pr.PreviewURL + backend.DocsPath
	}
}

// describeFailure explains why the current request failed, with the end of
// its output, or that the deployed preview stopped.
func (h *Handler) describeFailure(ctx context.Context, pr *pages.PR, row dbgen.OwnedPRRow) {
	switch {
	case row.RequestCurrent && row.BuildState == string(environments.IntentFailed):
		code := row.ExecutionErrorCode
		if code == "" && row.FailureCurrent {
			code = row.FailureCode
		}
		pr.Failure, pr.FailureCode = environments.FailureMessage(code), code
		if h.logs != nil && row.EnvironmentID > 0 && h.canReadLogTail(ctx, row.RepoID) {
			lines, err := h.logs.Tail(ctx, row.EnvironmentID, row.Generation, row.RequestRevision.Int64, failureLogLines)
			if err != nil {
				// The page stays useful without the excerpt; the log page has it all.
				h.logger.WarnContext(ctx, "read failed build output", "error", err)
			}
			pr.FailureLog = lines
		}
	case row.EnvironmentStatus == environments.StatusFailed && row.FailureCode == "preview_missing":
		pr.Failure, pr.FailureCode = environments.FailureMessage(row.FailureCode), row.FailureCode
	}
}

// headline sums the PR's preview up in one line, most pressing first.
func headline(pr pages.PR, row dbgen.OwnedPRRow, now time.Time) pages.Headline {
	current := row.RequestCurrent
	switch {
	case !row.Available.Bool:
		return pages.Headline{Tone: "tone-neutral", Title: "GitHub access to this repository is unavailable",
			Detail: "Previa keeps this pull request's record but cannot build or update it."}
	case row.State == "merged" || row.State == "closed":
		detail := pr.RemovedNote
		if detail == "" {
			detail = "Closed and merged pull requests have no preview."
		}
		title, tone := "Pull request closed", "tone-neutral"
		if row.State == "merged" {
			title, tone = "Pull request merged", "tone-merged"
		}
		return pages.Headline{Tone: tone, Title: title, Detail: detail}
	case pr.PublicationRetrying:
		return pages.Headline{Tone: "tone-warn", Title: "Publishing failed; retrying", Detail: environments.FailureMessage(row.PublicationErrorCode) + " Previa will retry automatically; any earlier preview stays available."}
	case pr.Waiting != "":
		return pages.Headline{Tone: "tone-info", Title: pr.Waiting, Detail: pr.WaitingNote}
	case current && (row.BuildState == "running" || row.BuildState == "queued") && row.ExecutionErrorCode == "worker_retrying":
		return pages.Headline{Tone: "tone-warn", Title: "Build attempt failed; retrying", Detail: "Previa will retry automatically. If attempts are exhausted, this request is marked failed. Check build logs for available output."}
	case current && (row.BuildState == "running" || row.BuildState == "queued") && pr.BuildWorkerMissing:
		return pages.Headline{Tone: "tone-warn", Title: "Build waiting for a worker", Detail: "No build worker is available. This request resumes when a worker reconnects; it is not publishing a new preview right now."}
	case current && row.BuildState == "running":
		return pages.Headline{Tone: "tone-info", Title: "Building " + pages.Short(row.RequestedSha),
			Detail: "The preview updates when the build succeeds. Follow it in the build log."}
	case current && row.BuildState == "queued":
		return pages.Headline{Tone: "tone-info", Title: "Queued to build " + pages.Short(row.RequestedSha),
			Detail: "It starts as soon as a worker has a free build slot."}
	case current && row.BuildState == "pending" && row.RequestSource == "automatic" && row.AvailableAt.Valid && row.AvailableAt.Time.After(now):
		// The quiet period: a countdown, and what resets or skips it.
		title := "Commit " + pages.Short(row.RequestedSha) + " builds in"
		detail := "Automatic builds wait until no new commit has arrived for five minutes, so a burst of pushes builds once; another push restarts the wait."
		if pr.DeployedSHA != "" && pr.DeployedSHA != row.RequestedSha {
			title = "New commit " + pages.Short(row.RequestedSha) + " builds in"
			detail += " The preview keeps serving " + pages.Short(pr.DeployedSHA) + " meanwhile."
		}
		if pr.Actions.BuildNow {
			detail += " Build now starts it at once."
		}
		return pages.Headline{Tone: "tone-info", Title: title, Detail: detail, Until: row.AvailableAt.Time.UTC().Format(time.RFC3339)}
	case current && row.BuildState == "pending":
		return pages.Headline{Tone: "tone-info", Title: "Starting the build of " + pages.Short(row.RequestedSha),
			Detail: "It starts as soon as a worker picks it up."}
	case current && row.BuildState == "failed":
		return pages.Headline{Tone: "tone-danger", Title: "Build failed", Detail: pr.Failure}
	case row.EnvironmentStatus == "failed" && row.FailureCode == "preview_missing":
		return pages.Headline{Tone: "tone-danger", Title: "Preview stopped", Detail: pr.Failure}
	case pr.DeployedSHA != "" && pr.Environment != "tearing_down" && pr.Environment != "torn_down":
		if pr.OlderPreview {
			return pages.Headline{Tone: "tone-warn", Title: "Preview live on an earlier commit",
				Detail: "It serves " + pages.Short(pr.DeployedSHA) + "; the latest commit " + pages.Short(pr.SHA) + " is not built yet."}
		}
		return pages.Headline{Tone: "tone-ok", Title: "Preview is live", Detail: "It serves the latest commit, " + pages.Short(pr.DeployedSHA) + "."}
	case pr.Environment == "tearing_down":
		return pages.Headline{Tone: "tone-warn", Title: "Removing the preview", Detail: "Its address stops serving and its containers are removed."}
	case row.BuildStatus == "conflicts":
		return pages.Headline{Tone: "tone-warn", Title: "Blocked by merge conflicts",
			Detail: "A preview can be built once the conflicts are resolved."}
	case row.BuildStatus == "checking" && pr.MergeUnknown:
		return pages.Headline{Tone: "tone-warn", Title: "GitHub did not report mergeability",
			Detail: "Previa asked several times and stopped waiting. Sync Pull Requests on the repository page asks again; a new commit does too."}
	case row.BuildStatus == "checking" && pr.NoWorker:
		return pages.Headline{Tone: "tone-warn", Title: "Waiting for a worker to check mergeability",
			Detail: "No worker is running, so GitHub is not asked yet. Start the worker (make run-worker); the check then runs by itself."}
	case row.BuildStatus == "checking":
		return pages.Headline{Tone: "tone-info", Title: "Checking mergeability",
			Detail: "GitHub is checking whether this pull request merges cleanly. Building is offered once it does."}
	case pr.RemovedNote != "":
		return pages.Headline{Tone: "tone-neutral", Title: "Preview removed", Detail: pr.RemovedNote}
	case pr.Actions.Unavailable != "":
		return pages.Headline{Tone: "tone-neutral", Title: "No preview yet",
			Detail: "Builds cannot start right now; the reason and the fix are below."}
	default:
		return pages.Headline{Tone: "tone-neutral", Title: "No preview yet",
			Detail: "Build the latest commit to get a preview address for this pull request."}
	}
}

func removedNote(row dbgen.OwnedPRRow) string {
	if row.TeardownRequested {
		switch row.TeardownReason.String {
		case string(environments.TeardownRequested):
			return "The owner tore this preview down. Build again to bring it back at the same address."
		case string(environments.TeardownExpired):
			return "This preview expired. A new build request, or a new commit with Automatic builds, brings it back at the same address."
		case string(environments.TeardownRevoked):
			return "GitHub access was removed, so the preview was torn down."
		}
	}
	if row.EnvironmentStatus == "torn_down" && row.State != "open" {
		return "The preview was removed when the pull request closed."
	}
	return ""
}

// actionsFor decides which controls the owner gets, from persisted state and
// whether a worker can build. The endpoints check everything again. A build
// action that would apply but cannot run now is named in Blocked, so the page
// shows it disabled with the reason and what fixes it.
func (h *Handler) actionsFor(ctx context.Context, row dbgen.OwnedPRRow, noWorker bool) (pages.Actions, error) {
	a := pages.Actions{ExpectedRevision: row.RequestRevision.Int64, RequestKey: requestKey()}
	unfinished := row.RequestCurrent && (row.BuildState == "pending" || row.BuildState == "queued" || row.BuildState == "running")
	denied := h.sourceDenied(ctx, row.RepoID)
	if row.State == environments.StateOpen && row.Available.Bool && !row.TeardownRequested && h.actions != nil && !denied {
		// Teardown needs no worker to be recorded; the removal runs when one does.
		deployed := previewDeployed(row.DeployedSha, row.EnvironmentStatus)
		a.Teardown = deployed || unfinished
		a.CancelOnly = !deployed
	}
	label := buildLabel(row)
	switch {
	case !row.Available.Bool:
		a.Unavailable, a.Hint, a.Blocked = "GitHub access to this repository is unavailable.",
			"Restore access on the Repositories page: choose repositories on GitHub and select this one again.", label
		return a, nil
	case row.State != "open":
		a.Unavailable = "Closed and merged pull requests are not built."
		return a, nil
	case denied:
		a.Unavailable = "GitHub no longer gives you access to this repository's code."
		a.Hint = "Building, tearing down and reading build logs need it. Once a repository administrator on GitHub restores your access, reload this page."
		return a, nil
	}
	if err := environments.BuildBlockReason(row.BuildStatus, row.NeedsRefresh); err != nil {
		a.Unavailable, a.Hint, a.Blocked = blockSentence(err), blockHint(err), label
		if errors.Is(err, environments.ErrPRChecking) {
			switch {
			case row.MergeabilityUnresolvedAt.Valid:
				a.Unavailable = "Build paused: GitHub did not report whether this pull request can merge."
				a.Hint = "Previa asked several times and stopped waiting. Choose Sync Pull Requests on the repository page to ask again, or push a new commit."
			case noWorker:
				a.Hint = "No worker is running to ask GitHub. Start the worker (make run-worker); the check then runs by itself."
			}
		}
		return a, nil
	}
	if h.admission != nil {
		allowed, err := h.admission.Allowed(ctx, workspacescope.WorkspaceID(ctx))
		if err != nil {
			return a, err
		}
		if !allowed {
			a.Unavailable, a.Hint, a.Blocked = "This workspace is waiting for early-access approval.",
				"An operator approves each new workspace before its first build; Early access shows its status. Approval starts no build by itself.", label
			return a, nil
		}
	}
	available, why, err := h.executionAvailable(ctx)
	if err != nil {
		return a, err
	}
	if !available {
		a.Unavailable, a.Hint, a.Blocked = why.reason, why.hint, label
		return a, nil
	}
	if !row.RequestCurrent {
		a.Build = true
		return a, nil
	}
	switch row.BuildState {
	case "pending":
		a.BuildNow = row.RequestSource == "automatic"
		if !a.BuildNow {
			a.InProgress = "Build queued"
		}
	case "queued":
		a.InProgress = "Build queued"
	case "running":
		a.InProgress = "Building preview"
	case "failed":
		a.Retry = true
	case "succeeded":
		a.Rebuild = true
	case "cancelled", "superseded":
		a.Build = true
	}
	return a, nil
}

// buildLabel names the build action that would apply to the PR's current
// state; empty while a manual request is already queued or running.
func buildLabel(row dbgen.OwnedPRRow) string {
	if !row.RequestCurrent {
		return "Build latest commit"
	}
	switch row.BuildState {
	case "pending":
		if row.RequestSource == "automatic" {
			return "Build now"
		}
		return ""
	case "queued", "running":
		return ""
	case "failed":
		return "Retry build"
	case "succeeded":
		return "Rebuild preview"
	default:
		return "Build latest commit"
	}
}

func blockSentence(err error) string {
	switch {
	case errors.Is(err, environments.ErrPRConflicts):
		return "Build refused: resolve merge conflicts and push a new commit first."
	case errors.Is(err, environments.ErrPRChecking):
		return "Build paused: GitHub is checking mergeability. Wait for the check or sync pull requests again."
	default:
		return "Conflicting upstream observations need a fresh GitHub snapshot. Refresh this repository before requesting work."
	}
}

// blockHint says what lifts a mergeability block.
func blockHint(err error) string {
	switch {
	case errors.Is(err, environments.ErrPRConflicts):
		return "Merge the base branch into this one (or rebase) on GitHub and push. Building is offered again once GitHub reports it mergeable."
	case errors.Is(err, environments.ErrPRChecking):
		return "GitHub usually answers within a minute. If it stays, choose Sync Pull Requests on the repository page."
	default:
		return "Choose Sync Pull Requests on the repository page, then try again."
	}
}

// ownedOpenPR resolves the PR in the URL for its owner.
func (h *Handler) ownedPR(w http.ResponseWriter, r *http.Request) (int64, int64, dbgen.OwnedPRRow, bool) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return 0, 0, dbgen.OwnedPRRow{}, false
	}
	n, err := publicid.PRNumber(r, id)
	if err != nil || n <= 0 {
		http.NotFound(w, r)
		return 0, 0, dbgen.OwnedPRRow{}, false
	}
	row, err := h.repo.Detail(r.Context(), user, id, int32(n))
	if err != nil {
		h.fail(w, r, err)
		return 0, 0, dbgen.OwnedPRRow{}, false
	}
	if !row.Available.Bool {
		h.fail(w, r, ErrUnavailable)
		return 0, 0, dbgen.OwnedPRRow{}, false
	}
	return user, id, row, true
}

// build records an owner's Build, Retry or Rebuild: ownership first, then the
// PR's state and mergeability, a worker that can build, a fresh read from
// GitHub (outside any lock), and finally the revision-checked, idempotent request with its
// wake-up in one transaction. Nothing builds on the server.
func (h *Handler) build(w http.ResponseWriter, r *http.Request) {
	user, id, row, ok := h.ownedPR(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	action := environments.IntentAction(r.PostForm.Get("action"))
	if action == "" {
		action = environments.BuildLatest
	}
	expected, err := strconv.ParseInt(r.PostForm.Get("expected_revision"), 10, 64)
	if err != nil || expected < 0 {
		expected = row.RequestRevision.Int64
	}
	key := strings.TrimSpace(r.PostForm.Get("request_key"))
	if key == "" {
		key = requestKey()
	}
	// What is wrong with the PR itself comes first; then whether a worker can
	// build at all.
	if row.State != "open" {
		if refusedFromSetup(w, r, id, "build-closed") {
			return
		}
		http.Error(w, "Closed and merged pull requests are not built.", http.StatusConflict)
		return
	}
	if err := environments.BuildBlockReason(row.BuildStatus, row.NeedsRefresh); err != nil {
		if refusedFromSetup(w, r, id, "build-blocked") {
			return
		}
		http.Error(w, blockSentence(err), http.StatusConflict)
		return
	}
	available, why, err := h.executionAvailable(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !available {
		if refusedFromSetup(w, r, id, "build-unavailable") {
			return
		}
		http.Error(w, why.reason+" No build was requested.", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.actions.Verifier.VerifyUpstream(r.Context(), row.ID); err != nil {
		h.refuse(w, r, id, err)
		return
	}
	// Reset test data starts from the chosen source: the repository baseline
	// or empty databases; never from the serving deployment's data.
	reset, source := action == environments.Rebuild && r.PostForm.Get("reset") == "true", ""
	if reset {
		source = cmp.Or(r.PostForm.Get("start_source"), environments.StartEmpty)
	}
	if _, err := h.actions.Builds.RequestBuild(r.Context(), environments.IntentCommand{
		UserID: user, PullRequestID: row.ID, ExpectedRevision: expected, IdempotencyKey: key, Action: action,
		Reset: reset, StartSource: source,
	}); err != nil {
		h.refuse(w, r, id, err)
		return
	}
	h.finishMutation(w, r, user, id)
}

// teardown removes the owner's preview of an open PR: confirmed in a dialog,
// CSRF-protected, owner-only. It records the request; the worker removes the
// route and then the containers.
func (h *Handler) teardown(w http.ResponseWriter, r *http.Request) {
	user, id, row, ok := h.ownedPR(w, r)
	if !ok {
		return
	}
	if h.actions == nil {
		http.Error(w, "Teardown is not configured on this server.", http.StatusServiceUnavailable)
		return
	}
	if row.State != "open" {
		http.Error(w, "Closed and merged pull requests are torn down automatically.", http.StatusConflict)
		return
	}
	if _, err := h.actions.Teardowns.RequestTeardown(r.Context(), row.ID, environments.TeardownRequested); err != nil {
		h.fail(w, r, err)
		return
	}
	// From the Previews page, return there (a fixed path, never a URL taken
	// from the form). "environments" is the page's former name.
	if ret := r.PostFormValue("return"); ret == "previews" || ret == "environments" {
		page, err := strconv.ParseInt(r.PostFormValue("page"), 10, 64)
		if err != nil || page < 1 {
			page = 1
		}
		publicid.Redirect(w, r, "/previews?page="+strconv.FormatInt(page, 10), http.StatusSeeOther)
		return
	}
	h.finishMutation(w, r, user, id)
}

// refuse answers a refused build with a short, safe reason. From the Setup
// tab it returns to the checklist with the matching notice.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, repoID int64, err error) {
	var limited *environments.UpstreamRetryAfter
	if code := setupRefusal(err); code != "" && refusedFromSetup(w, r, repoID, code) {
		return
	}
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(max(limited.Delay, time.Second)/time.Second)))
		http.Error(w, "GitHub asked Previa to slow down. Try again shortly. No build was requested.", http.StatusServiceUnavailable)
	case errors.Is(err, environments.ErrUpstreamUnavailable):
		http.Error(w, "GitHub could not confirm the pull request right now. Try again shortly. No build was requested.", http.StatusServiceUnavailable)
	case errors.Is(err, environments.ErrUpstreamInaccessible), errors.Is(err, environments.ErrIntentUnavailable):
		http.Error(w, "GitHub access to this pull request is no longer available. No build was requested.", http.StatusConflict)
	case errors.Is(err, environments.ErrUpstreamClosed), errors.Is(err, environments.ErrPRClosed):
		http.Error(w, "The pull request is closed. No build was requested.", http.StatusConflict)
	case errors.Is(err, environments.ErrPRConflicts), errors.Is(err, environments.ErrPRChecking), errors.Is(err, environments.ErrPRRefresh):
		http.Error(w, blockSentence(err), http.StatusConflict)
	case errors.Is(err, environments.ErrUpstreamChanged), errors.Is(err, environments.ErrIntentConflict), errors.Is(err, environments.ErrIntentKeyConflict):
		http.Error(w, "The pull request or its build changed meanwhile. Reload and try again.", http.StatusConflict)
	case errors.Is(err, environments.ErrIntentAction):
		http.Error(w, "That action no longer applies to the current build. Reload to see what changed.", http.StatusConflict)
	default:
		h.fail(w, r, err)
	}
}

// setupRefusal names a refused build for the Setup tab's notice; "" for
// errors that are not a refusal.
func setupRefusal(err error) string {
	var limited *environments.UpstreamRetryAfter
	switch {
	case errors.As(err, &limited), errors.Is(err, environments.ErrUpstreamUnavailable):
		return "build-github"
	case errors.Is(err, environments.ErrUpstreamInaccessible), errors.Is(err, environments.ErrIntentUnavailable):
		return "build-access"
	case errors.Is(err, environments.ErrUpstreamClosed), errors.Is(err, environments.ErrPRClosed):
		return "build-closed"
	case errors.Is(err, environments.ErrPRConflicts), errors.Is(err, environments.ErrPRChecking), errors.Is(err, environments.ErrPRRefresh):
		return "build-blocked"
	case errors.Is(err, environments.ErrUpstreamChanged), errors.Is(err, environments.ErrIntentConflict), errors.Is(err, environments.ErrIntentKeyConflict), errors.Is(err, environments.ErrIntentAction):
		return "build-changed"
	}
	return ""
}
