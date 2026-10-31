package workspaces

import (
	"fmt"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestPublicUUIDRoutesAndInvitationRecovery(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	catalog := publicid.New(f.pool)
	public := func(kind string, id, parent int64) string {
		t.Helper()
		v, e := catalog.Public(t.Context(), kind, id, parent)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	wid, rid, prid, uid := public("workspace", f.wid, 0), public("repository", f.repo, 0), public("pull-request", 1, f.repo), public("user", f.dev, 0)
	b := testBrowser{router: configuredFixtureRouter(t, f, "http://localhost", true), cookies: map[string]*http.Cookie{}}
	b.login(t, "owner")
	csrf := b.csrf(t)
	base := "/workspaces/" + wid
	page := b.request("GET", base+"/members", nil)
	if page.Code != 200 {
		t.Fatalf("UUID page: %d %s", page.Code, page.Body)
	}
	html := page.Body.String()
	if !strings.Contains(html, base+"/members/"+uid) || !strings.Contains(html, `value="`+rid+`"`) {
		t.Fatal("form resource identifiers were not UUIDs")
	}
	if regexp.MustCompile(`(?:href|action)="/(?:workspaces|repositories)/[0-9]+(?:/|\?|"|&)`).MatchString(html) {
		t.Fatal("numeric resource link leaked")
	}
	legacy := b.request("GET", fmt.Sprintf("/workspaces/%d/members", f.wid), nil)
	if legacy.Code != 303 || legacy.Header().Get("Location") != base+"/members" {
		t.Fatalf("old bookmark: %d %s", legacy.Code, legacy.Header())
	}
	if got := b.request("POST", fmt.Sprintf("/workspaces/%d/select", f.wid), url.Values{"csrf_token": {csrf}}); got.Code != 404 {
		t.Fatalf("numeric mutation accepted: %d", got.Code)
	}
	if got := b.request("GET", fmt.Sprintf("/repositories/%d/events", f.repo), nil); got.Code != 404 {
		t.Fatalf("numeric stream accepted: %d", got.Code)
	}
	detail := b.request("GET", "/repositories/"+rid+"/pull-requests/"+prid, nil)
	if detail.Code != 200 {
		t.Fatalf("UUID pull request: %d %s", detail.Code, detail.Body)
	}
	if _, err := catalog.Resolve(t.Context(), "pull-request", prid, f.otherRepo); err == nil {
		t.Fatal("PR UUID resolved under another repository")
	}
	for _, tc := range []struct {
		login, message string
		status         int
	}{
		{"@wrong", "valid GitHub username", 422}, {"missing-user", "no personal GitHub account", 422}, {"dev", "already a workspace member", 422}, {"provider-down", "could not check this account", 503},
	} {
		w, _ := f.service.Read(t.Context(), f.owner, f.wid)
		values := url.Values{"csrf_token": {csrf}, "workspace_revision": {fmt.Sprint(w.Revision)}, "login": {tc.login}, "role": {"reviewer"}, "repository": {rid}}
		result := b.request("POST", base+"/invitations", values)
		if result.Code != tc.status || !strings.Contains(result.Body.String(), tc.message) || strings.Contains(result.Body.String(), "Action needs attention") {
			t.Fatalf("%s: %d %s", tc.login, result.Code, result.Body)
		}
		if !strings.Contains(result.Body.String(), `value="`+tc.login+`"`) || !strings.Contains(result.Body.String(), `value="`+rid+`" checked`) {
			t.Fatalf("lost invitation selections for %s", tc.login)
		}
	}
	w, _ := f.service.Read(t.Context(), f.owner, f.wid)
	values := url.Values{"csrf_token": {csrf}, "workspace_revision": {fmt.Sprint(w.Revision)}, "login": {"pending-user"}, "role": {"reviewer"}, "repository": {rid}}
	if result := b.request("POST", base+"/invitations", values); result.Code != 200 {
		t.Fatalf("create invitation: %d %s", result.Code, result.Body)
	}
	w, _ = f.service.Read(t.Context(), f.owner, f.wid)
	values.Set("workspace_revision", fmt.Sprint(w.Revision))
	duplicate := b.request("POST", base+"/invitations", values)
	if duplicate.Code != 422 || !strings.Contains(duplicate.Body.String(), "already has a pending invitation") {
		t.Fatalf("pending invitation: %d %s", duplicate.Code, duplicate.Body)
	}
	outsider := testBrowser{router: b.router, cookies: map[string]*http.Cookie{}}
	outsider.login(t, "outsider")
	if result := outsider.request("GET", base+"/members", nil); result.Code != 404 {
		t.Fatalf("UUID bypassed membership: %d", result.Code)
	}
}
func TestPersonalNamesPersistAndRemainSelfScoped(t *testing.T) {
	f := newFixture(t)
	users := auth.NewRepository(f.pool)
	original, err := users.UpsertUser(t.Context(), auth.Identity{ID: 101, Username: "owner", Name: "GitHub Name", Email: "private@example.com", ProfileURL: "https://github.com/owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err = users.UpdatePersonalName(t.Context(), original.ID, original.ProfileRevision, "Ada", "Lovelace"); err != nil {
		t.Fatal(err)
	}
	updated, err := users.UpsertUser(t.Context(), auth.Identity{ID: 101, Username: "owner", Name: "Different GitHub Name", EmailSyncError: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName() != "Ada Lovelace" || updated.Email != "private@example.com" || updated.PublicID != original.PublicID {
		t.Fatalf("login overwrote own profile: %+v", updated)
	}
	if err = users.UpdatePersonalName(t.Context(), updated.ID, original.ProfileRevision, "Stale", "Name"); err != auth.ErrProfileConflict {
		t.Fatalf("stale name: %v", err)
	}
	b := testBrowser{router: configuredFixtureRouter(t, f, "http://localhost", true), cookies: map[string]*http.Cookie{}}
	b.login(t, "owner")
	csrf := b.csrf(t)
	profile := b.request("GET", "/protected", nil)
	if profile.Code != 200 || !strings.Contains(profile.Body.String(), "Ada Lovelace") || !strings.Contains(profile.Body.String(), "owner@example.com") {
		t.Fatalf("personal profile: %d %s", profile.Code, profile.Body)
	}
	values := url.Values{"first_name": {"Grace"}, "last_name": {"Hopper"}, "profile_revision": {fmt.Sprint(updated.ProfileRevision)}, "csrf_token": {csrf}, "user_id": {fmt.Sprint(f.dev)}}
	if result := b.request("POST", "/protected/profile", values); result.Code != 303 {
		t.Fatalf("save personal name: %d %s", result.Code, result.Body)
	}
	self, _ := users.GetUser(t.Context(), f.owner)
	other, _ := users.GetUser(t.Context(), f.dev)
	if self.DisplayName() != "Grace Hopper" || other.DisplayName() != "dev" {
		t.Fatal("profile mutation targeted another user")
	}
	values.Set("first_name", strings.Repeat("x", 81))
	values.Set("profile_revision", fmt.Sprint(self.ProfileRevision))
	if result := b.request("POST", "/protected/profile", values); result.Code != 400 || !strings.Contains(result.Body.String(), "80 characters") {
		t.Fatalf("invalid name: %d %s", result.Code, result.Body)
	}
	values.Del("csrf_token")
	if result := b.request("POST", "/protected/profile", values); result.Code != 403 {
		t.Fatalf("profile mutation omitted CSRF: %d", result.Code)
	}
	if err = users.UpdatePersonalName(t.Context(), self.ID, self.ProfileRevision, "", ""); err != nil {
		t.Fatal(err)
	}
	self, _ = users.GetUser(t.Context(), f.owner)
	if self.DisplayName() != "owner" {
		t.Fatal("empty name did not fall back to username")
	}
}
