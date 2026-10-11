package pullrequests

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
)

// FailureAttentionWindow is how long a failed build request needs attention
// after it finished: the Overview counts and highlights it, and Builds lists
// it under Needs attention. Older failures stay listed, quietly. A new
// failure of the same pull request is a new request and starts it again.
const FailureAttentionWindow = 7 * 24 * time.Hour

// The Builds page's failure filters.
const (
	FailuresAttention = "attention" // recent, not dismissed
	FailuresOlder     = "older"     // finished before the window, not dismissed
	FailuresDismissed = "dismissed" // dismissed by the owner
)

// ErrFailureChanged means the failure to dismiss is no longer the pull
// request's current, failed request: a newer request replaced it.
var ErrFailureChanged = errors.New("the failed build request was replaced")

// AttentionSince is the earliest finish time of a failure that still needs
// attention at now.
func AttentionSince(now time.Time) time.Time { return now.Add(-FailureAttentionWindow) }

func validFailures(v string) bool {
	switch v {
	case "", FailuresAttention, FailuresOlder, FailuresDismissed:
		return true
	}
	return false
}

// failureAttention classifies a Builds row with the same conditions as the
// DeliverySummary and Activity queries: only an open pull request's failed
// request for its latest commit can need attention.
func failureAttention(row dbgen.ActivityRow, since time.Time) string {
	switch {
	case row.BuildState != "failed":
		return ""
	case row.FailureDismissed:
		return FailuresDismissed
	case row.PrState != "open" || !row.RequestCurrent:
		return ""
	case row.CompletedAt.Valid && row.CompletedAt.Time.Before(since):
		return FailuresOlder
	}
	return FailuresAttention
}

// attentionNote says, in the row, why a failure is not highlighted.
func attentionNote(kind string) string {
	switch kind {
	case FailuresOlder:
		return "Over " + strconv.Itoa(int(FailureAttentionWindow/(24*time.Hour))) + " days old"
	case FailuresDismissed:
		return "Dismissed"
	}
	return ""
}

// ownedFailurePR resolves the owner's pull request for a dismissal. Unlike
// building, it does not need GitHub access: a dismissal is Previa's own note.
func (h *Handler) ownedFailurePR(w http.ResponseWriter, r *http.Request) (user int64, row dbgen.OwnedPRRow, ok bool) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return 0, row, false
	}
	n, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 32)
	if err != nil || n <= 0 {
		http.NotFound(w, r)
		return 0, row, false
	}
	if row, err = h.repo.Detail(r.Context(), user, id, int32(n)); err != nil {
		h.fail(w, r, err)
		return 0, row, false
	}
	return user, row, true
}

// dismissFailure hides the pull request's current failed request from the
// Overview's attention, for this owner, until it fails again.
func (h *Handler) dismissFailure(w http.ResponseWriter, r *http.Request) {
	user, row, ok := h.ownedFailurePR(w, r)
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil || revision <= 0 {
		http.Error(w, "Invalid build request.", http.StatusBadRequest)
		return
	}
	switch err := h.repo.DismissFailure(r.Context(), user, row.ID, revision); {
	case errors.Is(err, ErrFailureChanged):
		http.Error(w, "A newer build request replaced this failure. Reload the page.", http.StatusConflict)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	h.returnAfterFailureAction(w, r, row, true)
}

// restoreFailure undoes a dismissal.
func (h *Handler) restoreFailure(w http.ResponseWriter, r *http.Request) {
	user, row, ok := h.ownedFailurePR(w, r)
	if !ok {
		return
	}
	if err := h.repo.RestoreFailure(r.Context(), user, row.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	h.returnAfterFailureAction(w, r, row, false)
}

// returnAfterFailureAction goes back to where the owner acted: the Overview,
// the Builds page they were on (built from validated values, never a URL from
// the form), or the pull request. Builds offers Undo after a dismissal, since
// a filtered list no longer shows the row.
func (h *Handler) returnAfterFailureAction(w http.ResponseWriter, r *http.Request, row dbgen.OwnedPRRow, dismissed bool) {
	switch r.PostFormValue("return") {
	case "overview":
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	case "builds":
		v := url.Values{}
		if page, err := strconv.ParseInt(r.PostFormValue("page"), 10, 64); err == nil && page > 1 {
			v.Set("page", strconv.FormatInt(page, 10))
		}
		if f := r.PostFormValue("failures"); f != "" && validFailures(f) {
			v.Set("failures", f)
		}
		if repoID, err := strconv.ParseInt(r.PostFormValue("repo_id"), 10, 64); err == nil && repoID > 0 {
			v.Set("repo_id", strconv.FormatInt(repoID, 10))
		}
		if dismissed {
			v.Set("dismissed", strconv.FormatInt(row.RepoID, 10)+"-"+strconv.Itoa(int(row.Number)))
		}
		target := "/builds"
		if len(v) > 0 {
			target += "?" + v.Encode()
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/repositories/"+strconv.FormatInt(row.RepoID, 10)+"/pull-requests/"+strconv.Itoa(int(row.Number)), http.StatusSeeOther)
	}
}
