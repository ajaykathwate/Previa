package onboarding

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSetupPageAnswersOnlyPeopleWhoCanBuild(t *testing.T) {
	f := newFixture(t)
	router := f.router(t, "http://localhost")
	repo := f.public(t, "repository", f.repo, 0)
	setup := "/repositories/" + repo + "/setup"

	owner := newBrowser(router)
	owner.login(t, "owner")
	res := owner.do(http.MethodGet, setup, nil)
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "First preview checklist") || !strings.Contains(body, "Change 42") ||
		!strings.Contains(body, "Check setup") || !strings.Contains(body, `aria-current="page"`) || strings.Contains(body, "/repositories/"+strconv.FormatInt(f.repo, 10)+"/") {
		t.Fatalf("owner: %d %s", res.Code, body)
	}
	if res = owner.do(http.MethodGet, "/repositories/"+strconv.FormatInt(f.repo, 10)+"/setup", nil); res.Code != http.StatusSeeOther || res.Header().Get("Location") != setup {
		t.Fatalf("an old numeric bookmark redirects to the public address: %d %s", res.Code, res.Header().Get("Location"))
	}
	owner.headers.Set("HX-Request", "true")
	owner.headers.Set("HX-Target", "setup-detail")
	if res = owner.do(http.MethodGet, setup, nil); res.Code != http.StatusOK || strings.Contains(res.Body.String(), "<html") || !strings.Contains(res.Body.String(), "setup-card") {
		t.Fatalf("live refresh returns the checklist alone: %d", res.Code)
	}

	reviewer := newBrowser(router)
	reviewer.login(t, "reviewer")
	if res = reviewer.do(http.MethodGet, setup, nil); res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/previews" {
		t.Fatalf("Reviewers review previews instead: %d %s", res.Code, res.Header().Get("Location"))
	}
	outsider := newBrowser(router)
	outsider.login(t, "outsider")
	if res = outsider.do(http.MethodGet, setup, nil); res.Code != http.StatusNotFound {
		t.Fatalf("outsider: %d", res.Code)
	}
	dev := newBrowser(router)
	dev.login(t, "dev")
	if res = dev.do(http.MethodGet, "/repositories/"+f.public(t, "repository", f.otherRepo, 0)+"/setup", nil); res.Code != http.StatusNotFound {
		t.Fatalf("a repository the Developer was not given: %d", res.Code)
	}
	if res = dev.do(http.MethodGet, setup, nil); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Secrets are managed by Owners and Admins") {
		t.Fatalf("a Developer is told who manages secrets: %d", res.Code)
	}
}

func TestCheckRequestsAreProtectedAndReturnToTheChecklist(t *testing.T) {
	f := newFixture(t)
	router := f.router(t, "http://localhost")
	repo := f.public(t, "repository", f.repo, 0)
	pr := f.public(t, "pull-request", 42, f.repo)
	check := "/repositories/" + repo + "/setup/check?pr=" + pr

	dev := newBrowser(router)
	dev.login(t, "dev")
	if res := dev.do(http.MethodPost, check, url.Values{"expected_sha": {testSHA}}); res.Code != http.StatusForbidden || len(f.jobs.args) != 0 {
		t.Fatalf("a request without its CSRF token: %d", res.Code)
	}
	csrf := dev.csrf(t)
	res := dev.do(http.MethodPost, check, url.Values{"csrf_token": {csrf}, "expected_sha": {testSHA}})
	location := res.Header().Get("Location")
	if res.Code != http.StatusSeeOther || !strings.HasPrefix(location, "/repositories/"+repo+"/setup?pr="+pr) || !strings.Contains(location, "saved=check") || len(f.jobs.args) != 1 {
		t.Fatalf("requested: %d %s", res.Code, location)
	}
	page := dev.do(http.MethodGet, location, nil).Body.String()
	if !strings.Contains(page, "Setup check started") || !strings.Contains(page, "Checking ") {
		t.Fatal("the checklist says the check is on its way")
	}
	if res = dev.do(http.MethodPost, check, url.Values{"csrf_token": {csrf}, "expected_sha": {strings.Repeat("f", 40)}}); !strings.Contains(res.Header().Get("Location"), "notice=changed") {
		t.Fatalf("a page showing an older commit: %s", res.Header().Get("Location"))
	}
	numeric := "/repositories/" + repo + "/setup/check?pr=42"
	if res = dev.do(http.MethodPost, numeric, url.Values{"csrf_token": {csrf}, "expected_sha": {testSHA}}); res.Code != http.StatusNotFound {
		t.Fatalf("mutations accept only public addresses: %d", res.Code)
	}
	reviewer := newBrowser(router)
	reviewer.login(t, "reviewer")
	if res = reviewer.do(http.MethodPost, check, url.Values{"csrf_token": {reviewer.csrf(t)}, "expected_sha": {testSHA}}); res.Code != http.StatusNotFound || len(f.jobs.args) != 1 {
		t.Fatalf("a Reviewer cannot request checks: %d", res.Code)
	}
}

func TestOpenPreviewRecordsOpensFromPreviaOnly(t *testing.T) {
	f := newFixture(t)
	router := f.router(t, "http://localhost")
	repo := f.public(t, "repository", f.repo, 0)
	pr := f.public(t, "pull-request", 42, f.repo)
	open := "/repositories/" + repo + "/pull-requests/" + pr + "/open"
	opens := func() int {
		var n int
		if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM preview_opens").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	owner := newBrowser(router)
	owner.login(t, "owner")
	owner.headers.Set("Sec-Fetch-Site", "same-origin")
	if res := owner.do(http.MethodGet, open, nil); res.Code != http.StatusSeeOther || !strings.HasSuffix(res.Header().Get("Location"), "/repositories/"+repo+"/pull-requests/"+pr) {
		t.Fatalf("nothing deployed yet: %d %s", res.Code, res.Header().Get("Location"))
	}
	f.deploy(t, f.pr)
	res := owner.do(http.MethodGet, open, nil)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "https://pr-42-fixture.preview.test" || res.Header().Get("Referrer-Policy") != "no-referrer" || opens() != 1 {
		t.Fatalf("opened from Previa: %d %s %d", res.Code, res.Header().Get("Location"), opens())
	}
	admin := newBrowser(router)
	admin.login(t, "admin")
	admin.headers.Set("Sec-Fetch-Site", "cross-site")
	if res = admin.do(http.MethodGet, open, nil); res.Code != http.StatusSeeOther || opens() != 1 {
		t.Fatalf("a link from another site opens the preview but is not recorded: %d %d", res.Code, opens())
	}
	admin.headers.Set("Sec-Fetch-Site", "same-origin")
	admin.headers.Set("Sec-Purpose", "prefetch")
	if admin.do(http.MethodGet, open, nil); opens() != 1 {
		t.Fatal("a prefetch is not an open")
	}
	if res = owner.do(http.MethodGet, "/repositories/"+strconv.FormatInt(f.repo, 10)+"/pull-requests/42/open", nil); res.Code != http.StatusNotFound {
		t.Fatalf("numeric addresses are not accepted for an action: %d", res.Code)
	}
}

func TestARefusedBuildReturnsToTheChecklistWithItsReason(t *testing.T) {
	f := newFixture(t)
	router := f.router(t, "http://localhost")
	repo := f.public(t, "repository", f.repo, 0)
	pr := f.public(t, "pull-request", 42, f.repo)
	owner := newBrowser(router)
	owner.login(t, "owner")
	// The fixture's pull request page has no build workers, so building is refused.
	res := owner.do(http.MethodPost, "/repositories/"+repo+"/pull-requests/"+pr+"/build", url.Values{
		"csrf_token": {owner.csrf(t)}, "return": {"setup"}, "workspace_query": {"pr=" + pr}, "action": {"build"},
	})
	location := res.Header().Get("Location")
	target, err := url.Parse(location)
	if err != nil || res.Code != http.StatusSeeOther || target.Path != "/repositories/"+repo+"/setup" || target.Query().Get("pr") != pr || target.Query().Get("notice") != "build-unavailable" {
		t.Fatalf("refused build: %d %s %s", res.Code, location, res.Body.String())
	}
	if page := owner.do(http.MethodGet, location, nil).Body.String(); !strings.Contains(page, "Builds cannot start right now") {
		t.Fatal("the checklist explains the refusal")
	}
}
