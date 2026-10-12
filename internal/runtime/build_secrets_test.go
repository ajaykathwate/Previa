package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/secrets"
)

func buildSecretProvider(t *testing.T) *secrets.Provider {
	t.Helper()
	keys, err := secrets.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)), "1")
	if err != nil {
		t.Fatal(err)
	}
	var sealed []secrets.SealedSecret
	for name, value := range map[string]string{"NPM_TOKEN": "npm-test-credential", "REGISTRY_AUTH": "ghcr.io octo registry-test-credential", "RUNTIME_ONLY": "runtime-test-credential"} {
		ciphertext, version, err := keys.Seal(sameRepo.RepoID, name, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		sealed = append(sealed, secrets.SealedSecret{Name: name, Value: ciphertext, Version: version, Build: name != "RUNTIME_ONLY"})
	}
	provider, err := secrets.NewProvider(&memorySealed{values: map[int64][]secrets.SealedSecret{sameRepo.RepoID: sealed}}, keys)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestBuildCredentialsFollowCurrentTrustAndAreRedacted(t *testing.T) {
	fork := sameRepo
	fork.HeadRepoID, fork.Fork = 555, true
	unverified := sameRepo
	unverified.HeadRepoID = 0
	flaggedFork := sameRepo
	flaggedFork.Fork = true
	for _, tc := range []struct {
		name         string
		first, fresh Target
		allowed      bool
	}{
		{"trusted", sameRepo, sameRepo, true},
		{"fork", fork, fork, false},
		{"unverified", unverified, unverified, false},
		{"became-fork-during-preparation", sameRepo, flaggedFork, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mounted []byte
			builder := &fakeBuilder{err: &builds.Error{Code: builds.CodeBuildFailed, Message: "Rejected npm-test-credential", Err: errors.New("registry-test-credential")}}
			builder.observe = func(job builds.Job) {
				if got := len(job.Secrets) == 1 && string(job.Secrets["NPM_TOKEN"]) == "npm-test-credential" && len(job.Registries) == 1 && job.Registries[0].Secret == "registry-test-credential"; got != tc.allowed {
					t.Fatalf("build credentials allowed=%v, want %v", got, tc.allowed)
				}
				if !tc.allowed && (len(job.Secrets) != 0 || len(job.Registries) != 0) {
					t.Fatal("untrusted build received credentials")
				}
				mounted = job.Secrets["NPM_TOKEN"]
				if tc.allowed {
					if _, err := io.WriteString(job.Logs, "npm-test-credential registry-test-credential\n"); err != nil {
						t.Fatal(err)
					}
				}
			}
			r, sink := orchestration(&scriptedTargets{answers: []func() (Target, error){current(tc.first), current(tc.fresh)}}, &fakeSources{dir: t.TempDir()}, builder, &recordingSecrets{})
			r.secrets = buildSecretProvider(t)
			r.logs = func(_ context.Context, _ environments.Attempt, redactor *secrets.Redactor) (LogSink, error) {
				return redactingSink{inner: sink, redactor: redactor}, nil
			}
			_, err := r.Build(t.Context(), testRequest, environments.Attempt{})
			if CodeOf(err) != CodeBuild {
				t.Fatalf("failure classification: %v", err)
			}
			if tc.allowed {
				for _, value := range []string{"npm-test-credential", "registry-test-credential"} {
					if strings.Contains(fmt.Sprint(err), value) || strings.Contains(sink.String(), value) {
						t.Fatal("a build credential leaked through logs or a returned error")
					}
				}
				if !bytes.Equal(mounted, make([]byte, len(mounted))) {
					t.Fatal("decrypted build secret was not wiped")
				}
			}
		})
	}
}
