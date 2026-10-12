package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/moby/moby/client"
)

// Targets rechecks that a request may still run; *TargetStore implements it.
type Targets interface {
	Current(ctx context.Context, request environments.Request) (Target, error)
	// AdmissionScope returns the PR's base repository (BaseRepoID) and account
	// (AccountID), for the advisory capacity check.
	AdmissionScope(ctx context.Context, pullRequestID int64) (Target, error)
}

// Sources prepares an exact commit; *source.Preparer implements it.
type Sources interface {
	Prepare(ctx context.Context, request source.Request) (*source.Prepared, error)
	Release(prepared *source.Prepared) error
}

// Builder builds an image; *builds.Builder implements it.
type Builder interface {
	Build(ctx context.Context, job builds.Job) (builds.Image, error)
}

// Secrets decides and decrypts a container's environment; *secrets.Provider
// implements it.
type Secrets interface {
	ForContainer(ctx context.Context, repoID int64, source secrets.Source, yamlEnv []string) (*secrets.ContainerEnv, error)
}

// LogSink receives an attempt's build and startup output; *buildlogs.Writer
// implements it.
type LogSink interface {
	io.Writer
	System(message string)
	Close(ctx context.Context) error
}

// LogOpener opens an attempt's log with a redactor for every value the attempt
// may print. It returns an error when the attempt has no log stream.
type LogOpener func(ctx context.Context, attempt environments.Attempt, redactor *secrets.Redactor) (LogSink, error)

// Destinations reads repositories' owner-approved outbound connections; the
// outbound feature implements it.
type Destinations interface {
	Destinations(ctx context.Context, repoIDs ...int64) (map[int64][]egress.Destination, error)
}

// OutboundTargets lists the previews whose networks may carry outbound
// connections now; *TargetStore implements it.
type OutboundTargets interface {
	OutboundPreviews(ctx context.Context) ([]OutboundPreview, error)
}

// ComponentRecorder stores what a ready candidate runs next to its app;
// *TargetStore implements it.
type ComponentRecorder interface {
	RecordComponents(ctx context.Context, c Components) error
}

// Parts are the runtime's collaborators.
type Parts struct {
	// Instance is this database's identity (database.Instance); it names and
	// labels every object the runtime creates, and the runtime only ever
	// inspects, counts or removes objects that carry it.
	Instance string
	Docker   *client.Client
	// Admission serializes preview capacity checks across workers.
	Admission Locker
	Egress    *egress.Gateway
	Targets   Targets
	Sources   Sources
	Builder   Builder
	Secrets   Secrets
	Logs      LogOpener
	// Components records what each ready candidate runs; nil records nothing.
	Components ComponentRecorder
	// Destinations and Outbound enable owner-approved outbound connections
	// (with an egress gateway that forwards); nil leaves them off.
	Destinations Destinations
	Outbound     OutboundTargets
}

// Runtime turns an admitted request into a healthy, hardened preview
// container that is not yet routed. It implements environments.Runtime; the
// lifecycle transactions stay in internal/environments.
type Runtime struct {
	instance  string
	admission Locker
	docker    *client.Client
	egress    *egress.Gateway
	targets   Targets
	sources   Sources
	builder   Builder
	secrets   Secrets
	logs      LogOpener
	policy    Policy
	logger    *slog.Logger
	now       func() time.Time

	components      ComponentRecorder
	destinations    Destinations
	outboundTargets OutboundTargets
}

var (
	_ environments.Runtime  = (*Runtime)(nil)
	_ environments.Headroom = (*Runtime)(nil)
)

func New(parts Parts, policy Policy, logger *slog.Logger) (*Runtime, error) {
	if parts.Docker == nil || parts.Admission == nil || parts.Egress == nil || parts.Targets == nil || parts.Sources == nil || parts.Builder == nil || parts.Secrets == nil || parts.Logs == nil || logger == nil {
		return nil, errors.New("runtime requires Docker, an admission lock, egress, targets, sources, builder, secrets, logs and a logger")
	}
	if !dockerx.ValidInstance(parts.Instance) {
		return nil, errors.New("runtime requires this database's instance identity")
	}
	if policy.TmpfsBytes <= 0 || policy.PIDs <= 0 || policy.StartupTimeout <= 0 || policy.SetupTimeout <= 0 || policy.MaxPerRepo <= 0 {
		return nil, errors.New("runtime policy is incomplete")
	}
	if (parts.Destinations == nil) != (parts.Outbound == nil) {
		return nil, errors.New("outbound connections need both the approved destinations and the outbound previews")
	}
	return &Runtime{
		instance: parts.Instance, admission: parts.Admission, docker: parts.Docker, egress: parts.Egress, targets: parts.Targets, sources: parts.Sources,
		builder: parts.Builder, secrets: parts.Secrets, logs: parts.Logs, policy: policy, logger: logger, now: time.Now,
		components: parts.Components, destinations: parts.Destinations, outboundTargets: parts.Outbound,
	}, nil
}

// NewInventory returns a Runtime that can only list and remove this
// instance's Docker objects (Resources, PullRequests, RemoveResource and the
// cleanup executor), for housekeeping and its diagnostics. It cannot build.
func NewInventory(docker *client.Client, instance string, logger *slog.Logger) (*Runtime, error) {
	if docker == nil || logger == nil || !dockerx.ValidInstance(instance) {
		return nil, errors.New("inventory requires Docker, this database's instance identity and a logger")
	}
	return &Runtime{instance: instance, docker: docker, logger: logger, now: time.Now}, nil
}

// PolicyFrom adapts the worker's preview settings.
func PolicyFrom(c config.PreviewConfig) Policy {
	return Policy{
		AllowRoot: c.PreviewAllowRoot, WritableRootfs: c.PreviewWritableRootfs, TmpfsBytes: int64(c.PreviewTmpfsSize),
		PIDs: c.PreviewPIDs, OCIRuntime: c.PreviewOCIRuntime, StartupTimeout: c.PreviewStartupTimeout, SetupTimeout: c.PreviewSetupTimeout,
		MemoryBudget: int64(c.PreviewMemoryBudget), CPUBudget: c.PreviewCPUBudget, MaxPerRepo: c.PreviewMaxPerRepository, MaxPerAccount: c.PreviewMaxPerAccount,
	}
}

// Build prepares the request's exact commit, builds its image, rechecks access
// and fork identity, decrypts authorized secrets and starts a hardened
// candidate on the PR's private network. It returns once the candidate is
// healthy; it never routes it. On failure the candidate is removed.
func (r *Runtime) Build(ctx context.Context, request environments.Request, attempt environments.Attempt) (environments.Artifact, error) {
	if r.targets == nil {
		return environments.Artifact{}, failure(CodeUnavailable, "This runtime only inventories objects.", false, nil)
	}
	target, err := r.targets.Current(ctx, request)
	if err != nil {
		return environments.Artifact{}, err
	}
	buildLogs := r.openLogs(ctx, attempt, nil)
	image, settings, err := r.buildImage(ctx, request, target, buildLogs)
	r.closeLogs(ctx, buildLogs)
	if err != nil {
		return environments.Artifact{}, err
	}
	// Access and the PR's source can change while a build runs. Decide on
	// secrets with what is true now; an unbuilt candidate is never started.
	if target, err = r.targets.Current(ctx, request); err != nil {
		return environments.Artifact{}, err
	}
	env, err := r.secrets.ForContainer(ctx, target.RepoID, target.secretSource(), settings.Env)
	if err != nil {
		return environments.Artifact{}, failure(CodeSecrets, "Repository secrets could not be decrypted on the worker; the preview was not started.", false, err)
	}
	defer env.Wipe()
	id := requestIdentity{PullRequestID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, EnvironmentID: attempt.EnvironmentID, HeadSHA: request.HeadSHA}
	// The candidate's key names its app container; its services' credentials
	// are generated now so the log redacts them from the first line.
	key := fmt.Sprintf("previa-%s-pr%d-g%d-r%d-%d", r.instance, id.PullRequestID, id.Generation, id.Revision, r.now().UnixNano())
	plan, err := planBackend(key, settings)
	if err != nil {
		return environments.Artifact{}, failure(CodeUnavailable, "The preview's services could not be prepared.", true, err)
	}
	redactor, err := env.Redactor(plan.secrets...)
	if err != nil {
		return environments.Artifact{}, failure(CodeSecrets, "Repository secrets could not be prepared for log redaction.", false, err)
	}
	plan.redactEnv = append(env.SecretNames(), plan.credentialNames()...)
	startLogs := r.openLogs(ctx, attempt, redactor)
	defer r.closeLogs(ctx, startLogs)
	for _, notice := range env.Notices {
		startLogs.System(notice)
	}
	appEnv, notice := injectBackend(env.Environ(), plan.env)
	if notice != "" {
		startLogs.System(notice)
	}
	containerID, err := r.startCandidate(ctx, id, target, image, settings, appEnv, plan, startLogs)
	if err != nil {
		startLogs.System("The preview did not start: " + messageOf(err))
		return environments.Artifact{}, err
	}
	return environments.Artifact{ContainerID: containerID, ImageTag: image.Ref}, nil
}

// buildImage prepares the commit and builds it, always releasing the source.
func (r *Runtime) buildImage(ctx context.Context, request environments.Request, t Target, logs LogSink) (builds.Image, repoconfig.Config, error) {
	logs.System("Preparing commit " + request.HeadSHA[:min(12, len(request.HeadSHA))] + " from GitHub.")
	prepared, err := r.sources.Prepare(ctx, source.Request{
		InstallationID: t.InstallationID, BaseRepoID: t.BaseRepoID, CommitSHA: request.HeadSHA,
		PullRequestID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision,
	})
	if err != nil {
		if source.CodeOf(err) == source.CodeCancelled {
			return builds.Image{}, repoconfig.Config{}, r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled.", false, err))
		}
		var sourceErr *source.Error
		message, retryable := "The commit could not be prepared.", false
		if errors.As(err, &sourceErr) {
			message, retryable = sourceErr.Message, sourceErr.Retryable
		}
		logs.System("Source preparation failed: " + message)
		return builds.Image{}, repoconfig.Config{}, failure(CodeSource, message, retryable, err)
	}
	defer func() {
		if err := r.sources.Release(prepared); err != nil {
			// Workspaces.List finds it for housekeeping.
			r.logger.WarnContext(ctx, "release source workspace", "pr_id", request.PullRequestID, "error", err)
		}
	}()
	settings := prepared.Config.Config
	for _, notice := range prepared.Config.Notices {
		logs.System(notice.Message)
	}
	image, err := r.builder.Build(ctx, builds.Job{
		Instance: r.instance, PullRequestID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSHA: request.HeadSHA,
		BaseRepoID: t.BaseRepoID, HeadRepoID: t.HeadRepoID, ContextDir: prepared.ContextDir, Dockerfile: prepared.Dockerfile, Logs: logs,
	})
	if err != nil {
		var buildErr *builds.Error
		message, retryable := "The image build failed.", false
		if errors.As(err, &buildErr) {
			message, retryable = buildErr.Message, buildErr.Retryable
		}
		switch builds.CodeOf(err) {
		case builds.CodeCancelled:
			return builds.Image{}, repoconfig.Config{}, failure(CodeCancelled, message, false, err)
		case builds.CodeTimeout:
			return builds.Image{}, repoconfig.Config{}, failure(CodeTimeout, message, false, err)
		case builds.CodeSuperseded:
			return builds.Image{}, repoconfig.Config{}, failure(CodeNotCurrent, message, false, err)
		case builds.CodeDiskLow:
			return builds.Image{}, repoconfig.Config{}, failure(CodeDiskLow, message, true, err)
		}
		return builds.Image{}, repoconfig.Config{}, failure(CodeBuild, message, retryable, err)
	}
	return image, settings, nil
}

// interrupted reports cancellation or timeout instead of fallback when ctx ended.
func (r *Runtime) interrupted(ctx context.Context, fallback *Error) *Error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failure(CodeTimeout, "The build exceeded its time limit.", false, fallback)
	case ctx.Err() != nil:
		return failure(CodeCancelled, "The build was cancelled.", false, fallback)
	}
	return fallback
}

func asRuntimeError(err error, message string) error {
	var classified *Error
	if errors.As(err, &classified) {
		return err
	}
	return failure(CodeUnavailable, message, true, err)
}

func messageOf(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Message
	}
	return "an unexpected error occurred."
}

// openLogs opens the attempt's log; without one, output is discarded.
func (r *Runtime) openLogs(ctx context.Context, attempt environments.Attempt, redactor *secrets.Redactor) LogSink {
	if redactor == nil {
		redactor, _ = secrets.NewRedactor()
	}
	sink, err := r.logs(ctx, attempt, redactor)
	if err != nil || sink == nil {
		if attempt.EnvironmentID != 0 {
			r.logger.WarnContext(ctx, "build log unavailable", "environment_id", attempt.EnvironmentID, "error", err)
		}
		return discardSink{}
	}
	return sink
}

func (r *Runtime) closeLogs(ctx context.Context, sink LogSink) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := sink.Close(closeCtx); err != nil {
		r.logger.WarnContext(ctx, "build log incomplete", "error", err)
	}
}

type discardSink struct{}

func (discardSink) Write(p []byte) (int, error) { return len(p), nil }
func (discardSink) System(string)               {}
func (discardSink) Close(context.Context) error { return nil }
