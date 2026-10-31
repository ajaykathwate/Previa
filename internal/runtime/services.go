package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// This file runs a candidate's managed services and its setup and process
// containers: the services network, pinned images, protocol readiness, the
// one-off setup command, and the processes' liveness.

// servicesNetworkName is the PR's network for its managed services. Only the
// PR's candidates (apps, processes, setup) and their services join it: no
// egress proxy and no router, so services reach nothing beyond it.
func (r *Runtime) servicesNetworkName(pullRequestID int64) string {
	return r.networkName(pullRequestID) + "-services"
}

// servicesNetwork returns the PR's internal services network, creating it
// when missing. It refuses a same-named network Previa did not create.
func (r *Runtime) servicesNetwork(ctx context.Context, id requestIdentity, t Target) (string, error) {
	name := r.servicesNetworkName(id.PullRequestID)
	for attempt := 0; attempt < 2; attempt++ {
		inspected, err := r.docker.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			if !dockerx.OwnedBy(inspected.Network.Labels, r.instance, dockerx.KindServiceNetwork) || !inspected.Network.Internal {
				return "", failure(CodeNotOwned, "A Docker network with this preview's services network name was not created by Previa.", false, nil)
			}
			return inspected.Network.ID, nil
		}
		if !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("inspect services network: %w", err)
		}
		created, err := r.docker.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: dockerx.KindServiceNetwork,
			dockerx.LabelPR: strconv.FormatInt(id.PullRequestID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(t.BaseRepoID, 10),
		}})
		if cerrdefs.IsConflict(err) {
			continue // Created concurrently; inspect it.
		}
		if err != nil {
			return "", fmt.Errorf("create services network: %w", err)
		}
		return created.ID, nil
	}
	return "", errors.New("services network could not be created")
}

// serviceImages makes sure every service's pinned image is present, pulling
// it when needed, and returns the volumes each image declares (they become
// tmpfs mounts). It runs before admission: a pull may take minutes and never
// holds the admission lock.
func (r *Runtime) serviceImages(ctx context.Context, services []serviceRun, logs LogSink) (map[string][]string, error) {
	volumes := map[string][]string{}
	for _, s := range services {
		inspected, err := r.docker.ImageInspect(ctx, s.Image)
		if cerrdefs.IsNotFound(err) {
			logs.System(fmt.Sprintf("Pulling the %s %s image pinned by the operator.", s.profile.title, s.Version))
			pulled, pullErr := r.docker.ImagePull(ctx, s.Image, client.ImagePullOptions{})
			if pullErr == nil {
				pullErr = pulled.Wait(ctx)
				_ = pulled.Close()
			}
			if pullErr != nil {
				return nil, r.interrupted(ctx, failure(CodeServiceUnavailable, fmt.Sprintf("The %s %s image could not be pulled.", s.profile.title, s.Version), true, pullErr))
			}
			inspected, err = r.docker.ImageInspect(ctx, s.Image)
		}
		if err != nil {
			return nil, failure(CodeServiceUnavailable, fmt.Sprintf("The %s %s image could not be inspected.", s.profile.title, s.Version), true, err)
		}
		if inspected.Config != nil {
			volumes[s.Kind] = slices.Sorted(maps.Keys(inspected.Config.Volumes))
		}
	}
	return volumes, nil
}

// waitServicesReady waits until every service answers its protocol, not just
// a TCP accept: SQL services run an authenticated query and Redis answers an
// authenticated PING, each from inside the service's own container.
func (r *Runtime) waitServicesReady(ctx context.Context, services []serviceRun, ids map[string]string, logs LogSink) error {
	deadline, cancel := context.WithTimeout(ctx, r.policy.StartupTimeout)
	defer cancel()
	for _, s := range services {
		logs.System(fmt.Sprintf("Waiting up to %s for %s %s to accept authenticated connections.", r.policy.StartupTimeout, s.profile.title, s.Version))
		if err := r.waitServiceReady(ctx, deadline, s, ids[s.Kind]); err != nil {
			return err
		}
		logs.System(fmt.Sprintf("%s %s is ready at %s:%d.", s.profile.title, s.Version, s.alias, s.profile.port))
	}
	return nil
}

// waitServiceReady probes one service until it answers, stops, or the
// startup deadline every service shares passes.
func (r *Runtime) waitServiceReady(ctx, deadline context.Context, s serviceRun, containerID string) error {
	reason := "it never answered"
	for {
		ready, why, err := r.probeService(deadline, s, containerID)
		if err != nil || ready {
			return err
		}
		if why != "" {
			reason = why
		}
		select {
		case <-deadline.Done():
			if ctx.Err() != nil {
				return r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled while services were starting.", false, ctx.Err()))
			}
			return failure(CodeServiceFailed, fmt.Sprintf("%s %s was not ready within %s: %s.", s.profile.title, s.Version, r.policy.StartupTimeout, reason), false, nil)
		case <-time.After(probeInterval):
		}
	}
}

// probeService checks a starting service once. A stopped container fails
// it; otherwise it reports whether the service answers and, if not, why. A
// check that fails only because the deadline passed is no error: the caller
// reports the timeout.
func (r *Runtime) probeService(deadline context.Context, s serviceRun, containerID string) (ready bool, why string, err error) {
	inspected, err := r.docker.ContainerInspect(deadline, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if deadline.Err() == nil {
			return false, "", failure(CodeUnavailable, "A service could not be inspected while starting.", true, err)
		}
		return false, "", nil
	}
	if h := observedHealth(inspected.Container); h.State == string(container.StateExited) || h.State == string(container.StateDead) {
		message := fmt.Sprintf("%s %s stopped while starting (exit code %d).", s.profile.title, s.Version, h.ExitCode)
		if h.OOMKilled {
			message = fmt.Sprintf("%s %s ran out of memory while starting.", s.profile.title, s.Version)
		}
		return false, "", failure(CodeServiceFailed, message, false, nil)
	}
	ready, why, err = r.serviceAnswers(deadline, s, containerID, true)
	if err != nil && deadline.Err() == nil {
		return false, "", failure(CodeUnavailable, "A service readiness check could not run.", true, err)
	}
	return ready, why, nil
}

// serviceAnswers runs a service's protocol check inside its container. With
// credentials it authenticates (SQL services run SELECT 1, Redis answers PING
// with PONG); without them it checks that the server speaks its protocol
// (pg_isready, mysqladmin ping, or Redis asking for authentication).
func (r *Runtime) serviceAnswers(ctx context.Context, s serviceRun, containerID string, authenticated bool) (bool, string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var env, cmd []string
	port := strconv.Itoa(s.profile.port)
	switch {
	case s.Kind == "postgres" && authenticated:
		env = []string{"PGPASSWORD=" + s.password, "PGCONNECT_TIMEOUT=3"}
		cmd = []string{"psql", "-h", "127.0.0.1", "-p", port, "-U", serviceDatabase, "-d", serviceDatabase, "-tAc", "SELECT 1"}
	case s.Kind == "postgres":
		cmd = []string{"pg_isready", "-h", "127.0.0.1", "-p", port, "-t", "3"}
	case s.Kind == "mysql" && authenticated:
		env = []string{"MYSQL_PWD=" + s.password}
		cmd = []string{"mysql", "--protocol=tcp", "--host=127.0.0.1", "--port=" + port, "--user=" + serviceDatabase, "--database=" + serviceDatabase,
			"--connect-timeout=3", "--batch", "--skip-column-names", "--execute=SELECT 1"}
	case s.Kind == "mysql":
		cmd = []string{"mysqladmin", "--protocol=tcp", "--host=127.0.0.1", "--port=" + port, "--connect-timeout=3", "ping"}
	case authenticated:
		env = []string{"REDISCLI_AUTH=" + s.password}
		cmd = []string{"redis-cli", "-h", "127.0.0.1", "-p", port, "PING"}
	default:
		cmd = []string{"redis-cli", "-h", "127.0.0.1", "-p", port, "PING"}
	}
	result, err := dockerx.RunWithEnv(checkCtx, r.docker, containerID, env, cmd...)
	if err != nil {
		if ctx.Err() == nil && checkCtx.Err() != nil {
			return false, "its check timed out", nil
		}
		return false, "", err
	}
	switch {
	case (s.Kind == "postgres" || s.Kind == "mysql") && authenticated:
		return result.ExitCode == 0 && result.Stdout == "1", "it does not accept connections yet", nil
	case s.Kind == "postgres":
		return result.ExitCode == 0, "it does not accept connections", nil
	case s.Kind == "mysql":
		// mysqladmin ping reports success on an authentication refusal too:
		// either response proves this is a live MySQL server.
		return result.ExitCode == 0, "it does not answer MySQL ping", nil
	case authenticated:
		return result.Stdout == "PONG", "it does not answer PING yet", nil
	}
	// Unauthenticated, Redis still answers in its protocol: asking for a
	// password is an answer.
	return result.Stdout == "PONG" || strings.HasPrefix(result.Stdout, "NOAUTH"), "it does not answer PING", nil
}

// runOneOff runs a setup or seed container to completion within the setup
// timeout, streaming its output into the build log. A non-zero exit or the
// timeout fails the candidate with code (setup_failed or seed_failed). The
// container is removed once it succeeds; a failed one may run again (a
// fallback to other starting data), and only the new run's output is
// streamed.
func (r *Runtime) runOneOff(ctx context.Context, containerID string, argv []string, logs LogSink, name, code string) error {
	logs.System(fmt.Sprintf("Running the %s command (%d argument%s) with up to %s.", name, len(argv), plural(len(argv)), r.policy.SetupTimeout))
	since := ""
	if inspected, err := r.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{}); err == nil && inspected.Container.State != nil {
		if finished, err := time.Parse(time.RFC3339Nano, inspected.Container.State.FinishedAt); err == nil && finished.Year() > 1 {
			since = strconv.FormatFloat(float64(finished.Add(time.Microsecond).UnixNano())/1e9, 'f', 6, 64)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, r.policy.SetupTimeout)
	defer cancel()
	waited := r.docker.ContainerWait(runCtx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := r.docker.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		r.logger.WarnContext(ctx, "start "+name+" container", "container_id", containerID, "error", err)
		return r.interrupted(ctx, failure(CodeUnavailable, "The "+name+" container could not be started.", true, err))
	}
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		reader, err := r.docker.ContainerLogs(runCtx, containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true, Since: since})
		if err != nil {
			return
		}
		defer func() { _ = reader.Close() }()
		_, _ = stdcopy.StdCopy(logs, logs, reader)
	}()
	var outcome error
	select {
	case result := <-waited.Result:
		switch {
		case result.Error != nil:
			outcome = failure(CodeUnavailable, "The "+name+" command's result could not be read.", true, errors.New(result.Error.Message))
		case result.StatusCode != 0:
			outcome = failure(code, fmt.Sprintf("The %s command exited with code %d.", name, result.StatusCode), false, nil)
		}
	case err := <-waited.Error:
		switch {
		case ctx.Err() != nil:
			outcome = r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled during "+name+".", false, err))
		case runCtx.Err() != nil:
			outcome = failure(code, fmt.Sprintf("The %s command did not finish within %s.", name, r.policy.SetupTimeout), false, nil)
		default:
			outcome = failure(CodeUnavailable, "The "+name+" command could not be watched.", true, err)
		}
	}
	select {
	case <-streamed:
	case <-time.After(10 * time.Second):
	}
	if outcome != nil {
		return outcome
	}
	logs.System("The " + name + " command finished.")
	r.discard(ctx, containerID)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// checkProcesses requires every process to be running: one that exited or is
// restarting fails the candidate (process_failed).
func (r *Runtime) checkProcesses(ctx context.Context, processes map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(processes)) {
		inspected, err := r.docker.ContainerInspect(ctx, processes[name], client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return failure(CodeProcessFailed, fmt.Sprintf("The process %s no longer exists.", name), false, nil)
		}
		if err != nil {
			return failure(CodeUnavailable, "A process could not be inspected.", true, err)
		}
		h := observedHealth(inspected.Container)
		if !h.Running || h.Restarts > 0 {
			message := fmt.Sprintf("The process %s stopped (exit code %d).", name, h.ExitCode)
			if h.OOMKilled {
				message = fmt.Sprintf("The process %s ran out of memory.", name)
			}
			return failure(CodeProcessFailed, message, false, nil)
		}
	}
	return nil
}
