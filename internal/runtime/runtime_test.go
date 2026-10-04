package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
)

// memorySink records an attempt's log like buildlogs would receive it.
type memorySink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (m *memorySink) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}

func (m *memorySink) System(message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf.WriteString("[previa] " + message + "\n")
}

func (m *memorySink) Close(context.Context) error { return nil }

func (m *memorySink) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

// scriptedTargets answers successive Current calls from a script.
type scriptedTargets struct {
	mu      sync.Mutex
	answers []func() (Target, error)
	calls   int
}

func (s *scriptedTargets) Current(context.Context, environments.Request) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answer := s.answers[min(s.calls, len(s.answers)-1)]
	s.calls++
	return answer()
}

func current(t Target) func() (Target, error) { return func() (Target, error) { return t, nil } }
func refused(code string) func() (Target, error) {
	return func() (Target, error) { return Target{}, failure(code, "refused", false, nil) }
}

type fakeSources struct {
	err      error
	dir      string
	prepares int
	released int
}

func (f *fakeSources) Prepare(context.Context, source.Request) (*source.Prepared, error) {
	f.prepares++
	if f.err != nil {
		return nil, f.err
	}
	return &source.Prepared{ContextDir: f.dir, Dockerfile: "Dockerfile", Config: repoconfig.Result{Config: repoconfig.Config{Port: 3000, Env: []string{"MODE=preview"}}}}, nil
}

func (f *fakeSources) Release(*source.Prepared) error { f.released++; return nil }

type fakeBuilder struct {
	err  error
	jobs []builds.Job
}

func (f *fakeBuilder) Build(_ context.Context, job builds.Job) (builds.Image, error) {
	f.jobs = append(f.jobs, job)
	if f.err != nil {
		return builds.Image{}, f.err
	}
	return builds.Image{Ref: "previa/pr-3:g4-r2-0123456789ab", ID: "sha256:img"}, nil
}

type recordingSecrets struct {
	sources []secrets.Source
	yaml    [][]string
	err     error
}

func (r *recordingSecrets) ForContainer(_ context.Context, _ int64, source secrets.Source, yamlEnv []string) (*secrets.ContainerEnv, error) {
	r.sources = append(r.sources, source)
	r.yaml = append(r.yaml, yamlEnv)
	return nil, r.err
}

// orchestration builds a Runtime whose tested paths end before any Docker call.
func orchestration(targets Targets, sources *fakeSources, builder *fakeBuilder, provider *recordingSecrets) (*Runtime, *memorySink) {
	sink := &memorySink{}
	return &Runtime{
		targets: targets, sources: sources, builder: builder, secrets: provider,
		logs:   func(context.Context, environments.Attempt, *secrets.Redactor) (LogSink, error) { return sink, nil },
		policy: Policy{TmpfsBytes: 64 << 20, PIDs: 128, StartupTimeout: time.Minute, MaxPerRepo: 10},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now,
	}, sink
}

var testRequest = environments.Request{PullRequestID: 3, Generation: 4, Revision: 2, HeadSHA: "0123456789abcdef0123456789abcdef01234567", State: "open"}
var sameRepo = Target{PullRequestID: 3, RepoID: 2, InstallationID: 99, BaseRepoID: 100, HeadRepoID: 100}

func TestBuildRefusesStaleAttemptsBeforeAnyWork(t *testing.T) {
	sources := &fakeSources{}
	r, _ := orchestration(&scriptedTargets{answers: []func() (Target, error){refused(CodeNotCurrent)}}, sources, &fakeBuilder{}, &recordingSecrets{})
	if _, err := r.Build(t.Context(), testRequest, environments.Attempt{}); CodeOf(err) != CodeNotCurrent || sources.prepares != 0 {
		t.Fatalf("stale attempt: %v (prepares %d)", err, sources.prepares)
	}
}

func TestBuildReportsSourceAndBuildFailuresSafely(t *testing.T) {
	sources := &fakeSources{err: &source.Error{Code: source.CodeMissingDockerfile, Message: "The Dockerfile named in .previewrc.yaml does not exist."}}
	r, sink := orchestration(&scriptedTargets{answers: []func() (Target, error){current(sameRepo)}}, sources, &fakeBuilder{}, &recordingSecrets{})
	_, err := r.Build(t.Context(), testRequest, environments.Attempt{})
	if CodeOf(err) != CodeSource || messageOf(err) != "The Dockerfile named in .previewrc.yaml does not exist." || !strings.Contains(sink.String(), "Source preparation failed: The Dockerfile") {
		t.Fatalf("source failure: %v\n%s", err, sink)
	}
	builder := &fakeBuilder{err: &builds.Error{Code: builds.CodeBuildFailed, Message: "The build failed. The build log shows the failing step."}}
	sources = &fakeSources{dir: t.TempDir()}
	provider := &recordingSecrets{}
	r, _ = orchestration(&scriptedTargets{answers: []func() (Target, error){current(sameRepo)}}, sources, builder, provider)
	_, err = r.Build(t.Context(), testRequest, environments.Attempt{})
	if CodeOf(err) != CodeBuild || sources.released != 1 || len(provider.sources) != 0 {
		t.Fatalf("build failure: %v released %d secrets %d", err, sources.released, len(provider.sources))
	}
	job := builder.jobs[0]
	if job.BaseRepoID != 100 || job.HeadRepoID != 100 || job.HeadSHA != testRequest.HeadSHA || job.Revision != 2 || job.Dockerfile != "Dockerfile" {
		t.Fatalf("build job: %+v", job)
	}
}

func TestSecretsAreDecidedOnlyAfterAFreshRecheck(t *testing.T) {
	sources := &fakeSources{dir: t.TempDir()}
	provider := &recordingSecrets{}
	revoked := &scriptedTargets{answers: []func() (Target, error){current(sameRepo), refused(CodeInaccessible)}}
	r, _ := orchestration(revoked, sources, &fakeBuilder{}, provider)
	if _, err := r.Build(t.Context(), testRequest, environments.Attempt{}); CodeOf(err) != CodeInaccessible || len(provider.sources) != 0 || sources.released != 1 {
		t.Fatalf("revoked during build: %v secrets %d", err, len(provider.sources))
	}
	// A PR that became a fork's (or whose head was deleted) during the build is
	// asked about with an untrusted source; the provider then withholds secrets.
	fork := sameRepo
	fork.HeadRepoID, fork.Fork = 555, true
	provider = &recordingSecrets{err: errors.New("stop before Docker")}
	r, _ = orchestration(&scriptedTargets{answers: []func() (Target, error){current(sameRepo), current(fork)}}, &fakeSources{dir: t.TempDir()}, &fakeBuilder{}, provider)
	if _, err := r.Build(t.Context(), testRequest, environments.Attempt{}); CodeOf(err) != CodeSecrets {
		t.Fatalf("secrets failure: %v", err)
	}
	if len(provider.sources) != 1 || provider.sources[0].Trusted() || provider.yaml[0][0] != "MODE=preview" {
		t.Fatalf("secrets were requested for an untrusted source as trusted: %+v", provider.sources)
	}
}
