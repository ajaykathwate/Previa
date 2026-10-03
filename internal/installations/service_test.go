package installations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/go-github/v91/github"
)

type fakeStore struct {
	revision       int64
	linked         bool
	linkErr        error
	events         int
	status         string
	added, removed []RepositoryInfo
}

func (f *fakeStore) Prepare(context.Context, int64) (int64, error) { return f.revision, nil }
func (f *fakeStore) Link(_ context.Context, _, _, revision int64, _ string, _ []RepositoryInfo) error {
	if revision != f.revision {
		return ErrSuperseded
	}
	if f.linkErr != nil {
		return f.linkErr
	}
	f.linked = true
	return nil
}
func (f *fakeStore) List(context.Context, int64) ([]InstallationInfo, error) { return nil, nil }
func (f *fakeStore) ApplyEvent(_ context.Context, _ int64, status, _ string, added, removed []RepositoryInfo, _ bool) error {
	f.events++
	f.status = status
	f.added = added
	f.removed = removed
	f.revision++
	return nil
}

type fakeAuthorization struct {
	err   error
	calls int
}

func (a *fakeAuthorization) AuthCodeURL(state string) string {
	return "https://github.com/login/oauth/authorize?state=" + state
}
func (a *fakeAuthorization) Verify(context.Context, string, int64, int64) ([]RepositoryInfo, error) {
	a.calls++
	return []RepositoryInfo{{ID: 456, FullName: "owner/repo", Accessible: true}}, a.err
}

type fakeApp struct {
	invalidated  int
	installation *github.Installation
	client       *github.Client
	beforeGet    func()
}

func (a *fakeApp) Installation(context.Context, int64) (*github.Installation, error) {
	if a.beforeGet != nil {
		a.beforeGet()
	}
	return a.installation, nil
}
func (a *fakeApp) InstallationClient(int64) (*github.Client, error) { return a.client, nil }
func (a *fakeApp) Invalidate(int64)                                 { a.invalidated++ }

type fakeCleanup struct {
	calls int
	err   error
}

func (c *fakeCleanup) RequestInstallationCleanup(context.Context, int64) error {
	c.calls++
	return c.err
}
func testService(t *testing.T) (*Service, *fakeStore, *fakeApp, *fakeAuthorization) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"id":456,"full_name":"owner/repo"}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(new(server.URL+"/"), nil))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	app := &fakeApp{installation: &github.Installation{ID: new(int64(99)), Account: &github.User{Login: new("owner")}}, client: client}
	authorization := &fakeAuthorization{}
	return NewService(store, app, authorization, nil), store, app, authorization
}
func TestLinkRequiresIdentityAndAccessAndDiscardsStaleSnapshot(t *testing.T) {
	for _, scenario := range []string{"success", "forbidden", "superseded", "suspended", "ownership conflict"} {
		t.Run(scenario, func(t *testing.T) {
			service, store, app, authorization := testService(t)
			switch scenario {
			case "forbidden":
				authorization.err = ErrForbidden
			case "superseded":
				app.beforeGet = func() { store.revision++ }
			case "suspended":
				now := github.Timestamp{}
				app.installation.SuspendedAt = &now
			case "ownership conflict":
				store.linkErr = ErrForbidden
			}
			err := service.Link(t.Context(), "code", 1, 42, 99)
			if scenario == "success" {
				if err != nil || !store.linked {
					t.Fatalf("link failed: %v", err)
				}
			} else if err == nil || store.linked {
				t.Fatal("unsafe link succeeded")
			}
		})
	}
}
func TestLifecycleFixturePayloads(t *testing.T) {
	service, store, app, _ := testService(t)
	cleanup := &fakeCleanup{}
	service.cleanup = cleanup
	for _, fixture := range []struct{ file, kind string }{{"installation.json", "installation"}, {"installation_repositories.json", "installation_repositories"}} {
		payload, err := os.ReadFile("testdata/" + fixture.file)
		if err != nil {
			t.Fatal(err)
		}
		event, err := github.ParseWebHook(fixture.kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.HandleEvent(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if store.events != 2 || app.invalidated != 2 || len(store.added) != 1 || store.added[0].ID != 789 || len(store.removed) != 1 {
		t.Fatal("fixture repository sync did not apply")
	}
	for _, action := range []string{"suspend", "unsuspend", "new_permissions_accepted", "deleted"} {
		event := &github.InstallationEvent{Action: new(action), Installation: app.installation}
		if err := service.HandleEvent(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if cleanup.calls != 2 || store.status != "inactive" {
		t.Fatal("uninstall/suspension cleanup boundary not called")
	}
	cleanup.err = errors.New("cleanup unavailable")
	if err := service.HandleEvent(t.Context(), &github.InstallationEvent{Action: new("deleted"), Installation: app.installation}); err == nil {
		t.Fatal("cleanup error swallowed")
	}
	for _, event := range []any{nil, (*github.InstallationEvent)(nil), &github.InstallationEvent{Installation: &github.Installation{ID: new(int64(0))}}, &github.InstallationRepositoriesEvent{Action: new("unknown"), Installation: app.installation}} {
		if err := service.HandleEvent(t.Context(), event); err == nil {
			t.Fatal("invalid lifecycle event accepted")
		}
	}
}
