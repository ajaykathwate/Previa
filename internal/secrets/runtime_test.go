package secrets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type sealedFunc func(context.Context, int64) ([]SealedSecret, error)

func (f sealedFunc) SealedSecrets(ctx context.Context, repoID int64) ([]SealedSecret, error) {
	return f(ctx, repoID)
}

func sealedFor(t *testing.T, k *Keyring, repoID int64, values map[string]string) []SealedSecret {
	t.Helper()
	var out []SealedSecret
	for name, value := range values {
		sealed, version, err := k.Seal(repoID, name, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, SealedSecret{Name: name, Value: sealed, Version: version})
	}
	return out
}

func TestProviderWithholdsSecretsFromUntrustedSources(t *testing.T) {
	k := testKeyring(t)
	reads := 0
	provider, err := NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) {
		reads++
		// Even corrupt ciphertext must not matter: untrusted sources decrypt nothing.
		return []SealedSecret{{Name: "API_KEY", Value: []byte("corrupt"), Version: 1}}, nil
	}), k)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []Source{
		{BaseRepoID: 100, HeadRepoID: 101}, // fork
		{BaseRepoID: 100, HeadRepoID: 0},   // deleted fork or unknown head
		{BaseRepoID: 0, HeadRepoID: 0},     // unverified
	} {
		env, err := provider.ForContainer(t.Context(), 7, source, []string{"API_KEY=from-yaml", "PORT=3000"})
		if err != nil {
			t.Fatal(err)
		}
		if !env.Withheld || !slices.Equal(env.Environ(), []string{"API_KEY=from-yaml", "PORT=3000"}) || len(env.Notices) != 1 {
			t.Fatalf("untrusted source %+v: %+v", source, env)
		}
	}
	if reads != 0 {
		t.Fatal("secrets were read for an untrusted source")
	}
}

func TestProviderMergesSecretsOverYAMLForTrustedSources(t *testing.T) {
	k := testKeyring(t)
	provider, err := NewProvider(sealedFunc(func(_ context.Context, repoID int64) ([]SealedSecret, error) {
		if repoID != 7 {
			t.Errorf("wrong repository %d", repoID)
		}
		return sealedFor(t, k, 7, map[string]string{"API_KEY": "sk_test_owner", "DATABASE_URL": "postgres://preview"}), nil
	}), k)
	if err != nil {
		t.Fatal(err)
	}
	env, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100}, []string{"API_KEY=from-yaml", "PORT=3000"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"API_KEY=sk_test_owner", "DATABASE_URL=postgres://preview", "PORT=3000"}
	if env.Withheld || !slices.Equal(env.Environ(), want) {
		t.Fatalf("environment = %v; want %v", env.Environ(), want)
	}
	if len(env.Notices) != 1 || !strings.Contains(env.Notices[0], "API_KEY") || strings.Contains(env.Notices[0], "sk_test_owner") {
		t.Fatalf("override must be noted by name only: %v", env.Notices)
	}
	redactor, err := env.Redactor("ghs_platformtoken123")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(redactor.Redact([]byte("key=sk_test_owner db=postgres://preview tok=ghs_platformtoken123 port=3000"))); got != "key=[redacted] db=[redacted] tok=[redacted] port=3000" {
		t.Fatalf("redactor: %q", got)
	}
	env.Wipe()
	if len(env.Environ()) != 0 {
		t.Fatal("Wipe must drop the environment")
	}
}

func TestProviderFailsClosedOnUndecryptableValues(t *testing.T) {
	k := testKeyring(t)
	good := sealedFor(t, k, 7, map[string]string{"GOOD": "value-one"})
	provider, err := NewProvider(sealedFunc(func(context.Context, int64) ([]SealedSecret, error) {
		return append(good, SealedSecret{Name: "MOVED", Value: good[0].Value, Version: 1}), nil
	}), k)
	if err != nil {
		t.Fatal(err)
	}
	env, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100}, nil)
	if !errors.Is(err, ErrSecretsUnavailable) || env != nil || strings.Contains(err.Error(), "value-one") {
		t.Fatalf("a value moved to another name must fail closed without leaking: %v", err)
	}
	if _, err := NewProvider(nil, k); err == nil {
		t.Fatal("provider requires a store")
	}
	if _, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100}, []string{"broken"}); err == nil {
		t.Fatal("malformed YAML env must be rejected")
	}
}

func TestProviderWithoutKeyringServesOnlyRepositoriesWithoutSecrets(t *testing.T) {
	k := testKeyring(t)
	stored := sealedFor(t, k, 7, map[string]string{"TOKEN": "value-one"})
	provider, err := NewProvider(sealedFunc(func(_ context.Context, repoID int64) ([]SealedSecret, error) {
		if repoID == 7 {
			return stored, nil
		}
		return nil, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	env, err := provider.ForContainer(t.Context(), 8, Source{BaseRepoID: 100, HeadRepoID: 100}, []string{"MODE=preview"})
	if err != nil || len(env.Environ()) != 1 {
		t.Fatalf("a repository without secrets: %v %v", env, err)
	}
	if env, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100, HeadRepoID: 100}, nil); !errors.Is(err, ErrSecretsUnavailable) || env != nil {
		t.Fatalf("a repository with secrets must fail closed without a keyring: %v", err)
	}
	// Forks never needed the secrets, so they never needed the keyring either.
	if _, err := provider.ForContainer(t.Context(), 7, Source{BaseRepoID: 100}, nil); err != nil {
		t.Fatal(err)
	}
}
