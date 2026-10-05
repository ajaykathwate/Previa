package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Observed is one router as Traefik's API reports it.
type Observed struct {
	Present bool
	Service string // without the provider suffix
	Status  string // enabled, warning or disabled
	Errors  []string
}

// ErrRouterUnavailable means the router container could not be reached or
// controlled; route operations retry it.
var ErrRouterUnavailable = errors.New("router unavailable")

// Traefik controls the router container from the worker, which alone holds the
// Docker socket: it joins the router to preview networks and reads Traefik's
// API, which listens only on the container's loopback interface, by running
// wget inside the container. Traefik itself gets no Docker access.
type Traefik struct {
	docker    *client.Client
	container string
	api       string
}

// NewTraefik controls the named router container whose API listens on
// apiAddress (host:port) inside it.
func NewTraefik(docker *client.Client, container, apiAddress string) (*Traefik, error) {
	host, port, err := net.SplitHostPort(apiAddress)
	if docker == nil || !backendPattern.MatchString(container) || err != nil || host == "" || port == "" {
		return nil, errors.New("router control needs Docker, the router container's name and its API address")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("the router's API must listen on a loopback address inside its container")
	}
	return &Traefik{docker: docker, container: container, api: apiAddress}, nil
}

// Attach connects the router to a preview network so it can reach that PR's
// candidates. Attaching twice is harmless.
func (t *Traefik) Attach(ctx context.Context, networkID string) error {
	inspected, err := t.docker.ContainerInspect(ctx, t.container, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("%w: inspect router container: %w", ErrRouterUnavailable, err)
	}
	if t.attached(inspected.Container.NetworkSettings, networkID) {
		return nil
	}
	if _, err := t.docker.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: t.container, EndpointConfig: &network.EndpointSettings{}}); err != nil {
		if again, inspectErr := t.docker.ContainerInspect(ctx, t.container, client.ContainerInspectOptions{}); inspectErr == nil && t.attached(again.Container.NetworkSettings, networkID) {
			return nil // A concurrent attach won.
		}
		return fmt.Errorf("%w: connect router to preview network: %w", ErrRouterUnavailable, err)
	}
	return nil
}

func (t *Traefik) attached(settings *container.NetworkSettings, networkID string) bool {
	if settings == nil {
		return false
	}
	for _, endpoint := range settings.Networks {
		if endpoint != nil && endpoint.NetworkID == networkID {
			return true
		}
	}
	return false
}

// Router reads one router of the file provider from Traefik's API.
func (t *Traefik) Router(ctx context.Context, name string) (Observed, error) {
	result, err := dockerx.Run(ctx, t.docker, t.container, "wget", "-q", "-S", "-T", "3", "-O", "-",
		"http://"+t.api+"/api/http/routers/"+name+"@file")
	if err != nil {
		if cerrdefs.IsNotFound(err) || cerrdefs.IsConflict(err) {
			return Observed{}, fmt.Errorf("%w: %w", ErrRouterUnavailable, err)
		}
		return Observed{}, fmt.Errorf("%w: query router API: %w", ErrRouterUnavailable, err)
	}
	if result.ExitCode != 0 {
		if strings.Contains(result.Stderr, " 404 ") {
			return Observed{}, nil
		}
		return Observed{}, fmt.Errorf("%w: router API request failed", ErrRouterUnavailable)
	}
	var body struct {
		Service string   `json:"service"`
		Status  string   `json:"status"`
		Errors  []string `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &body); err != nil {
		return Observed{}, fmt.Errorf("%w: decode router API response: %w", ErrRouterUnavailable, err)
	}
	service, _, _ := strings.Cut(body.Service, "@")
	return Observed{Present: true, Service: service, Status: body.Status, Errors: body.Errors}, nil
}
