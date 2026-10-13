package app

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// A full test worker owns forwarding policy. Sharing the development proxy
// would overwrite its approvals and let its worker overwrite the test's.
func newE2EGateway(t *testing.T, docker *client.Client, image string) *egress.Gateway {
	t.Helper()
	name := "previa-egress-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	gateway, err := egress.NewNamedGateway(docker, image, name)
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the pipeline's cleanup: previews are removed first.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := docker.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("remove test egress container: %v", err)
		}
		if _, err := docker.NetworkRemove(ctx, name, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("remove test egress network: %v", err)
		}
	})
	return gateway
}
