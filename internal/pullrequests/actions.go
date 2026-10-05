package pullrequests

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	"github.com/go-chi/chi/v5"
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
	if a.Builds != nil && a.Verifier != nil && a.Teardowns != nil && a.Capabilities != nil {
		h.actions = &a
	}
	return h
}

const noWorker = "No preview worker is running with builds configured (the GitHub App, Docker and a preview domain). Start one, then try again."

const buildsPaused = "Builds are paused by the operator. Requests already queued start when builds resume; try again then."

// executionAvailable reports whether a worker can build now, and why not.
func (h *Handler) executionAvailable(ctx context.Context) (bool, string, error) {
	if h.actions == nil {
		return false, "Build execution is not configured on this server.", nil
	}
	caps, err := h.actions.Capabilities.Current(ctx)
	if err != nil {
		return false, "", err
	}
	if !caps.Execution {
		if caps.Paused {
			return false, buildsPaused, nil
		}
		return false, noWorker, nil
	}
	return true, "", nil
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

// detailPR maps an owned PR to its detail view: three lifecycle columns, why
// work waits or failed, the preview's address and expiry, and the actions the
// owner may take now.
func (h *Handler) detailPR(ctx context.Context, row dbgen.OwnedPRRow) (pages.PR, error) {
	pr := pages.PR{Number: row.Number, Title: row.Title, Author: row.Author,
		Branch: row.HeadBranch, SHA: row.HeadSha, State: DisplayStatus(row.State, row.BuildStatus), Build: row.BuildState,
		URL: row.HtmlUrl, RequestedSHA: row.RequestedSha, DeployedSHA: row.DeployedSha,
		Environment: row.EnvironmentStatus, Available: row.Available.Bool,
		Deadline: timestamp(row.AvailableAt), Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh,
		PublishedAt: timestamp(row.PublishedAt)}
	live := row.DeployedSha != "" && (row.EnvironmentStatus == "running" || row.EnvironmentStatus == "failed" || row.EnvironmentStatus == "publishing")
	if live {
		pr.OlderPreview = row.DeployedSha != row.HeadSha
		if h.actions != nil && h.actions.PreviewBaseDomain != "" && row.Hostname != "" {
			pr.PreviewURL = "https://" + row.Hostname + "." + h.actions.PreviewBaseDomain
		}
		if row.ExpiresAt.Valid {
			pr.ExpiresAt = row.ExpiresAt.Time.UTC().Format(time.RFC3339)
		}
	}
	if row.RequestCurrent && row.WaitingReason != "" {
		pr.Waiting, pr.WaitingNote = environments.WaitingTitle(row.WaitingReason), environments.WaitingMessage(row.WaitingReason)
	}
	switch {
	case row.RequestCurrent && row.BuildState == "failed":
		code := ""
		if row.FailureCurrent {
			code = row.FailureCode
		}
		pr.Failure = environments.FailureMessage(code)
	case row.EnvironmentStatus == "failed" && row.FailureCode == "preview_missing":
		pr.Failure = environments.FailureMessage(row.FailureCode)
	}
	pr.RemovedNote = removedNote(row)
	actions, err := h.actionsFor(ctx, row)
	if err != nil {
		return pages.PR{}, err
	}
	pr.Actions = actions
	return pr, nil
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
// whether a worker can build. The endpoints check everything again.
func (h *Handler) actionsFor(ctx context.Context, row dbgen.OwnedPRRow) (pages.Actions, error) {
	a := pages.Actions{ExpectedRevision: row.RequestRevision.Int64, RequestKey: requestKey()}
	unfinished := row.RequestCurrent && (row.BuildState == "pending" || row.BuildState == "queued" || row.BuildState == "running")
	if row.State == "open" && row.Available.Bool && !row.TeardownRequested && h.actions != nil {
		// Teardown needs no worker to be recorded; the removal runs when one does.
		live := row.EnvironmentStatus != "Not deployed" && row.EnvironmentStatus != "tearing_down" && row.EnvironmentStatus != "torn_down"
		a.Teardown = live || unfinished
	}
	switch {
	case !row.Available.Bool:
		a.Unavailable = "GitHub access to this repository is unavailable."
		return a, nil
	case row.State != "open":
		a.Unavailable = "Closed and merged pull requests are not built."
		return a, nil
	}
	if err := environments.BuildBlockReason(row.BuildStatus, row.NeedsRefresh); err != nil {
		a.Unavailable = blockSentence(err)
		return a, nil
	}
	available, reason, err := h.executionAvailable(ctx)
	if err != nil {
		return a, err
	}
	if !available {
		a.Unavailable = reason
		return a, nil
	}
	if !row.RequestCurrent {
		a.Build = true
		return a, nil
	}
	switch row.BuildState {
	case "pending", "queued":
		a.BuildNow = row.RequestSource == "automatic" // a manual request is already queued
	case "failed":
		a.Retry = true
	case "succeeded":
		a.Rebuild = true
	case "cancelled", "superseded":
		a.Build = true
	}
	return a, nil
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

// ownedOpenPR resolves the PR in the URL for its owner.
func (h *Handler) ownedPR(w http.ResponseWriter, r *http.Request) (int64, int64, dbgen.OwnedPRRow, bool) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return 0, 0, dbgen.OwnedPRRow{}, false
	}
	n, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 32)
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
		http.Error(w, "Closed and merged pull requests are not built.", http.StatusConflict)
		return
	}
	if err := environments.BuildBlockReason(row.BuildStatus, row.NeedsRefresh); err != nil {
		http.Error(w, blockSentence(err), http.StatusConflict)
		return
	}
	available, reason, err := h.executionAvailable(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !available {
		http.Error(w, reason+" No build was requested.", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.actions.Verifier.VerifyUpstream(r.Context(), row.ID); err != nil {
		h.refuse(w, r, err)
		return
	}
	if _, err := h.actions.Builds.RequestBuild(r.Context(), environments.IntentCommand{
		UserID: user, PullRequestID: row.ID, ExpectedRevision: expected, IdempotencyKey: key, Action: action,
	}); err != nil {
		h.refuse(w, r, err)
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
	// From the Environments page, return there (a fixed path, never a URL
	// taken from the form).
	if r.PostFormValue("return") == "environments" {
		page, err := strconv.ParseInt(r.PostFormValue("page"), 10, 64)
		if err != nil || page < 1 {
			page = 1
		}
		http.Redirect(w, r, "/environments?page="+strconv.FormatInt(page, 10), http.StatusSeeOther)
		return
	}
	h.finishMutation(w, r, user, id)
}

// refuse answers a refused build with a short, safe reason.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, err error) {
	var limited *environments.UpstreamRetryAfter
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
