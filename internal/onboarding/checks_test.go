package onboarding

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// fakeTree is a commit as a map of paths.
type fakeTree struct {
	files map[string]string
	dirs  map[string]bool
	links map[string]bool
	large map[string]bool
	err   error // returned by every read
	reads []string
}

func (f *fakeTree) Config(ctx context.Context) ([]byte, error) {
	return f.Read(ctx, repoconfig.Filename, repoconfig.MaxConfigBytes)
}

func (f *fakeTree) Stat(_ context.Context, name string) (string, error) {
	f.reads = append(f.reads, "stat "+name)
	switch {
	case f.err != nil:
		return "", f.err
	case f.large[name]:
		return EntryLarge, nil
	case f.links[name]:
		return EntryLink, nil
	case f.dirs[name]:
		return EntryDir, nil
	}
	if _, ok := f.files[name]; ok {
		return EntryFile, nil
	}
	return EntryMissing, nil
}

func (f *fakeTree) Read(_ context.Context, name string, limit int64) ([]byte, error) {
	f.reads = append(f.reads, "read "+name)
	if f.err != nil {
		return nil, f.err
	}
	if f.dirs[name] || f.links[name] {
		return nil, ErrNotRegular
	}
	content, ok := f.files[name]
	if !ok {
		return nil, ErrMissing
	}
	if int64(len(content)) > limit {
		return nil, ErrTooLarge
	}
	return []byte(content), nil
}

type testCatalog map[string][]string

func (c testCatalog) Versions(kind string) []string { return c[kind] }
func (c testCatalog) Image(kind, version string) (string, bool) {
	return kind + ":" + version, slices.Contains(c[kind], version)
}

var testPlatform = repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: testCatalog{"postgres": {"17", "16"}, "redis": {"7"}}}

func check(t *testing.T, tree *fakeTree) Result {
	t.Helper()
	r, err := Check(t.Context(), tree, testPlatform)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheckExplainsAMissingConfiguration(t *testing.T) {
	r := check(t, &fakeTree{})
	if r.Passed() || r.Config != ConfigMissing || r.RootDockerfile {
		t.Fatalf("no files: %+v", r)
	}
	r = check(t, &fakeTree{files: map[string]string{"Dockerfile": "FROM scratch"}})
	if r.Passed() || r.Config != ConfigMissing || !r.RootDockerfile {
		t.Fatalf("a root Dockerfile is noted for the Own Dockerfile kit: %+v", r)
	}
	r = check(t, &fakeTree{dirs: map[string]bool{".previewrc.yaml": true}})
	if r.Passed() || r.Config != ConfigInvalid || len(r.Issues) != 1 || r.Issues[0].Code != "invalid_file" {
		t.Fatalf("a directory is not a configuration: %+v", r)
	}
	r = check(t, &fakeTree{files: map[string]string{".previewrc.yaml": strings.Repeat("#", repoconfig.MaxConfigBytes+1)}})
	if r.Passed() || r.Issues[0].Code != "too_large" {
		t.Fatalf("an oversized configuration: %+v", r)
	}
}

func TestCheckReportsTheParsersIssuesWithTheirLines(t *testing.T) {
	r := check(t, &fakeTree{files: map[string]string{".previewrc.yaml": "port: 0\nservices:\n  postgres: 15\n"}})
	if r.Passed() || r.Config != ConfigInvalid {
		t.Fatalf("invalid configuration passed: %+v", r)
	}
	fields := map[string]Issue{}
	for _, issue := range r.Issues {
		fields[issue.Field] = issue
	}
	if fields["port"].Code != "invalid_port" || !strings.Contains(fields["services.postgres"].Message, "17 or 16") || fields["services.postgres"].Line != 3 {
		t.Fatalf("issues should name the key, the offered versions and the line: %+v", r.Issues)
	}
}

func TestCheckPassesAReadyCommitAndKeepsOnlySafeFacts(t *testing.T) {
	config := `port: 8080
health_check_path: /healthz
docs_path: /docs
context: api
dockerfile: build/Dockerfile
services: { postgres: 17, redis: 7 }
setup: ["bin/migrate", "up"]
processes:
  - name: worker
    command: ["bin/worker"]
env:
  - RAILS_ENV=production
  - API_TOKEN=do-not-store-me
`
	tree := &fakeTree{files: map[string]string{".previewrc.yaml": config, "api/build/Dockerfile": "FROM scratch"}}
	r := check(t, tree)
	if !r.Passed() || r.Port != 8080 || r.HealthCheckPath != "/healthz" || r.DocsPath != "/docs" || r.Context != "api" ||
		r.DockerfileState != PathFound || r.ContextState != PathFound || len(r.Services) != 2 || r.Services[0].Kind != "postgres" ||
		!slices.Equal(r.Setup, []string{"bin/migrate", "up"}) || len(r.Processes) != 1 || !slices.Equal(r.EnvNames, []string{"API_TOKEN", "RAILS_ENV"}) {
		t.Fatalf("ready commit: %+v", r)
	}
	data, err := encodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "do-not-store-me") || strings.Contains(string(data), "production") {
		t.Fatalf("configuration values must never be stored: %s", data)
	}
	decoded, ok := decodeResult(data)
	if !ok || !decoded.Passed() || decoded.Port != 8080 {
		t.Fatalf("round trip: %+v %v", decoded, ok)
	}
	if _, ok := decodeResult([]byte(`{"status":"maybe"}`)); ok {
		t.Fatal("an unknown status is not a result")
	}
}

func TestCheckFindsTheBuildContextAndDockerfile(t *testing.T) {
	withConfig := func(config string, extra map[string]string) *fakeTree {
		files := map[string]string{".previewrc.yaml": config}
		for k, v := range extra {
			files[k] = v
		}
		return &fakeTree{files: files, dirs: map[string]bool{}, links: map[string]bool{}, large: map[string]bool{}}
	}
	r := check(t, withConfig("port: 3000\n", nil))
	if r.Passed() || r.DockerfileState != PathMissing || r.ContextState != PathFound {
		t.Fatalf("no Dockerfile at the root: %+v", r)
	}
	tree := withConfig("context: web\n", nil)
	r = check(t, tree)
	if r.Passed() || r.ContextState != PathMissing || !slices.Contains(tree.reads, "stat web") {
		t.Fatalf("a missing context is named, not just its Dockerfile: %+v %v", r, tree.reads)
	}
	tree = withConfig("context: web\n", nil)
	tree.dirs["web"] = true
	if r = check(t, tree); r.Passed() || r.ContextState != PathFound || r.DockerfileState != PathMissing {
		t.Fatalf("the context exists but has no Dockerfile: %+v", r)
	}
	tree = withConfig("context: web\n", map[string]string{"web": "a file"})
	if r = check(t, tree); r.Passed() || r.ContextState != PathNotDir {
		t.Fatalf("a file is not a context: %+v", r)
	}
	tree = withConfig("port: 3000\n", nil)
	tree.dirs["Dockerfile"] = true
	if r = check(t, tree); r.Passed() || r.DockerfileState != PathNotFile {
		t.Fatalf("a directory named Dockerfile: %+v", r)
	}
	tree = withConfig("port: 3000\n", nil)
	tree.links["Dockerfile"] = true
	if r = check(t, tree); !r.Passed() || r.DockerfileState != PathUnverified {
		t.Fatalf("the build resolves links inside the tree: %+v", r)
	}
	tree = withConfig("port: 3000\n", nil)
	tree.large["Dockerfile"] = true
	if r = check(t, tree); !r.Passed() || r.DockerfileState != PathFound {
		t.Fatalf("a large Dockerfile still exists: %+v", r)
	}
}

func TestCheckAppliesTheBuildsSubmoduleRule(t *testing.T) {
	manifest := "[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = https://example.com/lib.git\n"
	tree := &fakeTree{files: map[string]string{".previewrc.yaml": "port: 3000\n", "Dockerfile": "FROM scratch", ".gitmodules": manifest}}
	if r := check(t, tree); r.Passed() || !r.Submodules {
		t.Fatalf("submodules under the root context fail the build: %+v", r)
	}
	tree = &fakeTree{files: map[string]string{".previewrc.yaml": "context: app\n", "app/Dockerfile": "FROM scratch", ".gitmodules": manifest}}
	if r := check(t, tree); !r.Passed() || r.Submodules {
		t.Fatalf("a submodule outside the context does not: %+v", r)
	}
	tree.files[".gitmodules"] = "[submodule \"x\"]\n\tpath = app/vendor\n"
	if r := check(t, tree); r.Passed() || !r.Submodules {
		t.Fatalf("a submodule inside the context does: %+v", r)
	}
}

func TestCheckReturnsGitHubFailuresInsteadOfAResult(t *testing.T) {
	_, err := Check(t.Context(), &fakeTree{err: &AttemptError{Code: FailRateLimited}}, testPlatform)
	if failureCode(err) != FailRateLimited {
		t.Fatalf("rate limit: %v", err)
	}
	if failureCode(errors.New("boom")) != FailInternal || failureCode(context.DeadlineExceeded) != FailGitHubDown {
		t.Fatal("unclassified errors are internal; a deadline means GitHub was slow")
	}
}
