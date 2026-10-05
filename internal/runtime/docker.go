package runtime

import (
	"context"
	"errors"
	"fmt"
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
// with the egress proxy attached, and the proxy URL for containers on it. It
// refuses a same-named network that Previa did not create or that is not
// internal.
func (r *Runtime) previewNetwork(ctx context.Context, id requestIdentity, t Target) (string, string, error) {
	name := r.networkName(id.PullRequestID)
	networkID := ""
	for attempt := 0; attempt < 2 && networkID == ""; attempt++ {
		inspected, err := r.docker.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			if !dockerx.OwnedBy(inspected.Network.Labels, r.instance, dockerx.KindPreviewNetwork) || !inspected.Network.Internal {
				return "", "", failure(CodeNotOwned, "A Docker network with this preview's name was not created by Previa.", false, nil)
			}
			networkID = inspected.Network.ID
			break
		}
		if !cerrdefs.IsNotFound(err) {
			return "", "", fmt.Errorf("inspect preview network: %w", err)
		}
		created, err := r.docker.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: dockerx.KindPreviewNetwork,
			dockerx.LabelPR: strconv.FormatInt(id.PullRequestID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(t.BaseRepoID, 10),
		}})
		if cerrdefs.IsConflict(err) {
			continue // Created concurrently; inspect it.
		}
		if err != nil {
			return "", "", fmt.Errorf("create preview network: %w", err)
		}
		networkID = created.ID
	}
	if networkID == "" {
		return "", "", errors.New("preview network could not be created")
	}
	proxyURL, err := r.egress.Attach(ctx, networkID)
	if err != nil {
		return "", "", err
	}
	return networkID, proxyURL, nil
}

// captureOutput copies the candidate's recent output into the log, bounded to
// the last 200 lines and 64 KiB. The log redacts it before storing.
func (r *Runtime) captureOutput(ctx context.Context, containerID string, logs LogSink) {
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
		logs.System("The app printed no output.")
		return
	}
	logs.System("App output (last " + outputLines + " lines at most):")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, _ = logs.Write([]byte(text))
}

// discard removes a failed or superseded candidate. A failure is logged, not
// returned: the candidate stays labelled, so Resources lists it and the
// environments cleanup contract removes it durably.
func (r *Runtime) discard(ctx context.Context, containerID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := r.RemoveContainer(ctx, containerID); err != nil {
		r.logger.WarnContext(ctx, "remove failed preview candidate", "container_id", containerID, "error", err)
	}
}
