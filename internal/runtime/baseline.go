package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// A repository baseline (docs/32-persistent-test-data.md) is built from one
// branch head: its image is built like a pull request's but without any
// repository secret or outbound approval, its services start on an internal
// network of their own, setup and then seed run once, and each SQL service's
// database is dumped into the baseline storage. No app starts, nothing is
// routed, and everything created is removed afterwards.

// Baseline failure categories, recorded with the refresh.
const (
	BaselineBranchNotFound = "branch_not_found"
	BaselineConfigInvalid  = "config_invalid"
	BaselineBuildFailed    = "build_failed"
	BaselineServiceFailed  = "service_failed"
	BaselineSetupFailed    = "setup_failed"
	BaselineSeedFailed     = "seed_failed"
	BaselineCaptureFailed  = "capture_failed"
	BaselineCaptureTooBig  = "capture_too_large"
	BaselineCapacityWait   = "capacity_wait"
	BaselineUnavailable    = "unavailable"
)

// ErrCaptureTooLarge is what a DumpSink returns once a dump exceeds the
// baseline size limit.
var ErrCaptureTooLarge = errors.New("baseline dump exceeds the size limit")

// BaselineJob is one baseline refresh to build and capture.
type BaselineJob struct {
	RepoID, GitHubRepoID, InstallationID, WorkspaceID int64
	RefreshID                                         int64
	Branch, CommitSHA                                 string
	// OpenLogs opens the refresh's build log with a redactor; it may be
	// called more than once, each writer continuing the same log.
	OpenLogs func(ctx context.Context, redactor *secrets.Redactor) (LogSink, error)
	// Capture returns where one service's dump is stored.
	Capture func(kind, version string) (DumpSink, error)
}

// DumpSink receives one service's dump: Commit stores it, Abort discards it.
type DumpSink interface {
	io.Writer
	Commit() (CapturedDump, error)
	Abort()
}

// CapturedDump is one stored baseline dump.
type CapturedDump struct {
	Kind, Version        string
	StorageKey, Checksum string
	Size                 int64
	KeyVersion           int32
}

// BaselineResult is what a refresh built: its configuration hash and one
// dump per SQL service.
type BaselineResult struct {
	ConfigHash string
	Dumps      []CapturedDump
}

// BaselineError is a classified refresh failure. Detail is a sentence safe
// to show on the Test data tab; Retryable means the refresh waits and tries
// again instead of failing.
type BaselineError struct {
	Code, Detail string
	Retryable    bool
	Err          error
}

func (e *BaselineError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Detail + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Detail
}

func (e *BaselineError) Unwrap() error { return e.Err }

func baselineFailure(code, detail string, retryable bool, err error) *BaselineError {
	return &BaselineError{Code: code, Detail: detail, Retryable: retryable, Err: err}
}

// RunBaseline builds and captures a repository baseline. Nothing it creates
// outlives it; a crash leaves labelled objects that the next attempt of the
// same refresh, or housekeeping, removes.
func (r *Runtime) RunBaseline(ctx context.Context, job BaselineJob) (BaselineResult, error) {
	if r.targets == nil {
		return BaselineResult{}, baselineFailure(BaselineUnavailable, "This worker only inventories objects.", false, nil)
	}
	if err := r.RemoveBaseline(ctx, job.RefreshID); err != nil {
		return BaselineResult{}, baselineFailure(BaselineUnavailable, "The leftovers of an earlier attempt could not be removed.", true, err)
	}
	empty, _ := secrets.NewRedactor()
	buildLogs := r.openBaselineLogs(ctx, job, empty)
	buildLogs.System(fmt.Sprintf("Building the test data baseline from branch %s at commit %s.", job.Branch, shortCommit(job.CommitSHA)))
	image, settings, configHash, err := r.buildBaselineImage(ctx, job, buildLogs)
	r.closeLogs(ctx, buildLogs)
	if err != nil {
		return BaselineResult{}, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := r.RemoveImage(cleanupCtx, image.Ref); err != nil {
			r.logger.WarnContext(ctx, "remove baseline image", "refresh_id", job.RefreshID, "error", err)
		}
	}()
	key := fmt.Sprintf("previa-%s-baseline%d-%d", r.instance, job.RefreshID, r.now().UnixNano())
	plan, err := planBackend(key, settings)
	if err != nil {
		return BaselineResult{}, baselineFailure(BaselineUnavailable, "The baseline's services could not be prepared.", true, err)
	}
	redactor, err := secrets.NewRedactor(plan.secrets...)
	if err != nil {
		return BaselineResult{}, baselineFailure(BaselineUnavailable, "The baseline's log redaction could not be prepared.", true, err)
	}
	logs := r.openBaselineLogs(ctx, job, redactor)
	defer r.closeLogs(ctx, logs)
	defer func() {
		if err := r.RemoveBaseline(context.WithoutCancel(ctx), job.RefreshID); err != nil {
			r.logger.WarnContext(ctx, "remove baseline containers", "refresh_id", job.RefreshID, "error", err)
		}
	}()
	b, err := r.startBaseline(ctx, job, image, settings, plan, logs)
	if err != nil {
		return BaselineResult{}, err
	}
	if len(plan.setup) > 0 {
		if err := r.runOneOff(ctx, b.setup, plan.setup, logs, "setup", CodeSetupFailed); err != nil {
			return BaselineResult{}, baselineError(err, BaselineSetupFailed)
		}
	}
	if err := r.runOneOff(ctx, b.seed, plan.seed, logs, "seed", CodeSeedFailed); err != nil {
		return BaselineResult{}, baselineError(err, BaselineSeedFailed)
	}
	result := BaselineResult{ConfigHash: configHash}
	for _, s := range plan.sqlServices() {
		dump, err := r.captureService(ctx, job, s, b.services[s.Kind], logs)
		if err != nil {
			return BaselineResult{}, err
		}
		result.Dumps = append(result.Dumps, dump)
	}
	logs.System("The baseline is captured. Its containers are removed now.")
	return result, nil
}

func (r *Runtime) openBaselineLogs(ctx context.Context, job BaselineJob, redactor *secrets.Redactor) LogSink {
	if job.OpenLogs == nil {
		return discardSink{}
	}
	sink, err := job.OpenLogs(ctx, redactor)
	if err != nil || sink == nil {
		r.logger.WarnContext(ctx, "baseline log unavailable", "refresh_id", job.RefreshID, "error", err)
		return discardSink{}
	}
	return sink
}

// buildBaselineImage prepares the branch head and builds its image, with no
// repository secret. The configuration must declare seed and a SQL service.
func (r *Runtime) buildBaselineImage(ctx context.Context, job BaselineJob, logs LogSink) (builds.Image, repoconfig.Config, string, error) {
	prepared, err := r.sources.Prepare(ctx, source.Request{InstallationID: job.InstallationID, BaseRepoID: job.GitHubRepoID, CommitSHA: job.CommitSHA, BaselineRefreshID: job.RefreshID})
	if err != nil {
		if source.CodeOf(err) == source.CodeCancelled || ctx.Err() != nil {
			return builds.Image{}, repoconfig.Config{}, "", ctx.Err()
		}
		var sourceErr *source.Error
		message, retryable := "The commit could not be prepared.", false
		if errors.As(err, &sourceErr) {
			message, retryable = sourceErr.Message, sourceErr.Retryable
		}
		logs.System("Source preparation failed: " + message)
		code := BaselineBuildFailed
		if retryable {
			code = BaselineUnavailable
		}
		return builds.Image{}, repoconfig.Config{}, "", baselineFailure(code, message, retryable, err)
	}
	defer func() {
		if err := r.sources.Release(prepared); err != nil {
			r.logger.WarnContext(ctx, "release baseline source workspace", "refresh_id", job.RefreshID, "error", err)
		}
	}()
	settings := prepared.Config.Config
	for _, notice := range prepared.Config.Notices {
		logs.System(notice.Message)
	}
	switch {
	case len(settings.Seed) == 0:
		detail := "Add a seed command to .previewrc.yaml on branch " + job.Branch + ": it creates the starting data every pull request copies."
		logs.System(detail)
		return builds.Image{}, repoconfig.Config{}, "", baselineFailure(BaselineConfigInvalid, detail, false, nil)
	case len(backend{services: planServices(settings)}.sqlServices()) == 0:
		detail := "Add postgres or mysql under services in .previewrc.yaml on branch " + job.Branch + ": only SQL databases have a baseline."
		logs.System(detail)
		return builds.Image{}, repoconfig.Config{}, "", baselineFailure(BaselineConfigInvalid, detail, false, nil)
	}
	duration := settings.BuildTimeout
	if duration <= 0 {
		duration = 12 * time.Minute
	}
	buildCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	logs.System(fmt.Sprintf("Building context %s with up to %s. Baseline builds get no repository secrets.", settings.Context, duration))
	image, err := r.builder.Build(buildCtx, builds.Job{
		Instance: r.instance, BaselineRefreshID: job.RefreshID, HeadSHA: job.CommitSHA,
		BaseRepoID: job.GitHubRepoID, HeadRepoID: job.GitHubRepoID, ContextDir: prepared.ContextDir, Dockerfile: prepared.Dockerfile, Logs: logs,
	})
	if err != nil {
		if ctx.Err() != nil {
			return builds.Image{}, repoconfig.Config{}, "", ctx.Err()
		}
		message, retryable := "The image build failed.", false
		var buildErr *builds.Error
		if errors.As(err, &buildErr) {
			message, retryable = buildErr.Message, buildErr.Retryable
		}
		code := BaselineBuildFailed
		if builds.CodeOf(err) == builds.CodeDiskLow {
			code, retryable = BaselineCapacityWait, true
		}
		return builds.Image{}, repoconfig.Config{}, "", baselineFailure(code, message, retryable, err)
	}
	return image, settings, prepared.Config.Hash, nil
}

// planServices are the configuration's services, for checks before a plan.
func planServices(settings repoconfig.Config) []serviceRun {
	runs := make([]serviceRun, len(settings.Services))
	for i, s := range settings.Services {
		runs[i] = serviceRun{BackendService: s}
	}
	return runs
}

// baselineRun is a started baseline's containers.
type baselineRun struct {
	services    map[string]string
	setup, seed string
}

// startBaseline admits the baseline's containers against the host's preview
// budget, creates them on an internal network of their own and starts its
// services. setup and seed are created, not started.
func (r *Runtime) startBaseline(ctx context.Context, job BaselineJob, image builds.Image, settings repoconfig.Config, plan backend, logs LogSink) (baselineRun, error) {
	volumes, err := r.serviceImages(ctx, plan.services, logs)
	if err != nil {
		return baselineRun{}, baselineError(err, BaselineServiceFailed)
	}
	unlock, err := r.admission.Lock(ctx)
	if err != nil {
		return baselineRun{}, baselineFailure(BaselineUnavailable, "Preview capacity could not be checked.", true, err)
	}
	b, err := r.createBaseline(ctx, job, image, settings, plan, volumes)
	unlock()
	if err != nil {
		return baselineRun{}, err
	}
	for _, s := range plan.services {
		if _, err := r.docker.ContainerStart(ctx, b.services[s.Kind], client.ContainerStartOptions{}); err != nil {
			return baselineRun{}, baselineFailure(BaselineServiceFailed, fmt.Sprintf("%s %s could not be started.", s.profile.title, s.Version), true, err)
		}
	}
	if err := r.waitServicesReady(ctx, plan.services, b.services, logs); err != nil {
		return baselineRun{}, baselineError(err, BaselineServiceFailed)
	}
	return b, nil
}

// createBaseline runs under the admission lock: the host budget must hold the
// baseline's services, setup and seed, which are created (and so counted)
// before the lock is released.
func (r *Runtime) createBaseline(ctx context.Context, job BaselineJob, image builds.Image, settings repoconfig.Config, plan backend, volumes map[string][]string) (baselineRun, error) {
	app := settings.Resources
	var cpuMillis, memory int64
	for _, s := range plan.services {
		cpuMillis, memory = cpuMillis+s.profile.cpuMillis, memory+s.profile.memory
	}
	cpuMillis, memory = cpuMillis+2*app.CPUMillis, memory+2*app.MemoryBytes // setup and seed
	if err := r.checkHostCapacity(ctx, cpuMillis, memory); err != nil {
		return baselineRun{}, baselineError(err, BaselineCapacityWait)
	}
	networkID, err := r.baselineNetwork(ctx, job)
	if err != nil {
		return baselineRun{}, baselineFailure(BaselineUnavailable, "The baseline network could not be prepared.", true, err)
	}
	inspected, err := r.docker.ImageInspect(ctx, image.ID)
	if err != nil || inspected.Config == nil {
		return baselineRun{}, baselineFailure(BaselineUnavailable, "The built image could not be inspected.", true, err)
	}
	b := baselineRun{services: map[string]string{}}
	create := func(name string, config *container.Config, host *container.HostConfig, networking *network.NetworkingConfig) (string, error) {
		created, err := r.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: config, HostConfig: host, NetworkingConfig: networking})
		if err != nil {
			return "", baselineFailure(BaselineUnavailable, "A container of the baseline could not be created.", true, err)
		}
		return created.ID, nil
	}
	for _, s := range plan.services {
		c, h, n := serviceSpec(s, volumes[s.Kind], networkID, r.baselineLabels(job, dockerx.KindService, s.Kind, s.profile.cpuMillis, s.profile.memory, s.Version), r.policy)
		if b.services[s.Kind], err = create(plan.key+"-"+s.Kind, c, h, n); err != nil {
			return baselineRun{}, err
		}
	}
	env, _ := injectBackend(settings.Env, plan.env) // YAML and connection settings only: no repository secret
	appConfig, appHost, _ := containerSpec(candidate{
		Name: plan.key, ImageID: image.ID, ImageUser: inspected.Config.User, Port: settings.Port, Env: env,
		CPUMillis: app.CPUMillis, Memory: app.MemoryBytes, NetworkID: networkID, Labels: map[string]string{},
	}, r.policy)
	networking := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{networkID: {}}}
	for _, command := range []struct {
		name string
		argv []string
		id   *string
	}{{"setup", plan.setup, &b.setup}, {"seed", plan.seed, &b.seed}} {
		if len(command.argv) == 0 {
			continue
		}
		c, h, n := commandSpec(appConfig, appHost, networking, command.argv, command.name, container.RestartPolicy{Name: container.RestartPolicyDisabled},
			r.baselineLabels(job, dockerx.KindSetup, command.name, app.CPUMillis, app.MemoryBytes, ""), app.CPUMillis, app.MemoryBytes)
		if *command.id, err = create(plan.key+"-"+command.name, c, h, n); err != nil {
			return baselineRun{}, err
		}
	}
	return b, nil
}

// baselineLabels identify a baseline's container: its refresh instead of a
// pull request, and the limits capacity accounting reads.
func (r *Runtime) baselineLabels(job BaselineJob, kind, component string, cpuMillis, memory int64, version string) map[string]string {
	labels := map[string]string{
		dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: kind,
		dockerx.LabelBaseline: strconv.FormatInt(job.RefreshID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(job.GitHubRepoID, 10),
		dockerx.LabelSHA: job.CommitSHA, dockerx.LabelComponent: component,
		dockerx.LabelCPUMillis: strconv.FormatInt(cpuMillis, 10), dockerx.LabelMemory: strconv.FormatInt(memory, 10),
		dockerx.LabelStarted: r.now().UTC().Format(time.RFC3339),
	}
	if version != "" {
		labels[dockerx.LabelVersion] = version
	}
	if job.WorkspaceID > 0 {
		labels[dockerx.LabelWorkspace] = strconv.FormatInt(job.WorkspaceID, 10)
	}
	return labels
}

// baselineNetwork creates the refresh's internal network: no egress proxy, no
// router, no other preview.
func (r *Runtime) baselineNetwork(ctx context.Context, job BaselineJob) (string, error) {
	created, err := r.docker.NetworkCreate(ctx, fmt.Sprintf("previa-%s-baseline%d", r.instance, job.RefreshID), client.NetworkCreateOptions{
		Driver: "bridge", Internal: true, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: dockerx.KindBaselineNetwork,
			dockerx.LabelBaseline: strconv.FormatInt(job.RefreshID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(job.GitHubRepoID, 10),
		}})
	if err != nil {
		return "", fmt.Errorf("create baseline network: %w", err)
	}
	return created.ID, nil
}

// checkHostCapacity refuses work that would exceed the host's preview budget.
func (r *Runtime) checkHostCapacity(ctx context.Context, cpuMillis, memory int64) error {
	b, err := r.budget(ctx)
	if err != nil {
		return failure(CodeUnavailable, "Preview capacity could not be checked.", true, err)
	}
	u, err := r.usage(ctx, Target{})
	if err != nil {
		return failure(CodeUnavailable, "Preview capacity could not be checked.", true, err)
	}
	switch {
	case u.memory+memory > b.memory:
		return failure(CodeCapacity, fmt.Sprintf("The baseline would need %s of memory beside the previews; the host allows %s.", humanBytes(u.memory+memory), humanBytes(b.memory)), true, nil)
	case u.cpuMillis+cpuMillis > b.cpuMillis:
		return failure(CodeCapacity, fmt.Sprintf("The baseline would need %.2f CPUs beside the previews; the host allows %.2f.", float64(u.cpuMillis+cpuMillis)/1000, float64(b.cpuMillis)/1000), true, nil)
	}
	return nil
}

// captureService streams one service's dump into the baseline storage.
func (r *Runtime) captureService(ctx context.Context, job BaselineJob, s serviceRun, containerID string, logs LogSink) (CapturedDump, error) {
	title := s.profile.title + " " + s.Version
	logs.System("Capturing the " + title + " database.")
	sink, err := job.Capture(s.Kind, s.Version)
	if err != nil {
		return CapturedDump{}, baselineFailure(BaselineUnavailable, "The baseline storage is not available on this worker.", true, err)
	}
	captureCtx, cancel := context.WithTimeout(ctx, r.copyTimeout())
	defer cancel()
	started := r.now()
	out, err := dockerx.StreamOutput(captureCtx, r.docker, containerID, dumpCommand(s.Kind)...)
	if err != nil {
		sink.Abort()
		return CapturedDump{}, baselineFailure(BaselineCaptureFailed, "The "+title+" database could not be read.", true, err)
	}
	_, copyErr := io.Copy(sink, out)
	dumped, waitErr := out.Wait(captureCtx)
	switch {
	case ctx.Err() != nil:
		sink.Abort()
		return CapturedDump{}, ctx.Err()
	case errors.Is(copyErr, ErrCaptureTooLarge):
		sink.Abort()
		detail := "The " + title + " baseline exceeds the size limit (PREVIEW_BASELINE_MAX_SIZE); seed less data."
		logs.System(detail)
		return CapturedDump{}, baselineFailure(BaselineCaptureTooBig, detail, false, copyErr)
	case copyErr != nil || waitErr != nil || dumped.ExitCode != 0 || captureCtx.Err() != nil:
		sink.Abort()
		logs.System("The " + title + " database could not be captured.")
		return CapturedDump{}, baselineFailure(BaselineCaptureFailed, "The "+title+" database could not be captured.", true, errors.Join(copyErr, waitErr))
	}
	dump, err := sink.Commit()
	if err != nil {
		if errors.Is(err, ErrCaptureTooLarge) {
			return CapturedDump{}, baselineFailure(BaselineCaptureTooBig, "The "+title+" baseline exceeds the size limit (PREVIEW_BASELINE_MAX_SIZE); seed less data.", false, err)
		}
		return CapturedDump{}, baselineFailure(BaselineCaptureFailed, "The "+title+" baseline could not be stored.", true, err)
	}
	dump.Kind, dump.Version = s.Kind, s.Version
	logs.System(fmt.Sprintf("Captured %s of %s, encrypted, in %s.", bytesText(dump.Size), title, r.now().Sub(started).Round(100*time.Millisecond)))
	return dump, nil
}

// baselineError turns a runtime failure into a baseline failure category;
// fallback is used for failures specific to the step.
func baselineError(err error, fallback string) error {
	var classified *Error
	if !errors.As(err, &classified) {
		return err
	}
	switch classified.Code {
	case CodeCancelled, CodeTimeout:
		return err
	case CodeCapacity:
		return baselineFailure(BaselineCapacityWait, classified.Message, true, err)
	case CodeServiceUnavailable, CodeServiceFailed:
		return baselineFailure(BaselineServiceFailed, classified.Message, classified.Retryable, err)
	case CodeSetupFailed:
		return baselineFailure(BaselineSetupFailed, classified.Message, false, err)
	case CodeSeedFailed:
		return baselineFailure(BaselineSeedFailed, classified.Message, false, err)
	case CodeUnavailable:
		return baselineFailure(BaselineUnavailable, classified.Message, true, err)
	}
	return baselineFailure(fallback, classified.Message, classified.Retryable, err)
}

// RemoveBaseline removes every container and the network a baseline refresh
// created, verifying their labels. Missing objects count as removed.
func (r *Runtime) RemoveBaseline(ctx context.Context, refreshID int64) error {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance, dockerx.LabelBaseline+"="+strconv.FormatInt(refreshID, 10))
	listed, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return fmt.Errorf("list baseline containers: %w", err)
	}
	var errs []error
	for _, c := range listed.Items {
		if err := r.RemoveContainer(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
	}
	networks, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter.Add("label", dockerx.LabelKind+"="+dockerx.KindBaselineNetwork)})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list baseline networks: %w", err))...)
	}
	for _, n := range networks.Items {
		if _, err := r.docker.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove baseline network: %w", err))
		}
	}
	return errors.Join(errs...)
}

// BaselineObject is something a baseline refresh left in Docker.
type BaselineObject struct {
	RefreshID int64
	Created   time.Time
}

// BaselineRefreshes lists the refreshes this instance's Docker objects belong
// to, with their newest object's creation time, for housekeeping.
func (r *Runtime) BaselineRefreshes(ctx context.Context) (map[int64]time.Time, error) {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance, dockerx.LabelBaseline)
	found := map[int64]time.Time{}
	note := func(labels map[string]string, created time.Time) {
		id, err := strconv.ParseInt(labels[dockerx.LabelBaseline], 10, 64)
		if err != nil || id <= 0 {
			return
		}
		if created.After(found[id]) {
			found[id] = created
		}
	}
	containers, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list baseline containers: %w", err)
	}
	for _, c := range containers.Items {
		note(c.Labels, time.Unix(c.Created, 0))
	}
	images, err := r.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list baseline images: %w", err)
	}
	for _, i := range images.Items {
		note(i.Labels, time.Unix(i.Created, 0))
	}
	networks, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list baseline networks: %w", err)
	}
	for _, n := range networks.Items {
		note(n.Labels, n.Created)
	}
	return found, nil
}

// RemoveBaselineImages removes the images a refresh built.
func (r *Runtime) RemoveBaselineImages(ctx context.Context, refreshID int64) error {
	listed, err := r.docker.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.Add("label",
		dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance, dockerx.LabelBaseline+"="+strconv.FormatInt(refreshID, 10))})
	if err != nil {
		return fmt.Errorf("list baseline images: %w", err)
	}
	var errs []error
	for _, i := range listed.Items {
		if err := r.RemoveImage(ctx, i.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
