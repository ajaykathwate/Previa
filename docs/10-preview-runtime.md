# 10 · Preview runtime

`internal/runtime` turns an admitted build request into a **healthy, isolated
preview container that nothing routes to yet**. It implements
`environments.Runtime` by composing source preparation ([06](06-source-and-configuration.md)),
the image builder ([09](09-image-builds.md)) and repository secrets
([07](07-repository-secrets.md)); every lifecycle transaction stays in
`internal/environments`. The route controller ([12](12-routing-and-https.md)) publishes
its candidates and its cleanup executor removes them ([13](13-teardown-and-cleanup.md));
the worker composes it whenever it can build ([22](22-mvp-operations.md)), streaming
each attempt's output into the build log ([08](08-build-logs.md)). A candidate may also
bring managed PostgreSQL and Redis, a setup command and processes, and receive
owner-approved outbound connections; those parts and their isolation are in
[11](11-backend-previews.md).

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Outbound network | **Public internet only through the egress proxy**, ports 80/443, the same rule as builds | Previews can call sandbox APIs, OAuth providers and CDNs; nothing internal is reachable. Apps or libraries that ignore `HTTP(S)_PROXY` cannot reach the internet. |
| User and filesystem | **Strict**: an image that runs as root runs as UID 65532; read-only root with a 256 MiB tmpfs at `/tmp` | The safest default; incompatible apps get a server-wide operator opt-out, never a per-repository one. |
| Capacity | **Host-derived budget**: previews together may use 60% of Docker's memory and 75% of its CPUs, 10 per repository; previews get half the CPU weight and are OOM-killed first | Keeps the platform responsive however many previews start. |
| gVisor | **Optional** `PREVIEW_OCI_RUNTIME` (for example `runsc`); default Docker's runc | A low-cost isolation upgrade where installed. Not verified here: Docker Desktop has no gVisor. |
| One egress proxy | The build-only proxy became **`previa-egress`**, shared by builds and previews, and also runs readiness probes | One hardened component for every untrusted network; probes come from inside the preview's network, never from the worker. |

## How a candidate starts

1. **Recheck.** Read the PR, its repository, installation and intent: the request
   must still be the open PR's current generation, commit and running revision, with
   repository access and an active installation. Otherwise stop (`not_current` or
   `inaccessible`) before any work.
2. **Build.** Prepare the exact commit, build it with output going to the attempt's
   log, and release the source workspace (always, even on failure).
3. **Recheck again, then secrets.** Access and the PR's source can change during a
   build, so the target is read again right before secrets are decided. A fork, an
   unknown or deleted head, or anything GitHub marks as a fork gets **no secrets**
   (none are even decrypted); a same-repository PR gets them, overriding YAML entries.
   Any decryption failure stops the start: no container runs with partial secrets.
4. **Admit.** Under a PostgreSQL advisory lock held until the container exists,
   refuse (retryable `capacity`) if 10 other PRs of the repository already have
   previews, or the new limits plus every running, restarting or created preview
   (the active one and its candidate both count during a swap) would exceed the host
   budget. The request then waits for capacity instead of failing
   ([15](15-housekeeping-and-capacity.md)); before admission, `Admit` refuses early
   when not even the smallest preview would fit.
5. **Network.** The PR's lifetime network `previa-<instance>-pr-<id>` (internal,
   created once) gets the egress proxy attached. A same-named network this instance
   did not create is refused.
6. **Create and start** the hardened container, then **wait for readiness**.
7. **Capture** the app's last 200 lines (64 KiB) of output into the log, redacted.
   On any failure, also remove the candidate.

The result is `environments.Artifact{ContainerID, ImageTag}`; publication
([12](12-routing-and-https.md)) decides whether it becomes active.

## Isolation, concretely

| Control | Preview container |
| --- | --- |
| User | Image user when it is not root; otherwise `65532:65532` with `HOME=/tmp`. `PREVIEW_ALLOW_ROOT=true` keeps root (server-wide). |
| Filesystem | Read-only root; `/tmp` tmpfs (`nosuid,nodev`, 256 MiB, counted against memory). `PREVIEW_WRITABLE_ROOTFS=true` makes root writable (server-wide; disk writes are then unbounded). |
| Privileges | Never privileged; every capability dropped; `no-new-privileges`; Docker's default seccomp and AppArmor profiles. Low ports work without a capability (`ip_unprivileged_port_start=0`). |
| Host access | No host paths, Docker socket, devices or published ports. |
| Network | Only `previa-pr-<id>`, an internal network shared with nothing but the egress proxy and, once a version is published, the router. No gateway, no route to the host, Docker's other networks, other previews or metadata. IPv6 disabled in the container. |
| Egress | `HTTP_PROXY`/`HTTPS_PROXY` (and `NODE_USE_ENV_PROXY=1`) point at the proxy; Previa's values override YAML and secrets. The proxy resolves names itself and refuses private, loopback, link-local, metadata and other special ranges, so DNS tricks fail too. |
| Resources | The repository's tier, clamped to platform ceilings (small 0.5 CPU/512 MiB, standard 1/1 GiB, large 2/2 GiB); no swap; 512 processes; CPU weight 512 (half the default); OOM score adjustment 500; logs capped at 2 × 10 MB. |
| Crashes | Restart on failure at most 3 times; then it stays stopped and counts as a crash loop. |

**Verified** on Docker Desktop 29.8 (WSL2) by `make test-runtime`, from inside a running preview:

| Probe | Result |
| --- | --- |
| Internet bypassing the proxy | blocked |
| A service on Docker's default bridge (stand-in for Postgres and the control plane), direct and through the proxy | blocked |
| Another PR's preview, direct and through the proxy | blocked |
| `host.docker.internal`, direct and through the proxy | blocked |
| `169.254.169.254` metadata, direct and through the proxy | blocked |
| External DNS from the preview | not resolvable |
| `http://example.com` through the proxy | reached |
| Inside the container | UID 65532, read-only root, writable `/tmp`, no effective capabilities, `NoNewPrivs` set, no IPv6 address |

Also verified: a decrypted secret reaches a same-repository preview's environment and
appears as `[redacted]` in its log; a fork's preview never receives it; an app
answering HTTP 500 fails as unhealthy, a crashing app as a crash loop, and a
cancelled start as cancelled, each leaving no container; the operator opt-outs give
root and a writable root (still without capabilities); the memory budget and the
per-repository limit refuse candidates; removal refuses containers, images and
networks Previa did not create.

**Isolation level, stated plainly.** Previews are containers on the host's kernel,
not virtual machines. A kernel or runtime escape is not contained; gVisor
(`PREVIEW_OCI_RUNTIME=runsc`) narrows that where installed. Anyone with access to the
Docker host can read a preview's environment, including injected secrets
(`docker inspect`). The egress proxy is the one component every preview can reach;
it runs only Previa's proxy without credentials and dials only public addresses, so
previews cannot reach each other through it, but a vulnerability in it would be
shared. Admission checks run without a global lock, so concurrent starts can overshoot
the budget by at most the number of concurrent builds.

## Readiness and observed health

- With `health_check_path` in `.previewrc.yaml`, ready means an HTTP response from 200
  to 399 for that path; without it, ready means the port accepts TCP connections.
  Redirects are not followed.
- Probes run every second with a 2-second timeout, from inside the egress container
  (`previa-egress probe -target <ip:port>`). The target is always the candidate's own
  address on its network and the configured port: there is no way to name another URL.
- The deadline is `PREVIEW_STARTUP_TIMEOUT` (120 s). Failures are `unhealthy` (with
  the last probe reason), `crashed` (exit code, or out of memory) or, if the build
  itself ended, `cancelled`/`timeout`.
- `Runtime.Health(containerID)` reports the observed state (running, restarts, exit
  code, OOM, crash loop, start/finish times) for recovery and the dashboard, and refuses
  containers Previa did not create.

## Inventory and removal

- Every candidate carries labels: PR, generation, revision, commit, repository IDs,
  environment ID, image reference, port, readiness path (when HTTP), CPU and memory
  limits, start time. The router reads none of them; routes come from the route
  controller.
- `Runtime.Endpoint` re-checks a candidate right before it is routed: this PR's
  preview of exactly that generation, revision and image, running, and ready now (up
  to 5 probes a second apart from the egress container). A missing, foreign, stopped
  or unready candidate is refused permanently and never routed.
- `Runtime.Resources(pr)` lists the PR's previews paired with their images, images no
  container uses, builder containers and build networks left by crashed builds, and
  the PR's preview network once no preview uses it. The environments cleanup
  contract schedules removal of everything not referenced.
- `Runtime.RemoveResource` (the cleanup executor) inspects each object first and
  refuses anything whose labels do not name Previa and the resource's PR
  (`not_owned`); an object already gone counts as removed. Containers are stopped
  gracefully (10-second stop timeout) and then removed by force; images are removed
  without force after their container (`in_use` otherwise, retried); networks once no
  preview or builder uses them, after the router and egress proxy are disconnected.

## The router's path

The router joins each `previa-pr-<id>` network when the PR's first version is
published, as the egress proxy does, and dials the preview by container name on its
port. The control plane (server, worker, Postgres) never joins preview networks, so
the path into previews never becomes a path out of them. The router has no Docker
socket, and its API listens only on loopback inside its container. A preview can
reach the router's public entry points, and through them other previews' public URLs,
exactly as anyone on the internet can; the direct container-to-container path between
previews stays blocked ([12](12-routing-and-https.md)).

## Configuration

| Variable (worker) | Default | Meaning |
| --- | --- | --- |
| `EGRESS_IMAGE` | `previa-egress:dev` | Proxy and prober image for builds and previews (`make egress-image`). |
| `PREVIEW_ALLOW_ROOT`, `PREVIEW_WRITABLE_ROOTFS` | `false` | Server-wide compatibility opt-outs. |
| `PREVIEW_TMPFS_SIZE`, `PREVIEW_PIDS` | `256MiB`, `512` | Writable `/tmp` size and process limit. |
| `PREVIEW_STARTUP_TIMEOUT` | `120s` | Readiness deadline. |
| `PREVIEW_MEMORY_BUDGET`, `PREVIEW_CPU_BUDGET` | empty | All previews together; empty = 60% of Docker memory, 75% of its CPUs. |
| `PREVIEW_MAX_PER_REPOSITORY` | `10` | Running previews per repository. |
| `PREVIEW_OCI_RUNTIME` | empty | Docker runtime for previews, for example `runsc`. |

The worker validates these at start. Repository secrets need the same keyring on
the worker as on the server (`SECRETS_ENCRYPTION_KEYS`, `SECRETS_ACTIVE_KEY_VERSION`).
Without it, repositories without secrets still get previews and a repository with
secrets fails closed (`secrets_unavailable`) instead of starting without them.

```sh
make egress-image   # proxy + prober image (needed by builds and previews)
make test-runtime   # real previews: hardening, readiness, secrets, network, capacity (~3 min)
```

**Expected:** every `TestRealRuntime…` test passes and the network test logs the probe
table above.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/runtime/runtime.go` | `Runtime.Build`: recheck, build, recheck, secrets, start; interfaces for its parts. |
| `internal/runtime/candidate.go` | Starting a candidate: capacity under the admission lock, its containers, ordered start, readiness, discard on failure. |
| `internal/runtime/target.go`, `queries.sql` | The access, freshness and fork recheck. |
| `internal/runtime/hardening.go` | User policy, environment, the hardened container specification. |
| `internal/runtime/capacity.go`, `health.go` | Admission; readiness and observed health. |
| `internal/runtime/docker.go`, `inventory.go` | Preview network, output capture, inventory, owner-verified removal. |
| `internal/platform/egress` | Proxy, readiness probe and the gateway that runs `previa-egress`. |
| `internal/platform/dockerx` | Docker client, shared labels, exec helper. |

Unit tests cover the user policy, environment precedence, every field of the
container specification, admission, the recheck (stale, superseded, revoked, fork,
deleted head), crash-loop detection and the build orchestration's failure paths
(nothing decrypted or started after a failed recheck). A PostgreSQL test covers the
recheck query. `make test-runtime` runs the real checks above.
