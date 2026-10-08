package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

const (
	cleanupTimeout = 30 * time.Second
	outputLines    = "200"
	outputBytes    = 64 << 10
)

// networkName is the PR's preview network. A PR has one lifetime environment,
// so its candidates and active preview share it; nothing else joins it except
// the egress proxy and the router.
func (r *Runtime) networkName(pullRequestID int64) string {
	return "previa-" + r.instance + "-pr-" + strconv.FormatInt(pullRequestID, 10)
}

// previewNetwork returns the PR's internal network, creating it when missing,
// with the egress proxy attached, and the proxy's address on it. It refuses a
// same-named network that Previa did not create or that is not internal.
func (r *Runtime) previewNetwork(ctx context.Context, id requestIdentity, t Target) (string, netip.Addr, error) {
	name := r.networkName(id.PullRequestID)
	networkID := ""
	for attempt := 0; attempt < 2 && networkID == ""; attempt++ {
		inspected, err := r.docker.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			if !dockerx.OwnedBy(inspected.Network.Labels, r.instance, dockerx.KindPreviewNetwork) || !inspected.Network.Internal {
				return "", netip.Addr{}, failure(CodeNotOwned, "A Docker network with this preview's name was not created by Previa.", false, nil)
			}
			networkID = inspected.Network.ID
			break
		}
		if !cerrdefs.IsNotFound(err) {
			return "", netip.Addr{}, fmt.Errorf("inspect preview network: %w", err)
		}
		created, err := r.docker.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: dockerx.KindPreviewNetwork,
			dockerx.LabelPR: strconv.FormatInt(id.PullRequestID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(t.BaseRepoID, 10),
		}})
		if cerrdefs.IsConflict(err) {
			continue // Created concurrently; inspect it.
		}
		if err != nil {
			return "", netip.Addr{}, fmt.Errorf("create preview network: %w", err)
		}
		networkID = created.ID
	}
	if networkID == "" {
		return "", netip.Addr{}, errors.New("preview network could not be created")
	}
	addr, err := r.egress.Join(ctx, networkID)
	if err != nil {
		return "", netip.Addr{}, err
	}
	return networkID, addr, nil
}

// captureOutput copies the candidate's recent output into the log, bounded to
// the last 200 lines and 64 KiB. The log redacts it before storing.
func (r *Runtime) captureOutput(ctx context.Context, containerID string, logs LogSink) {
	r.captureOutputOf(ctx, containerID, "App output", logs)
}

// captureOutputOf copies one container's recent output into the log under a
// title, bounded like captureOutput.
func (r *Runtime) captureOutputOf(ctx context.Context, containerID, title string, logs LogSink) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	reader, err := r.docker.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: outputLines})
	if err != nil {
		logs.System("The app's output could not be read.")
		return
	}
	defer func() { _ = reader.Close() }()
	output := &dockerx.BoundedBuffer{Limit: outputBytes}
	if _, err := stdcopy.StdCopy(output, output, reader); err != nil {
		logs.System("The app's output could not be read completely.")
	}
	text := output.String()
	if strings.TrimSpace(text) == "" {
		logs.System(title + ": none.")
		return
	}
	logs.System(title + " (last " + outputLines + " lines at most):")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, _ = logs.Write([]byte(text))
}

// discard removes one container of a candidate (a finished setup container,
// for example). A failure is logged, not returned: the container stays
// labelled, so Resources lists it and the environments cleanup contract
// removes it durably.
func (r *Runtime) discard(ctx context.Context, containerID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := r.RemoveContainer(ctx, containerID); err != nil {
		r.logger.WarnContext(ctx, "remove preview container", "container_id", containerID, "error", err)
	}
}

// discardCandidate removes every container of a failed or superseded
// candidate: its app and its services, setup and processes. Failures are
// logged; whatever stays is labelled and removed by cleanup.
func (r *Runtime) discardCandidate(ctx context.Context, pullRequestID int64, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := r.removeCandidateByKey(ctx, pullRequestID, key); err != nil {
		r.logger.WarnContext(ctx, "remove failed preview candidate", "pr_id", pullRequestID, "error", err)
	}
}
