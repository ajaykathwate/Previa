# 11 · Backend previews

Backend and API repositories preview as well as web frontends: a commit can ask
for platform-managed **PostgreSQL** and **Redis**, a **setup command** (migrations,
seed data), a few unrouted **processes** (a queue worker) and a **docs path**, and
its owner can approve **outbound connections** to an external database or message
broker. This narrows README §2.5's exclusion of multi-service apps: services come
only from the operator's pinned catalog, never from images chosen in a PR, and
arbitrary Compose files, custom service images, buildpacks, data that survives a
rebuild, copies of production databases and non-HTTP ingress stay out of scope.

Everything here builds on the preview runtime ([10](10-preview-runtime.md)): the
same hardening, the same blue/green publication and fences
([12](12-routing-and-https.md), [18](18-durable-publication-design.md)), the same
teardown and cleanup contract ([13](13-teardown-and-cleanup.md)) and the same
housekeeping ([15](15-housekeeping-and-capacity.md)). There is no second
orchestrator.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Which services | PostgreSQL and Redis only, at versions in `PREVIEW_SERVICE_CATALOG`, each image **pinned by digest**; empty uses Previa's pinned catalog (PostgreSQL 17/16, Redis 8/7), `none` offers none | A PR chooses a version, never an image; the operator decides what runs. |
| Ownership | Every service belongs to its **candidate**: each build revision gets its own, with credentials generated for it | A failed rebuild cannot touch the serving preview's data; nothing is shared between previews. |
| Data | On a size-capped **tmpfs** inside the service's memory limit; **data does not survive a rebuild** | Bounded storage that cannot fill the host disk, and no stale state between commits. |
| Network | A per-PR **services network** (internal) joined only by the PR's candidates and their services: no egress proxy, no router | Services reach nothing outside their candidate, and nothing else reaches them. *(The roadmap said "the PR's internal network"; a separate network was chosen so a service can never use the egress proxy. Flagged deviation.)* |
| Readiness order | Services pass a protocol check → setup succeeds → processes run → the app passes its readiness check → only then may the route be accepted | A preview is published only when its whole stack works. |
| Outbound connections | Owner-approved `host:port` list per repository on the **Outbound** tab; ports limited by `PREVIEW_OUTBOUND_PORTS` (`none` turns it off); forks get none | External test databases work without opening previews to the internet. |

## `.previewrc.yaml`

```yaml
port: 8080
health_check_path: /health
services:
  postgres: 17          # a version the operator's catalog offers
  redis: 7
setup: ["bin/api", "migrate", "--seed"]   # argv; runs once per candidate
processes:              # at most 3, unrouted, from the same image
  - name: worker
    command: ["bin/api", "worker"]
    resource_tier: small  # optional, default small
docs_path: /docs        # a path on the preview's own host
```

| Key | Type | Rules |
| --- | --- | --- |
| `services` | mapping of `postgres`/`redis` to a version (`17`, `"17"` or `7.4`) | Each kind once; the version must be in the catalog. An unknown service or version fails preparation with a message naming what this server offers. |
| `setup` | list of strings (argv) | Program first; at most 64 arguments and 4 KiB; no NUL bytes. It **replaces the image's entrypoint and command**; no shell parses it (use `["sh", "-c", "…"]` for one). |
| `processes` | list of `{name, command, resource_tier}` | At most 3; names `^[a-z][a-z0-9-]{0,30}$`, unique, not `app`, `setup`, `postgres` or `redis`; command as for `setup`; tier default `small`, clamped like the app's. |
| `docs_path` | string | An absolute path on the preview, validated like `health_check_path`; never an external URL. |

The effective configuration hash covers services (with their pinned images),
setup, processes and the docs path; without these keys it is unchanged.

### Examples

```yaml
# Node.js (Express + Prisma), BullMQ worker
port: 3000
health_check_path: /healthz
services: { postgres: 17, redis: 7 }
setup: ["npx", "prisma", "migrate", "deploy"]
processes:
  - name: queue
    command: ["node", "dist/worker.js"]
```

```yaml
# Ruby on Rails with Sidekiq
port: 3000
services: { postgres: 16, redis: 7 }
setup: ["bin/rails", "db:prepare"]
processes:
  - name: sidekiq
    command: ["bundle", "exec", "sidekiq"]
env:
  - RAILS_ENV=production
  - RAILS_LOG_TO_STDOUT=1
```

```yaml
# Django with Celery
port: 8000
health_check_path: /health/
services: { postgres: 17, redis: 8 }
setup: ["sh", "-c", "python manage.py migrate && python manage.py loaddata seed.json"]
processes:
  - name: celery
    command: ["celery", "-A", "project", "worker", "--loglevel=info"]
docs_path: /api/docs/
```

```yaml
# Go API with goose migrations
port: 8080
health_check_path: /readyz
services: { postgres: 17 }
setup: ["/app/migrate", "up"]
docs_path: /swagger/
```

## Connection settings

Previa sets, for the app, the setup container and every process:

| Service | Variables |
| --- | --- |
| PostgreSQL | `DATABASE_URL` (`postgres://preview:…@postgres-<id>:5432/preview?sslmode=disable`), `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `PGSSLMODE` |
| Redis | `REDIS_URL` (`redis://:…@redis-<id>:6379/0`) |

Previa's values **override** `.previewrc.yaml` and repository secrets; the build
log says which names were replaced. Passwords are 32 random characters generated
per candidate, registered with the log redactor before the first line is written,
and kept only in memory and in the containers' configuration: never in
PostgreSQL, job payloads, labels, metrics or routine errors. (Anyone with access
to the Docker host can read them with `docker inspect`, as with secrets.)

## How a backend candidate starts

1. The commit is prepared and built as usual ([06](06-source-and-configuration.md), [09](09-image-builds.md)).
2. Pinned service images are pulled if missing, **before** the admission lock.
3. Under the admission lock, the candidate's total (app, processes, services and
   the setup container) must fit the preview budget; every container is created,
   labelled and counted, then the lock is released. Nothing has started yet.
4. **Services** start and must answer their protocol from inside their container:
   PostgreSQL an authenticated `SELECT 1`, Redis an authenticated `PING` (a TCP
   accept is not enough), within `PREVIEW_STARTUP_TIMEOUT`.
5. **Setup** runs once, as a one-off container with the app's hardening, within
   `PREVIEW_SETUP_TIMEOUT`; its output streams into the build log. A non-zero exit
   or the timeout fails the build with `setup_failed`; the setup container is
   removed.
6. **Processes** start and must still be running after two seconds.
7. The **app** starts and passes its readiness check ([10](10-preview-runtime.md)).
8. Processes must still run and services still answer; then what the candidate
   runs is recorded (`preview_components`) and it is handed to publication.
9. Right before the route is accepted, `Runtime.Endpoint` checks the app and again
   that processes run and services answer (credential-free protocol checks).

Any failure removes **every** container of the candidate after copying the failing
part's last output into the build log. A failed rebuild therefore leaves the old
preview and **its** data serving.

## Isolation and hardening

| Control | Managed services |
| --- | --- |
| Image | The catalog's image, pinned by digest; never chosen by a PR |
| User | The official image's own unprivileged user (`postgres`, `redis`), not root |
| Filesystem | Read-only root; data, `/tmp` and PostgreSQL's socket directory on tmpfs (`nosuid,nodev`); every volume the image declares becomes a tmpfs, so Docker creates no volume |
| Privileges | Every capability dropped, `no-new-privileges`, Docker's default seccomp/AppArmor, `PREVIEW_OCI_RUNTIME` applies |
| Network | Only the PR's services network, reachable by a per-candidate hostname; no egress, no route, no published port; IPv6 off |
| Resources | PostgreSQL 0.5 CPU / 512 MiB (192 MiB of data), Redis 0.25 CPU / 256 MiB (`maxmemory 160mb`, no persistence); 256 processes; half CPU weight; OOM-killed first; logs capped; never restarted |

**Exceptions, stated plainly:** services keep their image's user rather than UID
65532 (the official images need their own user for their data directory);
PostgreSQL runs with `fsync=off` because its data never outlives the candidate;
`PREVIEW_ALLOW_ROOT` and `PREVIEW_WRITABLE_ROOTFS` do not apply to services. The
operator controls only the catalog (which images, by digest) and the setup
timeout. An image that is not the official PostgreSQL or Redis image may not work
under these rules.

The setup container and processes are the app's specification with another
command: the same user policy, read-only root, tmpfs, capabilities, networks and
environment, with their own limits (setup uses the app's tier).

## Approved outbound connections

The repository's **Outbound** tab (owner only, CSRF-protected forms, removal
confirmed in a modal) lists `host:port` destinations its previews may reach, at
most 10, one per port, on the ports `PREVIEW_OUTBOUND_PORTS` allows (default
`5432,3306,6379,6380,27017,5671,5672,9092,1433`; `none` turns the feature off and
the tab says so). Repository YAML can never set them.

How it works:

- The egress proxy listens on the allowed ports on every preview network. A
  candidate of a **trusted** (same-repository) pull request gets `/etc/hosts`
  entries mapping each approved name to the proxy's address on its network, and
  the worker records the approvals for that network on the proxy.
- The proxy identifies the repository by **the network the connection arrived
  on** (its own address there), never by anything the preview sends. It resolves
  the name itself, refuses private, loopback, link-local, metadata and other
  special ranges, dials exactly the address it checked and relays the bytes, so
  **TLS verification works end to end** with the real name.
- Bounds: 32 connections per preview network and 256 in all; each at most an hour,
  1 GiB, and 10 idle minutes. A withdrawn approval closes open connections within
  30 seconds.
- A refresh job (`outbound_refresh`, every minute and after every change) rewrites
  every network's approvals, so removals take effect within seconds and a
  restarted proxy is restored. New approvals reach previews started afterwards.
- **Forks get none**, the same rule as secrets; the PR page says so.
- Metrics: `previa_egress_connections_total{outcome}` and
  `previa_egress_bytes_total{direction}`; every decision is logged by the proxy.

Caveats: apps must resolve names through the system resolver (`/etc/hosts`); a
client with its own DNS implementation is not redirected. Name-based approvals
only; IP addresses are refused. A connection idle for ten minutes closes.

## Lifecycle, cleanup and crashes

A candidate is one resource in every existing path: its app's artifact
(container and image) stands for its services, setup and processes, which carry
the candidate's key (`io.previa.candidate`) and are counted, referenced, kept and
removed with it.

| Path | Behaviour |
| --- | --- |
| Blue/green | The new candidate brings its own services; the old preview and its data keep serving until the swap; the retired candidate's cleanup removes its services too. |
| Teardown, expiry | The route goes first; cleanup removes the app, processes, setup and services, then the image and the idle preview and services networks. |
| Recovery, inventory | `Runtime.Resources` lists each candidate once, including companions left before their app existed; unreferenced ones are cleaned after the grace period. |
| Pruning | Service images are the catalog's and never pruned by Previa; built images as before. |
| Crash | Whatever exists is labelled: a crash after any side effect leaves nothing owned behind once housekeeping runs. |

## What people see

- **Build log:** pulls, each service becoming ready (with its hostname), the setup
  command's output, processes starting, replaced variable names and outbound
  connections given or withheld.
- **PR page:** *Runs with* (services, setup, processes; "Data does not survive a
  rebuild"), *Docs*, *Outbound*, and *Managed services* offered by the running
  worker, labelled "None: no service catalog is configured" when there is none.
- **PR comment:** the docs link and "Runs with PostgreSQL 17 and Redis 7 · setup ran ·
  process worker" for a ready preview. Failures say which part failed:
  `service_unavailable`, `service_failed`, `setup_failed`, `process_failed`.

## Configuration

| Variable | Process | Default | Meaning |
| --- | --- | --- | --- |
| `PREVIEW_SERVICE_CATALOG` | worker | empty = Previa's pinned catalog | `kind:version=image@sha256:digest` pairs; `none` offers no services |
| `PREVIEW_SETUP_TIMEOUT` | worker | `5m` | Setup command limit, 10s–30m |
| `PREVIEW_OUTBOUND_PORTS` | server, worker | `5432,3306,6379,6380,27017,5671,5672,9092,1433` | Ports owners may approve, at most 16, never 80/443/3128; `none` turns the feature off |

Changing `EGRESS_IMAGE` (an upgrade) makes the worker replace `previa-egress` and
join it to the same networks again; previews started before then reach the proxy
again only after a rebuild.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/repoconfig` | The new keys: shape, catalog and command validation, clamping, hash |
| `internal/runtime/backend.go`, `runtime.go` | Plan, credentials, specs, readiness order, setup, processes, outbound |
| `internal/runtime/inventory.go`, `cleanup.go`, `endpoint.go`, `capacity.go` | Candidate grouping, removal, publication check, capacity |
| `internal/outbound` | Approvals: rules, store, Outbound tab, refresh job |
| `internal/platform/egress/forward.go`, `gateway.go` | TCP forwarding, allow files, ports, counters, replacement on upgrade |
| `web/pages/outbound`, `web/pages/pullrequests` | The Outbound tab and the PR page's backend facts |

Unit tests cover the YAML contract, catalog and port settings, the forwarder
(approved, unapproved, private, rebinding, limits, revocation, byte cap), the
specs and environment precedence; PostgreSQL tests cover approvals (owner
scoping, limit, one per port, the worker's view, wake-ups), the components record
and the PR comment; a browser test covers the Outbound tab and its modal.
`make test-e2e` runs `TestRealBackendEndToEnd` on real Docker, BuildKit and
Traefik with an HTTP API on PostgreSQL and Redis, a migration-and-seed setup
command and a queue worker (see the results in [26](26-release-report.md)).
