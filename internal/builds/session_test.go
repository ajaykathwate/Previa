package builds

import (
	"testing"

	"github.com/moby/buildkit/session/auth"
)

func TestRegistryAuthAnswersOnlyForItsHosts(t *testing.T) {
	a := registryAuth{logins: []RegistryLogin{{Host: "ghcr.io", Username: "octo", Secret: "t1"}, {Host: "docker.io", Username: "hub", Secret: "t2"}}}
	for host, want := range map[string]string{"ghcr.io": "t1", "GHCR.IO": "t1", "registry-1.docker.io": "t2", "quay.io": ""} {
		res, err := a.Credentials(t.Context(), &auth.CredentialsRequest{Host: host})
		if err != nil || res.Secret != want {
			t.Errorf("%s: %+v %v", host, res, err)
		}
	}
	if got := sessionAttachables(Job{}); len(got) != 0 {
		t.Fatal("a build without secrets or logins gets no providers")
	}
	if got := sessionAttachables(Job{BaseRepoID: 1, HeadRepoID: 1, Secrets: map[string][]byte{"A": []byte("b")}, Registries: a.logins}); len(got) != 2 {
		t.Fatalf("providers: %d", len(got))
	}
	for _, head := range []int64{0, 2} {
		if got := sessionAttachables(Job{BaseRepoID: 1, HeadRepoID: head, Secrets: map[string][]byte{"A": []byte("b")}, Registries: a.logins}); len(got) != 0 {
			t.Fatal("untrusted builds must never receive providers")
		}
	}
}
