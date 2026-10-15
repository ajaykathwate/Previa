package secrets_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/ajaykathwatee/previa/internal/secrets"
)

func TestBuildSecretOptInOwnerAndCSRF(t *testing.T) {
	f := setup(t)
	k := keyring(t, 1)
	store := secrets.NewRepository(f.pool)
	s, err := secrets.NewService(store, k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), f.owner, f.repoAvailable, "NPM_TOKEN", []byte("npm-test-token")); err != nil {
		t.Fatal(err)
	}
	provider, err := secrets.NewProvider(store, k)
	if err != nil {
		t.Fatal(err)
	}
	trusted := secrets.Source{BaseRepoID: 100, HeadRepoID: 100}
	build, err := provider.ForBuild(t.Context(), f.repoAvailable, trusted)
	if err != nil || len(build.Names()) != 0 {
		t.Fatalf("new secrets default to runtime only: %v", err)
	}
	b, csrf := newBrowser(t, f, s)
	path := fmt.Sprintf("/repositories/%d/secrets/build", f.repoAvailable)
	form := url.Values{"name": {"NPM_TOKEN"}, "build": {"true"}}
	if w := b.do("POST", path, form); w.Code != http.StatusForbidden {
		t.Fatalf("build opt-in requires CSRF: %d", w.Code)
	}
	form.Set("csrf_token", csrf)
	if w := b.do("POST", path, form); w.Code != http.StatusSeeOther {
		t.Fatalf("opt-in: %d", w.Code)
	}
	build, err = provider.ForBuild(t.Context(), f.repoAvailable, trusted)
	if err != nil || string(build.Values()["NPM_TOKEN"]) != "npm-test-token" {
		t.Fatalf("opted-in secret was not provided: %v", err)
	}
	build.Wipe()
	if _, err := s.SetBuild(t.Context(), f.other, f.repoAvailable, "NPM_TOKEN", false); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("other owner changed permission: %v", err)
	}
	if _, err := s.Put(t.Context(), f.owner, f.repoAvailable, "NPM_TOKEN", []byte("replacement-token")); err != nil {
		t.Fatal(err)
	}
	build, err = provider.ForBuild(t.Context(), f.repoAvailable, trusted)
	if err != nil || string(build.Values()["NPM_TOKEN"]) != "replacement-token" {
		t.Fatalf("replacement must retain the owner's permission: %v", err)
	}
	build.Wipe()
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false WHERE id=$1", f.repoAvailable); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBuild(t.Context(), f.owner, f.repoAvailable, "NPM_TOKEN", true); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("revoked repository allowed access: %v", err)
	}
	if changed, err := s.SetBuild(t.Context(), f.owner, f.repoAvailable, "NPM_TOKEN", false); err != nil || !changed {
		t.Fatalf("owner must still be able to revoke build use: %v", err)
	}
}
