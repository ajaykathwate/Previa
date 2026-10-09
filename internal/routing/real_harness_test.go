package routing_test

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// The real routing tests publish real previews through a real Traefik whose
// wildcard certificate comes from a local Pebble ACME server over DNS-01, with
// PostgreSQL in a throwaway schema. They need Docker, the egress image
// (make egress-image), TEST_DATABASE_URL and internet access to pull images.
// Run them with make test-routing.

const (
	traefikImage   = "traefik:v3.7.13@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0"
	pebbleImage    = "ghcr.io/letsencrypt/pebble@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199"
	challengeImage = "ghcr.io/letsencrypt/pebble-challtestsrv@sha256:12ce21884def456bcf9786542113949e1f19dc7738d2c70e156c2d0c38a1405b"
	appBaseImage   = "alpine:3.22"
	baseDomain     = "preview.test"
	unavailableURL = "https://previa.dashboard.test/previews/unavailable"
	appPort        = 8080
)

// The candidate app answers every request with its MARKER, which names the
// PR, generation and revision it was built for.
const appCommand = `while true; do printf "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: ${#MARKER}\r\nConnection: close\r\n\r\n%s" "$MARKER" | nc -l -p 8080 >/dev/null 2>&1; done`

// dnsHook is lego's exec DNS provider: it publishes the DNS-01 TXT record in
// Pebble's challenge test server, which Pebble then queries to validate.
const dnsHook = `#!/bin/sh
set -eu
case "$1" in
  present) wget -q -O /dev/null --post-data "{\"host\":\"$2\",\"value\":\"$3\"}" http://dns:8055/set-txt ;;
  cleanup) wget -q -O /dev/null --post-data "{\"host\":\"$2\"}" http://dns:8055/clear-txt ;;
esac
`

var (
	run        = time.Now().UnixNano()
	runSuffix  = strconv.FormatInt(run%2_000_000_000, 36)
	sharedMu   sync.Mutex
	shared     *harness
	sharedErr  error
	routerArgs = []string{
		"--global.checknewversion=false", "--global.sendanonymoususage=false", "--log.level=INFO",
		"--providers.file.directory=/etc/traefik/dynamic", "--providers.file.watch=true", "--providers.providersthrottleduration=250ms",
		"--entrypoints.web.address=:80", "--entrypoints.web.http.redirections.entrypoint.to=websecure", "--entrypoints.web.http.redirections.entrypoint.scheme=https",
		"--entrypoints.websecure.address=:443", "--entrypoints.websecure.http.aliasheadersstrategy=delete",
		"--entrypoints.websecure.http.tls.certresolver=preview", "--entrypoints.websecure.http.tls.domains[0].main=*." + baseDomain,
		"--entrypoints.traefik.address=127.0.0.1:8080", "--api=true", "--api.insecure=true", "--api.dashboard=false",
		"--ping=true", "--ping.entrypoint=traefik",
		"--certificatesresolvers.preview.acme.email=ops@" + baseDomain, "--certificatesresolvers.preview.acme.storage=/acme/acme.json",
		"--certificatesresolvers.preview.acme.caserver=https://pebble:14000/dir",
		// Pebble issues six-day certificates; at Traefik's default (90 days) it
		// would renew one at every start, as if it were about to expire.
		"--certificatesresolvers.preview.acme.certificatesduration=144",
		"--certificatesresolvers.preview.acme.dnschallenge.provider=exec",
		"--certificatesresolvers.preview.acme.dnschallenge.propagation.disablechecks=true",
	}
)

func TestMain(m *testing.M) {
	code := m.Run()
	sharedMu.Lock()
	if shared != nil {
		shared.close()
	}
	sharedMu.Unlock()
	os.Exit(code)
}

// harness is one Traefik stack, database schema and runtime shared by the
// real tests, which run one after another with their own pull requests.
type harness struct {
	docker      *client.Client
	instance    string // this database's instance: every Docker object it creates carries it
	admin, pool *pgxpool.Pool
	schema      string
	repo        *env.Repository
	service     *env.Service
	worker      *env.OperationWorker
	executors   env.Executors
	events      *webhooks.Service
	broker      *env.Broker
	runtime     *runtime.Runtime
	gateway     *egress.Gateway
	controller  *routing.Controller
	traefik     *routing.Traefik
	file        *routing.File
	builder     *commitBuilder
	reports     *reportRecorder
	logger      *slog.Logger

	repoID, githubRepoID, installationID int64
	networkName, acmeVolume, dir         string
	routerName, pebbleName, dnsName      string
	httpsAddr, httpAddr                  string
	roots                                *x509.CertPool
	prs                                  []int64
}

func realHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("TEST_ROUTING") != "1" {
		t.Skip("set TEST_ROUTING=1 (make test-routing) to publish real previews")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set TEST_DATABASE_URL for the real routing tests")
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared == nil && sharedErr == nil {
		shared, sharedErr = startHarness()
	}
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	return shared
}

func startHarness() (*harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	h := &harness{
		routerName: "previa-rt-router-" + runSuffix, pebbleName: "previa-rt-pebble-" + runSuffix, dnsName: "previa-rt-dns-" + runSuffix,
		networkName: "previa-rt-acme-" + runSuffix, acmeVolume: "previa-rt-acme-" + runSuffix,
		githubRepoID: 800_000_000 + run%100_000_000, installationID: 900_000_000 + run%100_000_000,
		reports: &reportRecorder{},
	}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("TEST_ROUTING_LOG") == "1" {
		h.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	var err error
	if h.docker, err = dockerx.New(ctx); err != nil {
		return nil, err
	}
	steps := []func(context.Context) error{h.startDatabase, h.startRouterStack, h.startPrevia, h.awaitCertificate}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			h.close()
			return nil, err
		}
	}
	return h, nil
}

func (h *harness) startDatabase(ctx context.Context) error {
	url := os.Getenv("TEST_DATABASE_URL")
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect to the test database: %w", err)
	}
	h.admin = admin
	h.schema = "routing_real_" + strconv.FormatInt(run, 10)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{h.schema}.Sanitize()); err != nil {
		return err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return errors.New("invalid TEST_DATABASE_URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = h.schema
	if h.pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		return err
	}
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := h.pool.Exec(ctx, up); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(file), err)
		}
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(h.pool), nil)
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return err
	}
	var userID, installation int64
	if err := h.pool.QueryRow(ctx, "INSERT INTO users(github_id, username) VALUES ($1, 'routing') RETURNING id", h.installationID).Scan(&userID); err != nil {
		return err
	}
	if err := h.pool.QueryRow(ctx, "INSERT INTO installations(github_installation_id, owner_user_id, status, account_login) VALUES ($1, $2, 'active', 'routing') RETURNING id", h.installationID, userID).Scan(&installation); err != nil {
		return err
	}
	return h.pool.QueryRow(ctx, "INSERT INTO repos(installation_id, full_name, github_repo_id, accessible, build_mode) VALUES ($1, $2, $3, true, 'automatic') RETURNING id",
		installation, "routing/real-"+runSuffix, h.githubRepoID).Scan(&h.repoID)
}

func (h *harness) startRouterStack(ctx context.Context) error {
	for _, ref := range []string{traefikImage, pebbleImage, challengeImage, appBaseImage} {
		if err := ensureImage(ctx, h.docker, ref); err != nil {
			return err
		}
	}
	var err error
	if h.dir, err = os.MkdirTemp("", "previa-routing-"); err != nil {
		return err
	}
	for _, sub := range []string{"dynamic", "certs", "scripts"} {
		if err := os.Mkdir(filepath.Join(h.dir, sub), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(h.dir, "scripts", "dns.sh"), []byte(dnsHook), 0o755); err != nil {
		return err
	}
	if err := h.copyPebbleRoot(ctx); err != nil {
		return err
	}
	if _, err := h.docker.NetworkCreate(ctx, h.networkName, client.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		return fmt.Errorf("create ACME test network: %w", err)
	}
	if err := h.run(ctx, h.dnsName, "dns", &container.Config{Image: challengeImage, Cmd: []string{"-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", ""}}, &container.HostConfig{}); err != nil {
		return err
	}
	if err := h.run(ctx, h.pebbleName, "pebble", &container.Config{
		Image: pebbleImage, Cmd: []string{"-config", "test/config/pebble-config.json", "-dnsserver", "dns:8053"},
		Env:          []string{"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0"},
		ExposedPorts: network.PortSet{network.MustParsePort("15000/tcp"): {}},
	}, &container.HostConfig{PortBindings: network.PortMap{network.MustParsePort("15000/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1")}}}}); err != nil {
		return err
	}
	// Fixed host ports: Docker picks new ephemeral ports when a container
	// restarts, and the restart tests need the same address afterwards.
	if h.httpsAddr, err = freeLoopbackAddress(); err != nil {
		return err
	}
	if h.httpAddr, err = freeLoopbackAddress(); err != nil {
		return err
	}
	return h.startRouter(ctx)
}

func freeLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String(), nil
}

func (h *harness) startRouter(ctx context.Context) error {
	return h.run(ctx, h.routerName, "router", &container.Config{
		Image: traefikImage, Cmd: routerArgs,
		Env:          []string{"LEGO_CA_CERTIFICATES=/certs/pebble.minica.pem", "EXEC_PATH=/scripts/dns.sh"},
		ExposedPorts: network.PortSet{network.MustParsePort("443/tcp"): {}, network.MustParsePort("80/tcp"): {}},
	}, &container.HostConfig{
		PortBindings: network.PortMap{
			network.MustParsePort("443/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: portOf(h.httpsAddr)}},
			network.MustParsePort("80/tcp"):  {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: portOf(h.httpAddr)}},
		},
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: filepath.Join(h.dir, "dynamic"), Target: "/etc/traefik/dynamic", ReadOnly: true},
			{Type: mount.TypeBind, Source: filepath.Join(h.dir, "certs"), Target: "/certs", ReadOnly: true},
			{Type: mount.TypeBind, Source: filepath.Join(h.dir, "scripts"), Target: "/scripts", ReadOnly: true},
			{Type: mount.TypeVolume, Source: h.acmeVolume, Target: "/acme"},
		},
	})
}

func portOf(address string) string {
	_, port, _ := net.SplitHostPort(address)
	return port
}

// run starts a test infrastructure container on the ACME network.
func (h *harness) run(ctx context.Context, name, alias string, cfg *container.Config, host *container.HostConfig) error {
	host.NetworkMode = container.NetworkMode(h.networkName)
	created, err := h.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{h.networkName: {Aliases: []string{alias}}}}})
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := h.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

func (h *harness) published(ctx context.Context, name, port string) (string, error) {
	for range 50 {
		inspected, err := h.docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if err != nil {
			return "", err
		}
		if s := inspected.Container.NetworkSettings; s != nil {
			for _, binding := range s.Ports[network.MustParsePort(port)] {
				if binding.HostPort != "" {
					return net.JoinHostPort("127.0.0.1", binding.HostPort), nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("%s publishes no %s port", name, port)
}

// copyPebbleRoot extracts the test CA that signs Pebble's own HTTPS API, so
// Traefik's ACME client trusts it.
func (h *harness) copyPebbleRoot(ctx context.Context) error {
	created, err := h.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: pebbleImage}})
	if err != nil {
		return err
	}
	defer func() {
		_, _ = h.docker.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	copied, err := h.docker.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: "/test/certs/pebble.minica.pem"})
	if err != nil {
		return err
	}
	defer func() { _ = copied.Content.Close() }()
	archive := tar.NewReader(copied.Content)
	if _, err := archive.Next(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(archive, 1<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.dir, "certs", "pebble.minica.pem"), data, 0o644)
}

// startPrevia assembles the real lifecycle: environments with the outbox, the
// real runtime (with a fast image builder), the route controller and Traefik.
func (h *harness) startPrevia(ctx context.Context) error {
	settings, err := config.LoadExecution()
	if err != nil {
		return err
	}
	if h.gateway, err = egress.NewGateway(h.docker, settings.EgressImage); err != nil {
		return err
	}
	if _, err := h.gateway.Ensure(ctx); err != nil {
		return fmt.Errorf("start the egress proxy (make egress-image): %w", err)
	}
	producer, err := queue.NewProducer(h.pool, h.logger)
	if err != nil {
		return err
	}
	operations, err := env.NewOperationBroker(producer)
	if err != nil {
		return err
	}
	h.repo = env.NewRepository(h.pool, operations).WithHostnames(routing.NewLabel).WithReports()
	h.reports.pool = h.pool
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	keys, err := secrets.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), "1")
	if err != nil {
		return err
	}
	provider, err := secrets.NewProvider(noSecrets{}, keys)
	if err != nil {
		return err
	}
	policy := runtime.PolicyFrom(settings.PreviewConfig)
	policy.StartupTimeout = time.Minute
	h.builder = &commitBuilder{docker: h.docker}
	instance, err := database.Instance(ctx, h.pool)
	if err != nil {
		return err
	}
	h.instance = instance
	h.runtime, err = runtime.New(runtime.Parts{
		Instance: instance, Admission: runtime.NewAdmissionLock(h.pool), Docker: h.docker, Egress: h.gateway, Targets: runtime.NewTargetStore(h.pool), Sources: appSources{}, Builder: h.builder, Secrets: provider,
		Logs: func(context.Context, env.Attempt, *secrets.Redactor) (runtime.LogSink, error) {
			return discardSink{}, nil
		},
	}, policy, h.logger)
	if err != nil {
		return err
	}
	if h.service, err = env.NewService(h.repo, h.runtime, h.logger, 5*time.Minute); err != nil {
		return err
	}
	if h.traefik, err = routing.NewTraefik(h.docker, h.routerName, "127.0.0.1:8080"); err != nil {
		return err
	}
	if h.file, err = routing.NewFile(filepath.Join(h.dir, "dynamic")); err != nil {
		return err
	}
	if h.controller, err = h.newController(h.traefik); err != nil {
		return err
	}
	h.executors = env.Executors{env.ActivateRoute: h.controller, env.RemoveRoute: h.controller, env.CleanupArtifact: h.runtime.CleanupExecutor(), env.ReportStatus: h.reports}
	if h.worker, err = h.newWorker(h.executors); err != nil {
		return err
	}
	if h.broker, err = env.NewExecutionBroker(h.pool, producer, h.service, h.worker, nil); err != nil {
		return err
	}
	h.events = webhooks.NewService(h.pool, h.broker)
	// The first table (no routes yet) carries the fallback router, whose use of
	// the wildcard resolver makes Traefik obtain the certificate.
	return h.controller.Sync(ctx)
}

// backends connects the route controller to the runtime's candidate check.
func (h *harness) backends() routing.Backends {
	return routing.BackendFunc(func(ctx context.Context, c routing.Candidate) (routing.Backend, error) {
		endpoint, err := h.runtime.Endpoint(ctx, runtime.Expected(c))
		if err != nil {
			return routing.Backend{}, runtime.AsOperationFailure(err)
		}
		return routing.Backend(endpoint), nil
	})
}

func (h *harness) newController(router routing.Router) (*routing.Controller, error) {
	controller, err := routing.NewController(h.repo, h.backends(), router, h.file,
		routing.Settings{BaseDomain: baseDomain, EntryPoint: "websecure", UnavailableURL: unavailableURL}, h.logger)
	if err != nil {
		return nil, err
	}
	controller.Converge = 45 * time.Second
	return controller, nil
}

func (h *harness) newWorker(executor env.OperationExecutor) (*env.OperationWorker, error) {
	return env.NewOperationWorker(h.repo, executor, h.logger, 4*time.Minute, map[env.OperationKind]time.Duration{
		env.ActivateRoute: 2 * time.Minute, env.RemoveRoute: 2 * time.Minute, env.CleanupArtifact: 90 * time.Second, env.ReportStatus: 5 * time.Second,
	})
}

// awaitCertificate waits until Traefik serves Pebble's wildcard certificate
// and records Pebble's root to verify it.
func (h *harness) awaitCertificate(ctx context.Context) error {
	mgmt, err := h.published(ctx, h.pebbleName, "15000/tcp")
	if err != nil {
		return err
	}
	minica, err := os.ReadFile(filepath.Join(h.dir, "certs", "pebble.minica.pem"))
	if err != nil {
		return err
	}
	apiRoots := x509.NewCertPool()
	apiRoots.AppendCertsFromPEM(minica)
	_, port, _ := net.SplitHostPort(mgmt)
	pebbleClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: apiRoots, ServerName: "localhost"}}}
	deadline := time.Now().Add(3 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		if h.roots == nil {
			if response, err := pebbleClient.Get("https://localhost:" + port + "/roots/0"); err == nil {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				_ = response.Body.Close()
				roots := x509.NewCertPool()
				if response.StatusCode == http.StatusOK && roots.AppendCertsFromPEM(body) {
					h.roots = roots
				}
			} else {
				last = err
			}
		}
		if h.roots != nil {
			_, err := h.certificate("probe." + baseDomain)
			if err == nil {
				return nil
			}
			last = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("traefik did not serve a Pebble wildcard certificate: %w", last)
}

func tlsConfig(roots *x509.CertPool, host string) *tls.Config {
	return &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}
}

// certificate completes a verified TLS handshake for host and returns the leaf.
func (h *harness) certificate(host string) (*x509.Certificate, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", h.httpsAddr, tlsConfig(h.roots, host))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0], nil
}

func (h *harness) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if h.runtime != nil {
		for _, pr := range h.prs {
			resources, _ := h.runtime.Resources(ctx, pr)
			for _, resource := range resources {
				_ = h.runtime.RemoveResource(ctx, resource)
			}
			for _, resource := range resources {
				if resource.Artifact.ImageTag != "" {
					_ = h.runtime.RemoveResource(ctx, env.Resource{PullRequestID: pr, Artifact: env.Artifact{ImageTag: resource.Artifact.ImageTag}})
				}
			}
		}
	}
	if h.docker != nil {
		for _, name := range []string{h.routerName, h.pebbleName, h.dnsName} {
			_, _ = h.docker.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		}
		_, _ = h.docker.NetworkRemove(ctx, h.networkName, client.NetworkRemoveOptions{})
		_, _ = h.docker.VolumeRemove(ctx, h.acmeVolume, client.VolumeRemoveOptions{Force: true})
		_ = h.docker.Close()
	}
	if h.pool != nil {
		h.pool.Close()
	}
	if h.admin != nil {
		_, _ = h.admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{h.schema}.Sanitize()+" CASCADE")
		h.admin.Close()
	}
	if h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
}

func ensureImage(ctx context.Context, docker *client.Client, ref string) error {
	if _, err := docker.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	pulled, err := docker.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = pulled.Close() }()
	return pulled.Wait(ctx)
}

// commitBuilder makes an image per job in milliseconds instead of a real
// build: it commits an alpine container with Previa's image labels and the
// marker app. Builds themselves are covered by make test-builder.
type commitBuilder struct {
	docker *client.Client
	mu     sync.Mutex
	gate   chan struct{} // when set, Build waits for it to close
	built  []string
}

func (b *commitBuilder) Build(ctx context.Context, job builds.Job) (builds.Image, error) {
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return builds.Image{}, ctx.Err()
		}
	}
	created, err := b.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: appBaseImage}})
	if err != nil {
		return builds.Image{}, err
	}
	defer func() {
		_, _ = b.docker.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	ref := job.ImageRef()
	label := func(name, value string) string { return "LABEL " + name + "=" + strconv.Quote(value) }
	committed, err := b.docker.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{Reference: ref, Changes: []string{
		label(dockerx.LabelManaged, "true"), label(dockerx.LabelInstance, job.Instance), label(dockerx.LabelKind, dockerx.KindImage),
		label(dockerx.LabelPR, strconv.FormatInt(job.PullRequestID, 10)), label(dockerx.LabelGeneration, strconv.FormatInt(job.Generation, 10)),
		label(dockerx.LabelRevision, strconv.FormatInt(job.Revision, 10)), label(dockerx.LabelSHA, job.HeadSHA),
		`CMD ["sh", "-c", ` + strconv.Quote(appCommand) + `]`,
	}})
	if err != nil {
		return builds.Image{}, err
	}
	b.mu.Lock()
	b.built = append(b.built, ref)
	b.mu.Unlock()
	return builds.Image{Ref: ref, ID: committed.ID}, nil
}

// appSources returns the marker app's configuration; the commit builder
// needs no source tree.
type appSources struct{}

func (appSources) Prepare(_ context.Context, r source.Request) (*source.Prepared, error) {
	return &source.Prepared{ContextDir: os.TempDir(), Dockerfile: "Dockerfile", Config: repoconfig.Result{Config: repoconfig.Config{
		Port: appPort, HealthCheckPath: "/",
		Env:       []string{"MARKER=" + marker(r.PullRequestID, r.Generation, r.Revision)},
		Resources: repoconfig.Resources{CPUMillis: 250, MemoryBytes: 64 << 20},
	}}}, nil
}

func (appSources) Release(*source.Prepared) error { return nil }

func marker(pr, generation, revision int64) string {
	return fmt.Sprintf("pr%d-g%d-r%d", pr, generation, revision)
}

type noSecrets struct{}

func (noSecrets) SealedSecrets(context.Context, int64) ([]secrets.SealedSecret, error) {
	return nil, nil
}

type discardSink struct{}

func (discardSink) Write(p []byte) (int, error) { return len(p), nil }
func (discardSink) System(string)               {}
func (discardSink) Close(context.Context) error { return nil }

// reportRecorder stands in for the GitHub reporter: like it, it reads the
// PR's persisted state when it runs, here only the environment's failure
// category.
type reportRecorder struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	codes []string
}

func (r *reportRecorder) Execute(ctx context.Context, op env.Operation) error {
	var code string
	if err := r.pool.QueryRow(ctx, "SELECT COALESCE(last_error_code, '') FROM environments WHERE pull_request_id=$1", op.PullRequestID).Scan(&code); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, code)
	return nil
}
