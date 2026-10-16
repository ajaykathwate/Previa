package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// processSettle is how long processes run before they are first checked, so
// one that exits at once is caught before the app starts.
const processSettle = 2 * time.Second

// startCandidate admits, creates and starts the candidate and waits until it
// is ready, in order: its managed services answer their protocol, its setup
// command succeeds, its processes run, and its app passes the readiness
// check. Any failure removes every container of the candidate after
// capturing the failing part's output.
func (r *Runtime) startCandidate(ctx context.Context, id requestIdentity, t Target, image builds.Image, settings repoconfig.Config, env []string, plan backend, logs LogSink) (containerID string, err error) {
	// Pinned service images are pulled first: a pull can take minutes and
	// must not hold the admission lock.
	volumes, err := r.serviceImages(ctx, plan.services, logs)
	if err != nil {
		return "", err
	}
	// Hold the admission lock from the capacity check until every labelled
	// container of the candidate exists, so the next admission counts them.
	unlock, err := r.admission.Lock(ctx)
	if err != nil {
		return "", r.interrupted(ctx, failure(CodeUnavailable, "Preview capacity could not be checked.", true, err))
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	l, err := r.admitCandidate(ctx, id, t, image, settings, env, plan, logs)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			l.discard(ctx) // still under the admission lock if creation failed
		}
	}()
	if err := l.createContainers(ctx, volumes); err != nil {
		return "", err
	}
	unlock()
	locked = false
	if err := l.start(ctx); err != nil {
		return "", err
	}
	r.captureOutput(ctx, l.created[""], logs)
	logs.System("The preview is healthy. It is not routed yet.")
	return l.created[""], nil
}

// launch is one candidate being started: the specification its containers
// derive from, the containers created so far and, when a step fails, which
// component's output explains the failure.
type launch struct {
	r        *Runtime
	id       requestIdentity
	plan     backend
	settings repoconfig.Config
	logs     LogSink

	app        *container.Config // the app's hardened specification
	appHost    *container.HostConfig
	networking *network.NetworkingConfig // both networks; nil without services
	labels     map[string]string
	services   string // the services network's ID, if any
	outbound   int    // approved outbound connections
	withheld   bool   // approvals withheld from an untrusted source

	created   map[string]string // component -> container ID ("" is the app)
	processes map[string]string // process name -> container ID
	failed    string            // the component whose output explains a failure
}

// admitCandidate checks capacity and prepares what the candidate's
// containers need: its networks, its approved outbound connections and the
// app's hardened specification. It runs under the admission lock.
func (r *Runtime) admitCandidate(ctx context.Context, id requestIdentity, t Target, image builds.Image, settings repoconfig.Config, env []string, plan backend, logs LogSink) (*launch, error) {
	cpuMillis, memory := plan.resources(settings)
	if err := r.checkCapacity(ctx, t, cpuMillis, memory); err != nil {
		return nil, err
	}
	networkID, egressAddr, err := r.previewNetwork(ctx, id, t)
	if err != nil {
		return nil, asRuntimeError(err, "The preview network could not be prepared.")
	}
	l := &launch{r: r, id: id, plan: plan, settings: settings, logs: logs, created: map[string]string{}, processes: map[string]string{}}
	if len(plan.services) > 0 {
		if l.services, err = r.servicesNetwork(ctx, id, t); err != nil {
			return nil, asRuntimeError(err, "The services network could not be prepared.")
		}
		l.networking = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{networkID: {}, l.services: {}}}
	}
	destinations, withheld, err := r.outbound(ctx, t, egressAddr)
	if err != nil {
		return nil, err
	}
	l.outbound, l.withheld = len(destinations), withheld
	inspected, err := r.docker.ImageInspect(ctx, image.ID)
	if err != nil || inspected.Config == nil {
		return nil, failure(CodeUnavailable, "The built image could not be inspected.", true, err)
	}
	l.labels = previewLabels(r.instance, t, id, image.Ref, settings.Port, settings.HealthCheckPath, settings.Resources.CPUMillis, settings.Resources.MemoryBytes, r.now())
	l.labels[dockerx.LabelCandidate] = plan.key
	l.labels[dockerx.LabelRedactEnv] = strings.Join(plan.redactEnv, ",")
	hosts := make([]string, len(destinations))
	for i, d := range destinations {
		hosts[i] = d.Host + ":" + egressAddr.String()
	}
	var replaced bool
	l.app, l.appHost, replaced = containerSpec(candidate{
		Name: plan.key, ImageID: image.ID, ImageUser: inspected.Config.User, Port: settings.Port, Env: env, ProxyURL: egress.ProxyURL(egressAddr),
		CPUMillis: settings.Resources.CPUMillis, Memory: settings.Resources.MemoryBytes, NetworkID: networkID, ExtraHosts: hosts, Labels: l.labels,
	}, r.policy)
	if replaced {
		logs.System("The image runs as root, so the preview runs as the unprivileged user 65532 instead.")
	}
	switch {
	case len(destinations) > 0:
		logs.System(fmt.Sprintf("%d approved outbound connection%s reach this preview through the egress proxy.", len(destinations), plural(len(destinations))))
	case withheld:
		logs.System("Approved outbound connections were withheld: the pull request comes from a fork or its source could not be verified.")
	}
	return l, nil
}

// createContainers creates, labels and so counts every container of the
// candidate; none starts yet.
func (l *launch) createContainers(ctx context.Context, volumes map[string][]string) error {
	app := l.settings.Resources
	for _, s := range l.plan.services {
		labels := componentLabels(l.labels, dockerx.KindService, s.Kind, s.profile.cpuMillis, s.profile.memory)
		labels[dockerx.LabelVersion] = s.Version
		c, h, n := serviceSpec(s, volumes[s.Kind], l.services, labels, l.r.policy)
		if err := l.create(ctx, s.Kind, l.plan.key+"-"+s.Kind, c, h, n); err != nil {
			return err
		}
	}
	if len(l.plan.setup) > 0 {
		c, h, n := commandSpec(l.app, l.appHost, l.networking, l.plan.setup, "setup", container.RestartPolicy{Name: container.RestartPolicyDisabled},
			componentLabels(l.labels, dockerx.KindSetup, "setup", app.CPUMillis, app.MemoryBytes), app.CPUMillis, app.MemoryBytes)
		if err := l.create(ctx, "setup", l.plan.key+"-setup", c, h, n); err != nil {
			return err
		}
	}
	for _, p := range l.plan.processes {
		c, h, n := commandSpec(l.app, l.appHost, l.networking, p.Command, "process-"+p.Name, container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: maxRestarts},
			componentLabels(l.labels, dockerx.KindProcess, p.Name, p.Resources.CPUMillis, p.Resources.MemoryBytes), p.Resources.CPUMillis, p.Resources.MemoryBytes)
		if err := l.create(ctx, "process:"+p.Name, l.plan.key+"-"+p.Name, c, h, n); err != nil {
			return err
		}
		l.processes[p.Name] = l.created["process:"+p.Name]
	}
	return l.create(ctx, "", l.plan.key, l.app, l.appHost, l.networking)
}

func (l *launch) create(ctx context.Context, component, name string, config *container.Config, host *container.HostConfig, networking *network.NetworkingConfig) error {
	result, err := l.r.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: config, HostConfig: host, NetworkingConfig: networking})
	if err != nil {
		return l.r.interrupted(ctx, failure(CodeUnavailable, "A container of the preview could not be created.", true, err))
	}
	l.created[component] = result.ID
	return nil
}

// start brings the created candidate up in order, then confirms that
// everything the app depends on is still up and records its components.
func (l *launch) start(ctx context.Context) error {
	for _, step := range []func(context.Context) error{
		l.startServices,       // 1. services answer their protocol
		l.setUp,               // 2. setup runs once
		l.startProcesses,      // 3. processes run
		l.startApp,            // 4. the app passes its readiness check
		l.confirmDependencies, // services and processes are still up
		l.recordComponents,
	} {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (l *launch) startServices(ctx context.Context) error {
	if len(l.plan.services) == 0 {
		return nil
	}
	services := map[string]string{}
	for _, s := range l.plan.services {
		if _, err := l.r.docker.ContainerStart(ctx, l.created[s.Kind], client.ContainerStartOptions{}); err != nil {
			l.failed = s.Kind
			return l.r.interrupted(ctx, failure(CodeServiceFailed, fmt.Sprintf("%s %s could not be started.", s.profile.title, s.Version), true, err))
		}
		services[s.Kind] = l.created[s.Kind]
	}
	if err := l.r.waitServicesReady(ctx, l.plan.services, services, l.logs); err != nil {
		l.failed = l.failedService(ctx, services)
		return err
	}
	return nil
}

// setUp runs the setup command once; its container is gone afterwards.
func (l *launch) setUp(ctx context.Context) error {
	if len(l.plan.setup) == 0 {
		return nil
	}
	if err := l.r.runSetup(ctx, l.created["setup"], l.plan.setup, l.logs); err != nil {
		return err
	}
	delete(l.created, "setup")
	return nil
}

func (l *launch) startProcesses(ctx context.Context) error {
	if len(l.processes) == 0 {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(l.processes)) {
		l.logs.System("Starting the process " + name + ".")
		if _, err := l.r.docker.ContainerStart(ctx, l.processes[name], client.ContainerStartOptions{}); err != nil {
			l.failed = "process:" + name
			return l.r.interrupted(ctx, failure(CodeProcessFailed, "The process "+name+" could not be started.", true, err))
		}
	}
	select {
	case <-ctx.Done():
		return l.r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled.", false, ctx.Err()))
	case <-time.After(processSettle):
	}
	return l.checkProcesses(ctx)
}

func (l *launch) startApp(ctx context.Context) error {
	if _, err := l.r.docker.ContainerStart(ctx, l.created[""], client.ContainerStartOptions{}); err != nil {
		return l.r.interrupted(ctx, failure(CodeUnavailable, "The preview container could not be started.", true, err))
	}
	check := "for TCP connections"
	if l.settings.HealthCheckPath != "" {
		check = "for HTTP 200-399 from " + l.settings.HealthCheckPath
	}
	l.logs.System(fmt.Sprintf("Waiting up to %s for the app on port %d %s.", l.r.policy.StartupTimeout, l.settings.Port, check))
	return l.r.waitReady(ctx, l.created[""], l.r.networkName(l.id.PullRequestID), l.settings.Port, l.settings.HealthCheckPath)
}

// confirmDependencies requires everything the app depends on to still be up
// once it is ready.
func (l *launch) confirmDependencies(ctx context.Context) error {
	if err := l.checkProcesses(ctx); err != nil {
		return err
	}
	for _, s := range l.plan.services {
		if ok, _, checkErr := l.r.serviceAnswers(ctx, s, l.created[s.Kind], true); checkErr != nil || !ok {
			l.failed = s.Kind
			return failure(CodeServiceFailed, fmt.Sprintf("%s %s stopped answering while the app started.", s.profile.title, s.Version), false, checkErr)
		}
	}
	return nil
}

func (l *launch) recordComponents(ctx context.Context) error {
	if l.r.components == nil {
		return nil
	}
	if err := l.r.components.RecordComponents(ctx, l.plan.components(l.id, l.settings, l.outbound, l.withheld)); err != nil {
		return failure(CodeUnavailable, "The preview's components could not be recorded.", true, err)
	}
	return nil
}

func (l *launch) checkProcesses(ctx context.Context) error {
	if err := l.r.checkProcesses(ctx, l.processes); err != nil {
		l.failed = l.failedProcess(ctx)
		return err
	}
	return nil
}

// discard captures the output that explains the failure (the failing
// component's, else the app's), then removes every container of the
// candidate.
func (l *launch) discard(ctx context.Context) {
	if companion, ok := l.created[l.failed]; ok && l.failed != "" {
		l.r.captureOutputOf(ctx, companion, l.plan.componentTitle(l.failed)+" output", l.logs)
	} else if app, ok := l.created[""]; ok {
		l.r.captureOutput(ctx, app, l.logs)
	}
	l.r.discardCandidate(ctx, l.id.PullRequestID, l.plan.key)
}

// failedService names the first service that is not running, for its output.
func (l *launch) failedService(ctx context.Context, ids map[string]string) string {
	for _, s := range l.plan.services {
		inspected, err := l.r.docker.ContainerInspect(ctx, ids[s.Kind], client.ContainerInspectOptions{})
		if err != nil || !observedHealth(inspected.Container).Running {
			return s.Kind
		}
	}
	if len(l.plan.services) > 0 {
		return l.plan.services[0].Kind
	}
	return ""
}

// failedProcess names the first process that is not running.
func (l *launch) failedProcess(ctx context.Context) string {
	for _, name := range slices.Sorted(maps.Keys(l.processes)) {
		inspected, err := l.r.docker.ContainerInspect(ctx, l.processes[name], client.ContainerInspectOptions{})
		if err != nil {
			return "process:" + name
		}
		if h := observedHealth(inspected.Container); !h.Running || h.Restarts > 0 {
			return "process:" + name
		}
	}
	return ""
}

func (b backend) componentTitle(component string) string {
	for _, s := range b.services {
		if s.Kind == component {
			return s.profile.title + " " + s.Version
		}
	}
	if name, ok := strings.CutPrefix(component, "process:"); ok {
		return "Process " + name
	}
	return component
}

// components describes the ready candidate for the dashboard and reports.
func (b backend) components(id requestIdentity, settings repoconfig.Config, outbound int, withheld bool) Components {
	c := Components{PullRequestID: id.PullRequestID, Generation: id.Generation, Revision: id.Revision, Setup: len(b.setup) > 0,
		DocsPath: settings.DocsPath, Outbound: outbound, OutboundWithheld: withheld, ConfigHash: b.configHash}
	for _, s := range b.services {
		c.Services = append(c.Services, ServiceComponent{Kind: s.Kind, Version: s.Version})
	}
	for _, p := range b.processes {
		c.Processes = append(c.Processes, p.Name)
	}
	return c
}
