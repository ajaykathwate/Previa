# 09 · Image builds

`internal/builds` turns a prepared exact-commit source ([06](06-source-and-configuration.md))
into a uniquely tagged Docker image. The Dockerfile belongs to the pull request's
author, so a build is untrusted code execution. This document describes how each build
is isolated, bounded and cleaned up, what was verified, and what remains a limit.
The builder is tested against a real Docker Engine. The preview runtime
([10](10-preview-runtime.md)) builds with it and starts containers from its images;
the worker does not dispatch builds until Prompt 10 ([21](21-roadmap.md)).

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Builder topology | **A fresh rootless BuildKit container per build**, removed afterwards | Resource limits cover every step, builds cannot affect each other, and a compromised build only reaches its own disposable builder. Costs a few seconds of startup. |
| Network for build steps | **Public internet through Previa's egress proxy**, ports 80/443 only, every private, local and metadata range refused | Most Dockerfiles need package registries; nothing internal is reachable. Tools that ignore proxy settings fail closed. |
| Cache scope | **Per base repository and source repository**: same-repository PRs share a cache, each fork gets its own | A fork's build cannot plant layers or cache-mount files that a trusted build (which later receives secrets) would reuse. This refines DESIGN §9's "per repo". |
| Limits (Balanced, env-overridable) | 2 CPUs, 4 GiB memory, 2,048 processes, 20 GiB disk, 4 GiB image, refuse to start below 15 GiB free | Owner decision; tune per host. |
| BuildKit versions | Daemon image `moby/buildkit:v0.33.1-rootless` pinned by digest; Go client v0.33.0 | Client v0.33.1 requires Go 1.26.8, but the toolchain is pinned to 1.26.5. Patch versions of client and daemon are compatible. |
| Egress proxy | Written in Go (`internal/platform/egress`, run as `previa-egress` by `cmd/egress`) rather than Squid; shared with previews ([10](10-preview-runtime.md)) | One language, exact address rules, unit-testable rebinding protection, no new third-party service. |
| Secrets | **Never** in builds: no build arguments, context or secret mounts | Repository secrets are runtime-only for the MVP ([07](07-repository-secrets.md)). |

## How a build runs

```
worker ──Docker API──► previa-build-<pr…>     internal network, created for this build
  │                        ├── builder: rootless BuildKit (state on the scope's cache volume)
  │                        └── previa-egress (connected for this build only)
  │                                 │  the only way out: ports 80/443, public addresses
  │                                 ▼
  │                              internet (registries, package mirrors)
  └──exec "buildctl dial-stdio"──► BuildKit API (Unix socket inside the builder; no port)
image ──docker-format tar, size-capped──► Docker image store ──► previa/pr-<id>:g<gen>-r<rev>-<sha12>
```

1. **Reuse.** If `previa/pr-<id>:g<gen>-r<rev>-<sha12>` already exists with this
   job's labels (an earlier attempt of the same revision finished it), it is returned.
2. **Sandbox.** Pull the pinned BuildKit image if Docker lacks it. Start the egress
   proxy if needed, create the build's internal network, connect the proxy to it,
   and start the builder. The builder takes the scope's cache volume if no other
   build holds it; otherwise it builds with a throwaway volume and says so in the log.
3. **Admission.** Refuse with `disk_low` (retryable) when the Docker host has less
   than `BUILD_MIN_FREE_DISK` free, measured inside the builder.
4. **Solve.** The Dockerfile frontend runs with the PR's tree as context. Proxy
   settings are predefined build arguments, which BuildKit keeps out of the image
   configuration and cache keys. Progress streams to the log in plain format. Every
   5 seconds BuildKit's disk usage is checked against `BUILD_DISK_BUDGET`.
5. **Export.** The image streams into Docker's store, failing past
   `BUILD_MAX_IMAGE_SIZE`; the stored image is then measured again. Oversized or
   partial images are removed.
6. **Cleanup, always.** The builder and its throwaway volume, and the network, are
   removed even after failure, timeout or cancellation. The cache volume stays for
   the next build of the scope. The caller releases the source workspace.

## Isolation, concretely

| Control | Builder container | Why it matters |
| --- | --- | --- |
| Network | Its own `internal` Docker network: no gateway, no route to the host, Docker's other networks or the internet | Docker network attachment is only the start: the only exit is the proxy below. |
| Egress proxy | Resolves names itself; refuses a destination if **any** address is private, loopback, link-local (incl. `169.254.169.254`), CGNAT, documentation, benchmark, multicast, reserved, or an IPv6 ULA/link-local/NAT64/6to4/Teredo range; ports 80/443 only; dials only the checked address | Stops access to Postgres, the server, other previews, host services and metadata, including through DNS rebinding. |
| Privileges | Not privileged; every capability dropped except `SETUID`/`SETGID` (needed by `newuidmap`); read-only root filesystem with owned tmpfs for runtime directories | A compromised step lands as an unprivileged user in a minimal container. |
| Profiles | Docker's seccomp and AppArmor profiles are disabled and `/proc` paths unmasked (the CLI's `systempaths=unconfined`) | Rootless BuildKit must create a user, mount and PID namespace per step; steps then run under BuildKit's own seccomp profile. This is the documented rootless trade-off and far narrower than a privileged builder. |
| Resources | cgroup limits on CPU, memory (no swap) and processes cover the daemon and every step; disk budget polled and enforced by stopping the build | A timeout alone is not resource isolation. |
| Host access | No Docker socket, no host paths, no published ports, no Previa credentials or repository secrets | Nothing in the builder can act on the host or on GitHub. |

**Verified** on Docker Desktop 29.8 (WSL2) by `make test-builder`, from inside a build step:

| Probe | Result |
| --- | --- |
| Internet bypassing the proxy (`wget -Y off http://example.com`) | blocked |
| A service on Docker's default bridge (stand-in for Postgres, the server, previews), direct and through the proxy | blocked |
| `host.docker.internal`, direct and through the proxy | blocked |
| `169.254.169.254` metadata, direct and through the proxy | blocked |
| Docker socket present | no |
| Credential-like environment variables | none |
| `http://example.com` through the proxy | reached |

Also verified: a step exceeding a 256 MiB memory limit is killed and reported; a
40 MB image is refused under a 16 MiB cap; a build writing 400 MB stops under a
256 MiB disk budget; timeouts and cancellation stop the build and leave no builder or
network; 100,000 lines of output do not stall the build; a malformed Dockerfile and a
failing step are reported with their reason; a fork's build does not reuse the trusted
scope's cache; and two simultaneous builds of one scope never share a cache.

**Limits, stated plainly.** Builders are containers on the shared host kernel, not
virtual machines; a kernel exploit is not contained (gVisor or VM isolation is an
optional upgrade). The outer seccomp and AppArmor profiles are off for the builder (see
above). Code can still exfiltrate anything it is given over HTTPS to a public host;
builds are given no secrets. Tools that ignore `HTTP(S)_PROXY` cannot reach the
network. The egress proxy is shared by concurrent builds, but it only connects to
public addresses, so builds cannot reach each other through it. Each build network
takes an address range from Docker's default pools until it is removed.

## Identity, retries and cleanup

- Image: `previa/pr-<pr id>:g<generation>-r<revision>-<12-char sha>`, labelled
  `io.previa.managed`, `pull-request-id`, `generation`, `revision`, `head-sha` and the
  repository IDs. A same-SHA Retry/Rebuild is a new revision and a new image.
- Builders, build networks and images carry the same labels.
  `Builder.Resources(pr)` lists them (including crash leftovers) and `Remove` deletes
  one idempotently: an artifact that is already gone counts as removed. These plug
  into the environments cleanup contract.
- A builder holding a cache volume longer than `BUILD_TIMEOUT` + 15 minutes belongs
  to a crashed worker; the next build of that scope removes it. Cache volumes
  (`previa-buildcache-<instance>-r<base>-h<head>`) are trimmed by BuildKit's GC to a
  quarter of the disk budget; housekeeping removes those of repositories that can no
  longer be built and the oldest unused ones beyond `BUILD_CACHE_BUDGET`, and untagged
  images ([14](14-housekeeping-and-capacity.md)).
- Every object carries this database's instance (`io.previa.instance`) in its labels
  and names (`previa/<instance>-pr-<n>:…` images, `previa-builder-<instance>-…`), and
  the builder only reuses or removes objects of its own instance.
- Before building, the builder requires `BUILD_MIN_FREE_DISK` free plus
  `BUILD_DISK_BUDGET` for every other running build of its instance; a shortage
  (`disk_low`) makes the request wait for disk space rather than fail.
- Failures are typed (`builds.Error`): `invalid_job`, `builder_unavailable` and
  `disk_low` (retryable), `build_failed`, `timeout`, `cancelled`, `disk_budget`,
  `image_too_large`, `superseded`. Messages are safe to show the PR owner; the log
  holds the failing step.

## Prerequisites and configuration

- A Docker Engine (Docker Desktop works) that the worker reaches through the
  standard `DOCKER_HOST` environment, user namespaces enabled (the default on current
  kernels), and internet access for Docker to pull the BuildKit image and for builds.
- The egress proxy image, built locally: `make egress-image` produces
  `previa-egress:dev` (shared with previews).

| Variable (worker) | Default | Meaning |
| --- | --- | --- |
| `BUILDKIT_IMAGE` | pinned `moby/buildkit:v0.33.1-rootless@sha256:f8a8…57b4` | Builder image. |
| `EGRESS_IMAGE` | `previa-egress:dev` | Egress proxy image, shared with previews. |
| `BUILD_CPUS`, `BUILD_MEMORY`, `BUILD_PIDS` | `2`, `4GiB`, `2048` | Per-build limits. |
| `BUILD_DISK_BUDGET`, `BUILD_MAX_IMAGE_SIZE`, `BUILD_MIN_FREE_DISK` | `20GiB`, `4GiB`, `15GiB` | Disk and image bounds; sizes take `KiB`, `MiB`, `GiB`, `TiB`. |

```sh
make egress-image         # static proxy binary in an empty, non-root image
make test-builder         # real builds, isolation probes, limits, caching, cleanup (~4 min)
```

**Expected:** every `TestRealBuilder…` test passes, the isolation test logs the probe
table above, and afterwards only `previa-egress` (container and network) remains.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/builds/builder.go` | `Builder.Build`: reuse, sandbox, admission, solve, export, failure classes. |
| `internal/builds/sandbox.go`, `dial.go` | Builder and network lifecycle, cache lock, readiness, free disk; the exec connection. |
| `internal/builds/image.go`, `labels.go` | Size-capped export, reuse, inventory and removal; names and labels. |
| `internal/platform/egress`, `cmd/egress`, `deploy/egress/Dockerfile` | The egress proxy, its gateway and its image (shared with previews). |
| `internal/builds/logsink.go` | Progress into the log sink ([08](08-build-logs.md)). |
| `internal/platform/dockerx` | Docker client construction. |

Unit tests (`go test -race ./internal/builds`) cover every address range, the proxy
end to end against local servers (allowed, denied, mixed DNS answers, ports,
hop-by-hop headers, and that no unchecked address is ever dialled), job validation,
names, cache scopes, the export cap and failure classification. The real tests above
run only with `make test-builder`.
