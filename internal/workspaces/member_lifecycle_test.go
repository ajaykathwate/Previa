package workspaces

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
)

func membershipRevision(t *testing.T, f fixture, user int64) int64 {
	t.Helper()
	var rev int64
	if err := f.pool.QueryRow(t.Context(), "SELECT revision FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", f.wid, user).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	return rev
}

func publicUser(t *testing.T, f fixture, user int64) string {
	t.Helper()
	id, err := publicid.New(f.pool).Public(t.Context(), "user", user, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRemovedMembersAreDeactivatedAndReinvitedWithPreviousAccess(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	router := configuredFixtureRouter(t, f, "http://localhost", true)
	owner := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	owner.login(t, "owner")
	csrf := owner.csrf(t)
	base, dev := "/workspaces/"+publicWorkspace(t, f), publicUser(t, f, f.dev)
	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	removed := owner.request("POST", base+"/members/"+dev, url.Values{"csrf_token": {csrf}, "workspace_revision": {strconv.FormatInt(w.Revision, 10)},
		"revision": {strconv.FormatInt(membershipRevision(t, f, f.dev), 10)}, "action": {"remove"}})
	if removed.Code != http.StatusSeeOther {
		t.Fatalf("remove member: %d %s", removed.Code, removed.Body)
	}
	if _, err := f.service.Authorize(t.Context(), f.dev, f.repo, "read", false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("removed member kept access: %v", err)
	}
	if active := owner.request("GET", base+"/members", nil).Body.String(); strings.Contains(active, `href="`+base+"/members/"+dev+`"`) {
		t.Fatal("removed member still listed as active")
	}
	list := owner.request("GET", base+"/members?view=deactivated", nil).Body.String()
	if !strings.Contains(list, "Deactivated") || !strings.Contains(list, "removed by owner") || !strings.Contains(list, "reinvite="+dev) {
		t.Fatalf("deactivated list: %s", list)
	}
	profile := owner.request("GET", base+"/members/"+dev, nil)
	if profile.Code != 200 || !strings.Contains(profile.Body.String(), "All access to this workspace is revoked") || strings.Contains(profile.Body.String(), "Manage access</button>") {
		t.Fatalf("deactivated profile: %d", profile.Code)
	}
	reviewer := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	reviewer.login(t, "reviewer")
	if r := reviewer.request("GET", base+"/members/"+dev, nil); r.Code != http.StatusNotFound {
		t.Fatalf("non-manager saw a deactivated profile: %d", r.Code)
	}
	if r := reviewer.request("GET", base+"/members?view=deactivated", nil); r.Code != http.StatusNotFound {
		t.Fatalf("non-manager saw deactivated members: %d", r.Code)
	}
	prefill := owner.request("GET", base+"/members?view=deactivated&reinvite="+dev, nil).Body.String()
	repo, _ := publicid.New(f.pool).Public(t.Context(), "repository", f.repo, 0)
	if !strings.Contains(prefill, "Re-invite @dev") || !strings.Contains(prefill, `value="dev"`) ||
		!strings.Contains(prefill, `value="developer" checked`) || !strings.Contains(prefill, `value="`+repo+`" checked`) {
		t.Fatal("re-invitation was not prefilled with the previous role and repositories")
	}
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	if _, err := f.service.Authorize(t.Context(), f.dev, f.repo, "operate", false); err != nil {
		t.Fatalf("re-invited member did not regain access: %v", err)
	}
	if list := owner.request("GET", base+"/members?view=deactivated", nil).Body.String(); strings.Contains(list, "reinvite="+dev) {
		t.Fatal("rejoined member is still deactivated")
	}
}

func TestInvitedPeopleAnswerInsidePrevia(t *testing.T) {
	f := newFixture(t)
	router := configuredFixtureRouter(t, f, "http://localhost", true)
	w, _ := f.service.Read(t.Context(), f.owner, f.wid)
	if _, err := f.service.Invite(t.Context(), InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: "dev", Role: "developer", Repositories: []int64{f.repo}}); err != nil {
		t.Fatal(err)
	}
	var invitation int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM workspace_invitations WHERE github_id=103 AND state='pending'").Scan(&invitation); err != nil {
		t.Fatal(err)
	}
	inv, err := publicid.New(f.pool).Public(t.Context(), "invitation", invitation, 0)
	if err != nil {
		t.Fatal(err)
	}
	other := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	other.login(t, "reviewer")
	if r := other.request("POST", "/workspace-invitations/received/"+inv, url.Values{"csrf_token": {other.csrf(t)}, "action": {"accept"}}); r.Code != http.StatusForbidden {
		t.Fatalf("another account accepted the invitation: %d", r.Code)
	}
	if strings.Contains(other.request("GET", "/workspaces", nil).Body.String(), "Invitations for you") {
		t.Fatal("invitation shown to the wrong account")
	}
	dev := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	dev.login(t, "dev")
	directory := dev.request("GET", "/workspaces", nil).Body.String()
	if !strings.Contains(directory, "Invitations for you") || !strings.Contains(directory, "Access to acme/api") || !strings.Contains(directory, `class="switcher-badge"`) && !strings.Contains(directory, "1 invitation waiting") {
		t.Fatalf("received invitation not shown: %s", directory)
	}
	csrf := dev.csrf(t)
	if r := dev.request("POST", "/workspace-invitations/received/"+inv, url.Values{"action": {"accept"}}); r.Code != http.StatusForbidden {
		t.Fatalf("accept without CSRF: %d", r.Code)
	}
	accepted := dev.request("POST", "/workspace-invitations/received/"+inv, url.Values{"csrf_token": {csrf}, "action": {"accept"}})
	if accepted.Code != http.StatusSeeOther || accepted.Header().Get("Location") != "/dashboard?saved=joined" {
		t.Fatalf("a Developer who joins starts on the Overview's next steps: %d %s", accepted.Code, accepted.Header().Get("Location"))
	}
	if _, err := f.service.Authorize(t.Context(), f.dev, f.repo, "operate", false); err != nil {
		t.Fatalf("accepted invitation granted no access: %v", err)
	}
	if strings.Contains(dev.request("GET", "/workspaces", nil).Body.String(), "Invitations for you") {
		t.Fatal("answered invitation still listed")
	}
}

func TestWorkspaceDeletionNeedsASoleOwnerWithoutConnections(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	w, _ := f.service.Read(ctx, f.owner, f.wid)
	if err := f.service.Delete(ctx, f.owner, f.wid, w.Revision, "DELETE"); !errors.Is(err, ErrDeleteMembers) {
		t.Fatalf("deleted a shared workspace: %v", err)
	}
	if err := f.service.Delete(ctx, f.admin, f.wid, w.Revision, "DELETE"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an admin deleted the workspace: %v", err)
	}
	solo, err := f.service.Create(ctx, f.owner, "Solo team")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := f.service.Read(ctx, f.owner, solo)
	if err := f.service.Delete(ctx, f.owner, f.wid, w.Revision, "DELETE"); !errors.Is(err, ErrDeleteMembers) {
		t.Fatal("membership check skipped", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE installations SET workspace_id=$1 WHERE workspace_id=$2", solo, f.wid); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Delete(ctx, f.owner, solo, s.Revision, "DELETE"); !errors.Is(err, ErrDeleteInstallation) {
		t.Fatalf("deleted a workspace with a connected installation: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE installations SET workspace_id=$1 WHERE workspace_id=$2", f.wid, solo); err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{"delete", "Delete", "Solo team", ""} {
		if err := f.service.Delete(ctx, f.owner, solo, s.Revision, typed); !errors.Is(err, ErrDeleteConfirmation) {
			t.Fatalf("confirmation %q must be DELETE in capitals: %v", typed, err)
		}
	}
	router := configuredFixtureRouter(t, f, "http://localhost", true)
	owner := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	owner.login(t, "owner")
	id, _ := publicid.New(f.pool).Public(ctx, "workspace", solo, 0)
	settings := owner.request("GET", "/workspaces/"+id+"/settings", nil).Body.String()
	if !strings.Contains(settings, "Delete workspace…") || !strings.Contains(settings, "Invite a teammate") {
		t.Fatal("sole owner was not offered deletion with the handover alternative")
	}
	if shared := owner.request("GET", "/workspaces/"+publicWorkspace(t, f)+"/settings", nil).Body.String(); !strings.Contains(shared, "Delete workspace…") || !strings.Contains(shared, "Deleting ends access for all members") {
		t.Fatal("owner should reach F4 durable deletion even with other members")
	}
	deleted := owner.request("POST", "/workspaces/"+id+"/delete", url.Values{"csrf_token": {owner.csrf(t)}, "workspace_revision": {strconv.FormatInt(s.Revision, 10)}, "confirm": {"DELETE"}})
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != "/workspaces?saved=deleted" {
		t.Fatalf("delete workspace: %d %s", deleted.Code, deleted.Header().Get("Location"))
	}
	if _, err := f.service.Read(ctx, f.owner, solo); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted workspace still readable: %v", err)
	}
	if r := owner.request("GET", "/workspaces/"+id+"/settings", nil); r.Code != http.StatusNotFound {
		t.Fatalf("deleted workspace settings: %d", r.Code)
	}
}

func TestMemberProfilesAndActivityRespectWorkspaceAccess(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo, f.otherRepo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	var private int64
	if err := f.pool.QueryRow(t.Context(), "INSERT INTO pull_requests(repo_id,number,head_sha,head_repo_id,generation,title) VALUES($1,7,'sha7',302,1,'Secret change') RETURNING id", f.otherRepo).Scan(&private); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int64{f.pr, private} {
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,requested_at)
			SELECT id,1,0,generation,head_sha,'manual','build',$2,'activity-'||id,'pending',now(),now() FROM pull_requests WHERE id=$1`, pr, f.dev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET title='Public change' WHERE id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	router := configuredFixtureRouter(t, f, "http://localhost", true)
	base, dev := "/workspaces/"+publicWorkspace(t, f), publicUser(t, f, f.dev)
	for login, wantPrivate := range map[string]bool{"owner": true, "reviewer": false} {
		b := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
		b.login(t, login)
		r := b.request("GET", base+"/members/"+dev, nil)
		body := r.Body.String()
		if r.Code != 200 || !strings.Contains(body, "Recent activity") || !strings.Contains(body, "Public change") {
			t.Fatalf("%s member profile: %d", login, r.Code)
		}
		if strings.Contains(body, "Secret change") != wantPrivate {
			t.Fatalf("%s saw private build activity: %v", login, !wantPrivate)
		}
		if strings.Contains(body, "dev@example.com") {
			t.Fatal("member profile exposed email")
		}
	}
	self := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	self.login(t, "dev")
	own := self.request("GET", "/protected", nil).Body.String()
	if !strings.Contains(own, "Recent activity") || !strings.Contains(own, "Secret change") || !strings.Contains(own, "Joined") {
		t.Fatal("own profile activity missing")
	}
}

func TestCreatedWorkspaceOpensItsSetupChecklist(t *testing.T) {
	f := newFixture(t)
	owner := testBrowser{router: configuredFixtureRouter(t, f, "http://localhost", true), cookies: map[string]*http.Cookie{}}
	owner.login(t, "owner")
	created := owner.request("POST", "/workspaces", url.Values{"csrf_token": {owner.csrf(t)}, "name": {"Launch team"}})
	location := created.Header().Get("Location")
	if created.Code != http.StatusSeeOther || !strings.HasSuffix(location, "/setup") {
		t.Fatalf("create workspace: %d %s", created.Code, location)
	}
	setup := owner.request("GET", location, nil).Body.String()
	for _, want := range []string{"Set up your workspace", "1 of 4 done", "Invite your teammates", "Connect repositories", `name="return" value="setup"`} {
		if !strings.Contains(setup, want) {
			t.Fatalf("setup checklist missing %q", want)
		}
	}
	if !strings.Contains(setup, "Launch team") || !strings.Contains(owner.request("GET", "/workspaces", nil).Body.String(), "Launch team") {
		t.Fatal("new workspace not selected")
	}
}
