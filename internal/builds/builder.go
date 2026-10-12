package builds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	buildkit "github.com/moby/buildkit/client"
	"github.com/moby/moby/client"
	"github.com/tonistiigi/fsutil"
)

// Config bounds builds. Values come from config.BuilderConfig, except
// Instance: this database's identity (database.Instance), which names and
// labels everything the builder creates.
type Config struct {
	Instance      string
	BuildKitImage string
	CPUs          float64
	Memory        int64
	PIDs          int64
	DiskBudget    int64
	MaxImageSize  int64
	MinFreeDisk   int64
	// MaxDuration is the longest a build may run (BUILD_TIMEOUT). A builder
	// holding a cache longer than this belongs to a crashed worker.
	MaxDuration time.Duration
	// EgressHosts are the only hosts a build may reach; empty allows any
	// public host (BUILD_EGRESS).
	EgressHosts []string
}

// ConfigFrom adapts the worker's builder settings.
func ConfigFrom(c config.BuilderConfig, instance string, maxDuration time.Duration) Config {
	return Config{
		Instance: instance, BuildKitImage: c.BuildKitImage, CPUs: c.BuildCPUs,
		Memory: int64(c.BuildMemory), PIDs: c.BuildPIDs, DiskBudget: int64(c.BuildDiskBudget),
		MaxImageSize: int64(c.BuildMaxImageSize), MinFreeDisk: int64(c.BuildMinFreeDisk), MaxDuration: maxDuration,
		EgressHosts: egressHosts(c.BuildEgress, c.BuildEgressHosts),
	}
}

// Builder turns a prepared exact-commit source into a uniquely tagged Docker
// image inside a fresh BuildKit container on a private network. It never starts
// the image and never receives repository secrets or Previa credentials.
type Builder struct {
	docker *client.Client
	egress *egress.Gateway
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

func NewBuilder(docker *client.Client, gateway *egress.Gateway, cfg Config, logger *slog.Logger) (*Builder, error) {
	if docker == nil || gateway == nil || logger == nil {
		return nil, errors.New("builder requires a Docker client, egress gateway and logger")
	}
	if cfg.BuildKitImage == "" || cfg.CPUs <= 0 || cfg.Memory <= 0 || cfg.PIDs <= 0 || cfg.DiskBudget <= 0 || cfg.MaxImageSize <= 0 || cfg.MinFreeDisk < 0 {
		return nil, errors.New("builder limits are incomplete")
	}
	if !dockerx.ValidInstance(cfg.Instance) {
		return nil, errors.New("builder requires this database's instance identity")
	}
	return &Builder{docker: docker, egress: gateway, cfg: cfg, logger: logger, now: time.Now}, nil
}

// Job is one admitted build attempt.
type Job struct {
	// Instance is this database's identity; it must match the builder's.
	Instance                            string
	PullRequestID, Generation, Revision int64
	HeadSHA                             string
	// GitHub repository IDs. HeadRepoID is 0 when the head repository is
	// unknown (for example deleted); such a build gets a cache of its own.
	BaseRepoID, HeadRepoID int64
	// From source.Prepared: the extracted tree and the Dockerfile inside it.
	ContextDir, Dockerfile string
	Logs                   LogSink
	// Secrets are the values Dockerfile steps may read with
	// RUN --mount=type=secret,id=NAME; Registries log base image pulls in.
	// Both are empty for untrusted sources. Never logged.
	Secrets    map[string][]byte
	Registries []RegistryLogin
}

// RegistryLogin is one registry a build may pull private base images from.
type RegistryLogin struct {
	Host, Username, Secret string
}

// Image is a built image, ready for a candidate container.
type Image struct {
	Ref, ID string
	Size    int64
	Reused  bool // an earlier attempt for the same revision built it
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (j Job) validate() error {
	switch {
	case !dockerx.ValidInstance(j.Instance):
		return errors.New("a build needs this database's instance identity")
	case j.PullRequestID <= 0 || j.Generation < 0 || j.Revision <= 0:
		return errors.New("a build needs a PR, generation and revision")
	case !commitSHA.MatchString(j.HeadSHA):
		return errors.New("a build needs a full lowercase commit SHA")
	case j.BaseRepoID <= 0 || j.HeadRepoID < 0:
		return errors.New("a build needs its repository IDs")
	case !filepath.IsAbs(j.ContextDir):
		return errors.New("the build context must be an absolute path")
	case j.Dockerfile == "" || !filepath.IsLocal(j.Dockerfile):
		return errors.New("the Dockerfile must be inside the build context")
	case j.Logs == nil:
		return errors.New("a build needs a log sink")
	}
	return nil
}

var (
	errDiskBudget        = errors.New("disk budget exceeded")
	errSupersededAttempt = errors.New("a newer attempt took over")
)

const diskPoll = 5 * time.Second

// Build produces the job's image. It reuses an image an earlier attempt
// finished for the same revision; otherwise it builds in a fresh sandbox that is
// always removed afterwards. The caller releases the source workspace.
func (b *Builder) Build(ctx context.Context, job Job) (Image, error) {
	if err := job.validate(); err != nil {
		return Image{}, failure(CodeInvalidJob, "The build request is incomplete.", false, err)
	}
	if job.Instance != b.cfg.Instance {
		return Image{}, failure(CodeInvalidJob, "The build request belongs to another Previa instance.", false, nil)
	}
	if existing, ok := b.reusable(ctx, job); ok {
		job.Logs.System("Reusing the image an earlier attempt built for this revision.")
		return existing, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if s, ok := job.Logs.(supersedable); ok {
		go func() {
			select {
			case <-s.Done():
				cancel(errSupersededAttempt)
			case <-ctx.Done():
			}
		}()
	}
	job.Logs.System("Starting an isolated builder for commit " + job.HeadSHA[:12] + ".")
	sb, err := b.openSandbox(ctx, job)
	if err != nil {
		return Image{}, b.interrupted(ctx, err, failure(CodeBuilderUnavailable, "The isolated builder could not be started.", true, err))
	}
	defer func() {
		if err := sb.close(ctx); err != nil {
			// Leftovers carry labels; Resources and housekeeping find them.
			b.logger.WarnContext(ctx, "build sandbox cleanup failed", "pr_id", job.PullRequestID, "error", err)
		}
	}()
	if !sb.cached {
		job.Logs.System("Another build of this source is using its cache, so this build runs without it.")
	}
	free, err := sb.freeDisk(ctx)
	if err != nil {
		return Image{}, b.interrupted(ctx, err, failure(CodeBuilderUnavailable, "The builder could not measure free disk space.", true, err))
	}
	metrics.BuildDiskFree(free)
	// Every other running build may still grow to its disk budget; count that
	// as already used. Builders exist before they measure, so two builds
	// starting together each count the other.
	others, err := b.otherBuilders(ctx, sb.containerID)
	if err != nil {
		return Image{}, b.interrupted(ctx, err, failure(CodeBuilderUnavailable, "The builder could not count running builds.", true, err))
	}
	if needed := b.cfg.MinFreeDisk + int64(others)*b.cfg.DiskBudget; free < needed {
		return Image{}, failure(CodeDiskLow, fmt.Sprintf("The build host has %s free; with %d other builds running, a build needs at least %s.", humanBytes(free), others, humanBytes(needed)), true, nil)
	}
	bk, err := buildkit.New(ctx, "", buildkit.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return dialBuilder(ctx, b.docker, sb.containerID)
	}))
	if err != nil {
		return Image{}, failure(CodeBuilderUnavailable, "The builder could not be reached.", true, err)
	}
	defer func() { _ = bk.Close() }()
	image, err := b.solve(ctx, cancel, bk, sb, job)
	if held := b.endMetering(ctx, sb, job); !held && err == nil {
		b.removePartialImage(ctx, job)
		return Image{}, failure(CodeBuilderUnavailable, "Previa could not confirm that the build reached only package registries, so its image was discarded.", true, nil)
	}
	return image, err
}

// otherBuilders counts this instance's running builders other than self.
func (b *Builder) otherBuilders(ctx context.Context, self string) (int, error) {
	listed, err := b.docker.ContainerList(ctx, client.ContainerListOptions{Filters: client.Filters{}.Add("label",
		dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+b.cfg.Instance, dockerx.LabelKind+"="+dockerx.KindBuilder)})
	if err != nil {
		return 0, fmt.Errorf("list running builders: %w", err)
	}
	n := 0
	for _, c := range listed.Items {
		if c.ID != self {
			n++
		}
	}
	return n, nil
}

func (b *Builder) solve(ctx context.Context, cancel context.CancelCauseFunc, bk *buildkit.Client, sb *sandbox, job Job) (Image, error) {
	source, err := fsutil.NewFS(job.ContextDir)
	if err != nil {
		return Image{}, failure(CodeInvalidJob, "The build context cannot be read.", false, err)
	}
	attrs := map[string]string{"filename": filepath.ToSlash(job.Dockerfile)}
	// Proxy settings reach build steps as predefined build arguments: BuildKit
	// keeps them out of the image configuration and the layer cache keys.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		attrs["build-arg:"+name] = sb.proxyURL
	}
	for key, value := range job.identity(dockerx.KindImage) {
		attrs["label:"+key] = value
	}
	loader := newImageLoader(ctx, b.docker, b.cfg.MaxImageSize)
	status := make(chan *buildkit.SolveStatus)
	progressed := make(chan struct{})
	go func() {
		defer close(progressed)
		streamProgress(ctx, status, job.Logs)
	}()
	go b.watchDisk(ctx, cancel, bk)
	_, solveErr := bk.Solve(ctx, nil, buildkit.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{"context": source, "dockerfile": source},
		Exports:       []buildkit.ExportEntry{{Type: buildkit.ExporterDocker, Attrs: map[string]string{"name": job.ImageRef()}, Output: loader.output}},
		Session:       sessionAttachables(job),
	}, status)
	<-progressed
	loadErr := loader.wait(ctx)
	if err := errors.Join(solveErr, loadErr); err != nil {
		b.removePartialImage(ctx, job)
		return Image{}, b.buildFailure(ctx, sb, job, err)
	}
	inspected, err := b.docker.ImageInspect(ctx, job.ImageRef())
	if err != nil {
		return Image{}, b.interrupted(ctx, err, failure(CodeBuilderUnavailable, "The built image could not be found in Docker.", true, err))
	}
	if inspected.Size > b.cfg.MaxImageSize {
		b.removePartialImage(ctx, job)
		job.Logs.System(fmt.Sprintf("The image is %s, above the %s limit, and was removed.", humanBytes(inspected.Size), humanBytes(b.cfg.MaxImageSize)))
		return Image{}, failure(CodeImageTooLarge, "The image is larger than the allowed size.", false, nil)
	}
	job.Logs.System(fmt.Sprintf("Built image %s (%s).", job.ImageRef(), humanBytes(inspected.Size)))
	return Image{Ref: job.ImageRef(), ID: inspected.ID, Size: inspected.Size}, nil
}

// watchDisk stops the build once its builder stores more than the budget.
func (b *Builder) watchDisk(ctx context.Context, cancel context.CancelCauseFunc, bk *buildkit.Client) {
	ticker := time.NewTicker(diskPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		usage, err := bk.DiskUsage(ctx)
		if err != nil {
			continue
		}
		var total int64
		for _, u := range usage {
			total += u.Size
		}
		if total > b.cfg.DiskBudget {
			cancel(errDiskBudget)
			return
		}
	}
}

func (b *Builder) removePartialImage(ctx context.Context, job Job) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := b.Remove(cleanupCtx, Artifact{Kind: dockerx.KindImage, ID: job.ImageRef()}); err != nil {
		b.logger.WarnContext(ctx, "remove partial build image failed", "pr_id", job.PullRequestID, "error", err)
	}
}

// interrupted reports why ctx ended, if it did, instead of fallback.
func (b *Builder) interrupted(ctx context.Context, err error, fallback *Error) *Error {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errSupersededAttempt):
		return failure(CodeSuperseded, "A newer attempt replaced this build.", false, err)
	case errors.Is(cause, errDiskBudget):
		return failure(CodeDiskBudget, fmt.Sprintf("The build stored more than %s and was stopped.", humanBytes(b.cfg.DiskBudget)), false, err)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failure(CodeTimeout, "The build exceeded its time limit.", false, err)
	case ctx.Err() != nil:
		return failure(CodeCancelled, "The build was cancelled.", false, err)
	}
	return fallback
}

// buildFailure classifies a failed solve and states the reason in the log.
func (b *Builder) buildFailure(ctx context.Context, sb *sandbox, job Job, err error) *Error {
	if f := b.interrupted(ctx, err, nil); f != nil {
		job.Logs.System(f.Message)
		return f
	}
	if errors.Is(err, errImageTooLarge) {
		message := fmt.Sprintf("The exported image is larger than the %s limit.", humanBytes(b.cfg.MaxImageSize))
		job.Logs.System(message)
		return failure(CodeImageTooLarge, message, false, err)
	}
	summary := summarize(err)
	if strings.Contains(summary, "exit code: 137") || sb.oomKilled(ctx) {
		summary = fmt.Sprintf("A build step was stopped for exceeding the %s memory limit.", humanBytes(b.cfg.Memory))
	}
	job.Logs.System("Build failed: " + summary)
	return failure(CodeBuildFailed, "The build failed. The build log shows the failing step.", false, err)
}

// summarize shortens a BuildKit error to its first useful line.
func summarize(err error) string {
	text := strings.TrimSpace(err.Error())
	if first, _, found := strings.Cut(text, "\n"); found {
		text = first
	}
	text = strings.TrimPrefix(text, "rpc error: code = Unknown desc = ")
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}

func (s *sandbox) oomKilled(ctx context.Context) bool {
	inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	inspected, err := s.docker.ContainerInspect(inspectCtx, s.containerID, client.ContainerInspectOptions{})
	return err == nil && inspected.Container.State != nil && inspected.Container.State.OOMKilled
}

// staleAfter is how long a cache-holding builder may run before a new build
// treats it as abandoned.
func (b *Builder) staleAfter() time.Duration {
	if b.cfg.MaxDuration <= 0 {
		return staleBuilder
	}
	return b.cfg.MaxDuration + 15*time.Minute
}

// humanBytes renders a size with one decimal in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "B"
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, s
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
