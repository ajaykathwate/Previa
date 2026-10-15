package secrets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Builds get only the secrets the owner allowed, never for untrusted sources;
// REGISTRY_AUTH logs registries in and is not given to Dockerfile steps.
func TestForBuildGivesTrustedBuildsOnlyAllowedSecrets(t *testing.T) {
	k := testKeyring(t)
	sealed := sealedFor(t, k, 7, map[string]string{
		"NPM_TOKEN":      "npm-secret-1234",
		"RUNTIME_ONLY":   "runtime-value-1",
		RegistryAuthName: "ghcr.io octo ghp_registry_token\n# a comment\nhttps://Registry.Example.com:5000 robot pass-word-77",
	})
	for i := range sealed {
		sealed[i].Build = sealed[i].Name != "RUNTIME_ONLY"
	}
	provider, err := NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) { return sealed, nil }), k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := provider.ForBuild(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.Names(), []string{"NPM_TOKEN"}) || string(b.Values()["NPM_TOKEN"]) != "npm-secret-1234" {
		t.Fatalf("build secrets: %v", b.Names())
	}
	want := []RegistryLogin{{Host: "ghcr.io", Username: "octo", Secret: "ghp_registry_token"}, {Host: "registry.example.com:5000", Username: "robot", Secret: "pass-word-77"}}
	if !slices.Equal(b.Registries(), want) {
		t.Fatalf("registries: %+v", b.Registries())
	}
	if len(b.Notices) != 0 {
		t.Fatalf("unexpected notices: %v", b.Notices)
	}
	r, err := b.Redactor()
	if err != nil {
		t.Fatal(err)
	}
	if out := string(r.Redact([]byte("npm-secret-1234 ghp_registry_token pass-word-77 runtime-value-1"))); strings.Contains(out, "npm-secret") || strings.Contains(out, "ghp_") || strings.Contains(out, "pass-word") {
		t.Fatalf("build log redaction: %q", out)
	}
	b.Wipe()
	if len(b.Names()) != 0 || len(b.Registries()) != 0 {
		t.Fatal("Wipe keeps nothing")
	}

	for _, source := range []Source{{BaseRepoID: 100, HeadRepoID: 101}, {BaseRepoID: 100}} {
		b, err := provider.ForBuild(t.Context(), 7, source)
		if err != nil || !b.Withheld || len(b.Names()) != 0 || len(b.Registries()) != 0 || len(b.Notices) != 1 {
			t.Fatalf("untrusted %+v: %+v %v", source, b, err)
		}
	}
}

func TestForBuildWithoutAllowedSecretsNeedsNoKeyring(t *testing.T) {
	provider, err := NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) {
		return []SealedSecret{{Name: "RUNTIME_ONLY", Value: []byte("x"), Version: 1}}, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := provider.ForBuild(t.Context(), 7, Source{BaseRepoID: 1, HeadRepoID: 1}); err != nil || len(b.Names()) != 0 || b.Withheld {
		t.Fatalf("no build secrets: %+v %v", b, err)
	}
	provider, _ = NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) {
		return []SealedSecret{{Name: "NPM_TOKEN", Value: []byte("x"), Version: 1, Build: true}}, nil
	}), nil)
	if _, err := provider.ForBuild(t.Context(), 7, Source{BaseRepoID: 1, HeadRepoID: 1}); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("an allowed secret without a keyring fails closed: %v", err)
	}
}

func TestRegistryAuthValidationAndRuntimeExclusion(t *testing.T) {
	for _, value := range []string{"broken line", "ghcr.io user abc", "ghcr.io:70000 user token", "ghcr.io user token\nghcr.io other token", "# empty"} {
		if err := validateRegistryAuth([]byte(value)); err == nil || strings.Contains(err.Error(), value) {
			t.Fatalf("invalid registry credentials must be rejected without echoing them: %v", err)
		}
	}
	k := testKeyring(t)
	sealed := sealedFor(t, k, 7, map[string]string{RegistryAuthName: "ghcr.io octo private-token"})
	provider, err := NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) { return sealed, nil }), k)
	if err != nil {
		t.Fatal(err)
	}
	env, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100}, nil)
	if err != nil || len(env.Environ()) != 0 {
		t.Fatalf("registry credentials reached the application: %v", err)
	}
	provider, err = NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) {
		t.Fatal("forks must not even read the sealed store")
		return nil, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := provider.ForBuild(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 101}); err != nil || !b.Withheld || len(b.Values()) > 0 {
		t.Fatalf("fork credentials: %v", err)
	}
}
