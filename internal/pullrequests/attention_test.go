package pullrequests_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
)

// A failed build request needs attention for FailureAttentionWindow, then
// counts as older; a dismissal hides it from both until the pull request
// fails again, and Undo brings it back. Builds lists every failure and
// filters them the same way the Overview counts them.
func TestFailuresNeedAttentionUntilOldOrDismissed(t *testing.T) {
	h := newActionsHarness(t)
	repo := pr.NewRepository(h.pool)
	counts := func() (int64, int64) {
		t.Helper()
		row, err := repo.Delivery(t.Context(), h.userID, pr.AttentionSince(time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		return row.Failed, row.FailedOlder
	}
	builds := func(failures string) string {
		t.Helper()
		w := h.request(t, "GET", "/builds?failures="+failures, "")
		if w.Code != http.StatusOK {
			t.Fatalf("builds %q: %d", failures, w.Code)
		}
		return w.Body.String()
	}
	listed := func(body string) bool { return strings.Contains(body, "/repositories/1/pull-requests/7") }

	h.exec(t, `INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, requested_by, idempotency_key, state, available_at, started_at, completed_at)
		SELECT id, 1, 0, generation, head_sha, 'manual', 'build', $2, 'key-failed-00000001', 'failed', now(), now(), now() FROM pull_requests WHERE id=$1`, h.prID, h.userID)
	if failed, older := counts(); failed != 1 || older != 0 {
		t.Fatalf("a fresh failure needs attention, got %d recent and %d older", failed, older)
	}
	if body := builds(pr.FailuresAttention); !listed(body) || !strings.Contains(body, `failure/dismiss`) || !strings.Contains(body, `badge tone-danger`) {
		t.Fatal("Needs attention lists the failure, in red, with Dismiss")
	}

	h.exec(t, "UPDATE build_intents SET completed_at = now() - interval '8 days' WHERE pull_request_id=$1", h.prID)
	if failed, older := counts(); failed != 0 || older != 1 {
		t.Fatalf("an eight-day-old failure is older, got %d recent and %d older", failed, older)
	}
	if listed(builds(pr.FailuresAttention)) {
		t.Fatal("an older failure no longer needs attention")
	}
	body := builds(pr.FailuresOlder)
	if !listed(body) || !strings.Contains(body, "Over 7 days old") || strings.Contains(body, `badge tone-danger`) {
		t.Fatal("Older failures lists it quietly, saying why")
	}

	// Dismiss, with the revision the page showed and its CSRF token.
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(token) != 2 {
		t.Fatal("the page has no CSRF token")
	}
	dismiss := func(revision string) *http.Response {
		t.Helper()
		form := url.Values{"csrf_token": {token[1]}, "return": {"builds"}, "failures": {pr.FailuresOlder}, "revision": {revision}}
		return h.request(t, "POST", "/repositories/1/pull-requests/7/failure/dismiss", form.Encode()).Result()
	}
	if res := dismiss("2"); res.StatusCode != http.StatusConflict {
		t.Fatalf("dismissing a revision that is not the current failure is refused, got %d", res.StatusCode)
	}
	res := dismiss("1")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/builds?dismissed=1-7&failures=older" {
		t.Fatalf("a dismissal returns to the same view, offering Undo: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if failed, older := counts(); failed != 0 || older != 0 {
		t.Fatalf("a dismissed failure is not counted, got %d recent and %d older", failed, older)
	}
	if body := builds(pr.FailuresDismissed); !listed(body) || !strings.Contains(body, "failure/restore") {
		t.Fatal("Dismissed lists it with Undo")
	}
	if body := h.request(t, "GET", "/builds?dismissed=1-7", "").Body.String(); !strings.Contains(body, "Failure dismissed.") {
		t.Fatal("Builds offers Undo right after a dismissal")
	}

	// The pull request fails again: a new request needs attention again.
	h.exec(t, "UPDATE build_intents SET revision=2, base_revision=1, completed_at=now() WHERE pull_request_id=$1", h.prID)
	if failed, _ := counts(); failed != 1 {
		t.Fatal("a new failure of a dismissed pull request needs attention again")
	}

	// Undo restores the dismissal of the earlier revision's failure.
	h.exec(t, "UPDATE build_intents SET revision=1, base_revision=0, completed_at = now() - interval '8 days' WHERE pull_request_id=$1", h.prID)
	form := url.Values{"csrf_token": {token[1]}, "return": {"overview"}}
	if res := h.request(t, "POST", "/repositories/1/pull-requests/7/failure/restore", form.Encode()).Result(); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/dashboard" {
		t.Fatalf("Undo from the Overview returns there: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if _, older := counts(); older != 1 {
		t.Fatal("Undo counts the failure again")
	}

	if w := h.request(t, "GET", "/builds?failures=everything", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown filter is refused, got %d", w.Code)
	}
}
