package reviews

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/realtime"
)

var (
	requestKeyField = regexp.MustCompile(`name="request_key" value="([^"]+)"`)
	formAction      = regexp.MustCompile(`<form method="post" action="([^"]+/findings)"`)
)

// form is a page's new-finding form: its address and request key.
func composerForm(t *testing.T, body string) (string, string) {
	t.Helper()
	action := formAction.FindStringSubmatch(body)
	key := requestKeyField.FindStringSubmatch(body)
	if len(action) != 2 || len(key) != 2 {
		t.Fatalf("no new-finding form in:\n%s", body)
	}
	return html.UnescapeString(action[1]), key[1]
}

func TestReviewersUseTheReviewPageAndNothingElse(t *testing.T) {
	f := newFixture(t)
	f.publish(t, f.pr, shaA)
	b := newBrowser(f.router(t, "http://localhost", nil))
	b.login(t, "reviewer")
	csrf := b.csrf(t)
	review := f.reviewPath(t)
	res := b.do(http.MethodGet, review, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("review page: %d %s", res.Code, res.Body.String())
	}
	page := res.Body.String()
	for _, want := range []string{"How to test", "Record a finding", "Recorded against <strong>Deployment 1</strong>", "Why: &lt;b&gt;faster&lt;/b&gt; checkout", "Copy review link", "http://localhost" + review} {
		if !strings.Contains(page, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	if strings.Contains(page, "pr-tabs") || strings.Contains(page, "Write instructions") {
		t.Error("a Reviewer sees neither the operator switch nor instruction editing")
	}
	pr := strings.TrimSuffix(review, "/review")
	if res := b.do(http.MethodGet, pr, nil); res.Code != http.StatusSeeOther || !strings.HasSuffix(res.Header().Get("Location"), "/review") {
		t.Fatalf("a shared pull request link leads to its review: %d %q", res.Code, res.Header().Get("Location"))
	}
	if res := b.do(http.MethodGet, pr+"/logs", nil); res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/previews" {
		t.Fatalf("logs stay closed to Reviewers: %d %q", res.Code, res.Header().Get("Location"))
	}
	if res := b.do(http.MethodGet, pr+"/open", nil); res.Code != http.StatusNoContent {
		t.Fatalf("Open preview is open to Reviewers: %d %q", res.Code, res.Header().Get("Location"))
	}
	repo := "/repositories/" + f.public(t, "repository", f.repo, 0)
	for _, path := range []string{repo + "/policy", repo + "/secrets", pr + "/build", pr + "/teardown"} {
		if res := b.do(http.MethodPost, path, url.Values{"csrf_token": {csrf}}); res.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.Code)
		}
	}
	if res := b.do(http.MethodPost, review+"/instructions", url.Values{"csrf_token": {csrf}, "summary": {"Mine now"}}); res.Code != http.StatusNotFound {
		t.Fatalf("a forged instructions save: %d", res.Code)
	}
	other := "/repositories/" + f.public(t, "repository", f.otherRepo, 0) + "/pull-requests/" + f.public(t, "pull-request", 7, f.otherRepo) + "/review"
	if res := b.do(http.MethodGet, other, nil); res.Code != http.StatusNotFound {
		t.Fatalf("a repository without a grant: %d", res.Code)
	}
}

func TestTheReviewStreamIsOpenToReviewers(t *testing.T) {
	f := newFixture(t)
	b := newBrowser(f.router(t, "http://localhost", realtime.NewHub()))
	b.login(t, "reviewer")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/repositories/"+f.public(t, "repository", f.repo, 0)+"/events", nil).WithContext(ctx)
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	res := httptest.NewRecorder()
	b.router.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.HasPrefix(res.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %q", res.Code, res.Header().Get("Content-Type"))
	}
	page := b.do(http.MethodGet, f.reviewPath(t), nil).Body.String()
	if !strings.Contains(page, "data-live-events=") || !strings.Contains(page, "data-live-refresh") {
		t.Fatal("the review page follows its repository's changes")
	}
}

func TestFindingFormsWorkWithoutJavaScript(t *testing.T) {
	f := newFixture(t)
	f.publish(t, f.pr, shaA)
	b := newBrowser(f.router(t, "http://localhost", nil))
	b.login(t, "reviewer")
	csrf := b.csrf(t)
	review := f.reviewPath(t)
	action, key := composerForm(t, b.do(http.MethodGet, review, nil).Body.String())
	finding := url.Values{"csrf_token": {csrf}, "body": {"<script>alert(1)</script> crashes"}, "path": {"/checkout"}, "details": {"POST /api/pay"}, "request_key": {key}, "deployment": {"1"}}
	res := b.do(http.MethodPost, action, finding)
	if res.Code != http.StatusSeeOther || !strings.HasSuffix(res.Header().Get("Location"), "?saved=finding#finding-1") {
		t.Fatalf("create: %d %q %s", res.Code, res.Header().Get("Location"), res.Body.String())
	}
	if res := b.do(http.MethodPost, action, finding); res.Code != http.StatusSeeOther || !strings.HasSuffix(res.Header().Get("Location"), "#finding-1") {
		t.Fatalf("a double submission lands on the same finding: %d %q", res.Code, res.Header().Get("Location"))
	}
	page := b.do(http.MethodGet, review+"?saved=finding", nil).Body.String()
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt; crashes") || strings.Contains(page, "<script>alert(1)") || !strings.Contains(page, "Finding added") {
		t.Fatal("finding text is escaped and the save confirmed")
	}

	// A form problem shows the page again with the draft.
	_, key = composerForm(t, page)
	res = b.do(http.MethodPost, action, url.Values{"csrf_token": {csrf}, "body": {"  "}, "path": {"/keep-me"}, "request_key": {key}, "deployment": {"1"}})
	if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), `value="/keep-me"`) || !strings.Contains(res.Body.String(), "Describe what you found.") {
		t.Fatalf("validation: %d", res.Code)
	}

	// A newer deployment while writing: choose, never relabelled.
	f.publish(t, f.pr, shaB)
	draft := url.Values{"csrf_token": {csrf}, "body": {"Slow on the old one"}, "request_key": {key}, "deployment": {"1"}}
	res = b.do(http.MethodPost, action, draft)
	body := res.Body.String()
	if res.Code != http.StatusConflict || !strings.Contains(body, "A newer deployment was published while you wrote this.") ||
		!strings.Contains(body, "Slow on the old one") || !strings.Contains(body, "Record against Deployment 2") || !strings.Contains(body, "Keep Deployment 1") {
		t.Fatalf("version conflict: %d\n%s", res.Code, body)
	}
	draft.Del("deployment")
	draft.Set("older_deployment", "1")
	if res := b.do(http.MethodPost, action, draft); res.Code != http.StatusSeeOther || !strings.HasSuffix(res.Header().Get("Location"), "#finding-2") {
		t.Fatalf("keeping deployment 1: %d %q", res.Code, res.Header().Get("Location"))
	}

	// Reply, edit and resolve, returning to the same list.
	listQuery := url.Values{"state": {"all"}}.Encode()
	res = b.do(http.MethodPost, review+"/findings/1/replies", url.Values{"csrf_token": {csrf}, "body": {"Seen on deployment 2 too"}, "request_key": {"reply-key-0000000001"}, "deployment": {"2"}, "return_query": {listQuery}})
	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "state=all") || !strings.HasSuffix(res.Header().Get("Location"), "#finding-1-2") {
		t.Fatalf("reply: %d %q", res.Code, res.Header().Get("Location"))
	}
	if res := b.do(http.MethodPost, review+"/findings/1/messages/1/edit", url.Values{"csrf_token": {csrf}, "body": {"Crashes on submit"}, "revision": {"1"}}); res.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", res.Code, res.Body.String())
	}
	res = b.do(http.MethodPost, review+"/findings/1/messages/1/edit", url.Values{"csrf_token": {csrf}, "body": {"A late, stale wording"}, "revision": {"1"}})
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "This message changed in another window.") || !strings.Contains(res.Body.String(), "A late, stale wording") {
		t.Fatalf("a stale edit keeps the draft: %d", res.Code)
	}
	if res := b.do(http.MethodPost, review+"/findings/1/resolve", url.Values{"csrf_token": {csrf}, "revision": {"1"}, "deployment": {"2"}}); res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "saved=resolved") {
		t.Fatalf("resolve: %d %q", res.Code, res.Header().Get("Location"))
	}
	if res := b.do(http.MethodPost, review+"/findings/1/reopen", url.Values{"csrf_token": {csrf}, "revision": {"1"}}); res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "notice=stale") {
		t.Fatalf("a stale reopen explains itself: %d %q", res.Code, res.Header().Get("Location"))
	}
	page = b.do(http.MethodGet, review+"?state=all", nil).Body.String()
	if !strings.Contains(page, "Resolved by you on Deployment 2") && !strings.Contains(page, "Resolved by Ravi Reviewer on Deployment 2") {
		t.Fatalf("resolution line missing:\n%s", page)
	}

	// Removal without JavaScript confirms in place.
	res = b.do(http.MethodGet, review+"?state=all&remove=2.1", nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "<noscript>") || !strings.Contains(res.Body.String(), "Remove your finding?") || !strings.Contains(res.Body.String(), "data-open-on-load") {
		t.Fatalf("no-JavaScript removal: %d", res.Code)
	}
	if res := b.do(http.MethodPost, review+"/findings/2/messages/1/remove", url.Values{"csrf_token": {csrf}}); res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "saved=removed-finding") {
		t.Fatalf("remove: %d %q", res.Code, res.Header().Get("Location"))
	}
	if res := b.do(http.MethodPost, review+"/findings", url.Values{"body": {"No token"}, "request_key": {key}}); res.Code != http.StatusForbidden {
		t.Fatalf("a missing CSRF token: %d", res.Code)
	}
	numeric := "/repositories/" + strconv.FormatInt(f.repo, 10) + "/pull-requests/42/review/findings"
	if res := b.do(http.MethodPost, numeric, url.Values{"csrf_token": {csrf}, "body": {"x"}, "request_key": {key}}); res.Code != http.StatusNotFound {
		t.Fatalf("a numeric address in a change: %d", res.Code)
	}
}

func TestModerationNeedsAReasonAndAsksAgain(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.CreateFinding(t.Context(), f.as(f.reviewer), FindingInput{Body: "Rude words", RequestKey: key(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Reply(t.Context(), f.as(f.reviewer), 1, ReplyInput{Body: "More rude words", RequestKey: key(2)}); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(f.router(t, "http://localhost", nil))
	b.login(t, "dev")
	csrf := b.csrf(t)
	review := f.reviewPath(t)
	page := b.do(http.MethodGet, review, nil).Body.String()
	if !strings.Contains(page, "Remove as moderator") || !strings.Contains(page, "Remove finding") || !strings.Contains(page, `class="repo-tabs pr-tabs"`) {
		t.Fatal("a Developer sees moderation and the Preview | Review switch")
	}
	res := b.do(http.MethodPost, review+"/findings/1/messages/2/remove", url.Values{"csrf_token": {csrf}})
	if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "Choose why you are removing it.") || !strings.Contains(res.Body.String(), "data-open-on-load") {
		t.Fatalf("moderation without a reason: %d", res.Code)
	}
	if res := b.do(http.MethodPost, review+"/findings/1/messages/2/remove", url.Values{"csrf_token": {csrf}, "reason": {"abuse"}}); res.Code != http.StatusSeeOther {
		t.Fatalf("moderation: %d", res.Code)
	}
	if res := b.do(http.MethodPost, review+"/findings/1/remove", url.Values{"csrf_token": {csrf}, "reason": {"spam"}}); res.Code != http.StatusSeeOther {
		t.Fatalf("remove the finding: %d", res.Code)
	}
	page = b.do(http.MethodGet, review+"?state=all", nil).Body.String()
	if !strings.Contains(page, "Removed by Maya Developer · Spam or off-topic") || strings.Contains(page, "Rude words") {
		t.Fatal("a moderated finding is a tombstone with its reason, and its text is gone")
	}
	pr := strings.TrimSuffix(review, "/review")
	if detail := b.do(http.MethodGet, pr, nil).Body.String(); !strings.Contains(detail, `class="repo-tabs pr-tabs"`) || !strings.Contains(detail, review) {
		t.Fatal("the pull request page links its review")
	}
}

func TestInstructionsSaveInPlaceAndKeepConflicts(t *testing.T) {
	f := newFixture(t)
	f.publish(t, f.pr, shaA)
	b := newBrowser(f.router(t, "http://localhost", nil))
	b.login(t, "dev")
	csrf := b.csrf(t)
	review := f.reviewPath(t)
	if page := b.do(http.MethodGet, review, nil).Body.String(); !strings.Contains(page, "Write instructions") || !strings.Contains(page, `data-edit-form="instructions"`) {
		t.Fatal("an empty review invites instructions")
	}
	save := url.Values{"csrf_token": {csrf}, "summary": {"Checkout validates the address"}, "steps": {"Open /checkout\nChoose Pay"}, "expected": {"An inline error"}, "start_path": {"/checkout"}, "revision": {"0"}}
	if res := b.do(http.MethodPost, review+"/instructions", save); res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "saved=instructions") {
		t.Fatalf("save: %d %q", res.Code, res.Header().Get("Location"))
	}
	if page := b.do(http.MethodGet, review, nil).Body.String(); !strings.Contains(page, "<li>Choose Pay</li>") || !strings.Contains(page, "?path=%2Fcheckout") {
		t.Fatal("saved steps and a start link through Open preview")
	}
	save.Set("summary", "Someone else's older edit")
	res := b.do(http.MethodPost, review+"/instructions", save)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "Someone saved these instructions while you were editing.") || !strings.Contains(res.Body.String(), "Save mine instead") {
		t.Fatalf("conflict: %d", res.Code)
	}
	save.Set("start_path", "https://elsewhere.example")
	save.Set("revision", "1")
	if res := b.do(http.MethodPost, review+"/instructions", save); res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "Use a path inside the preview") {
		t.Fatalf("an outside address: %d", res.Code)
	}
}
