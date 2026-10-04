package dockerx

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"
)

// New connects to the Docker Engine named by the standard environment
// (DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH; the local socket by
// default), negotiating the API version, and checks that it answers.
func New(ctx context.Context) (*client.Client, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("configure Docker client: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.Ping(pingCtx, client.PingOptions{}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("reach Docker Engine: %w", err)
	}
	return c, nil
}
