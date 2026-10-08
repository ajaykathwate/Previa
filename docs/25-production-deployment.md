# 25 · Production deployment

Use **one DigitalOcean Droplet running Ubuntu 24.04, Docker Engine and Docker
Compose**, **PostgreSQL 17 in Docker on that Droplet**, **DigitalOcean Spaces for
encrypted off-server backups**, and **Cloudflare DNS** for the dashboard and
preview domains. The Go server, Go worker, Traefik, Prometheus and Grafana run on
the Droplet. The worker creates build containers and live PR preview containers
on that same Docker host. This is the production arrangement described throughout
this guide; infrastructure has not been provisioned by writing these instructions.

Rent the Droplet once. **A new PR creates Docker containers, not a new Droplet
or a separate VM.** The application runs unattended after SSH disconnects.
Spaces holds backup files; the live PostgreSQL database stays on the Droplet.

This follows [README §4.9](../README.md#49-infrastructure-and-deployment),
[DESIGN](../DESIGN.md), and the actual files in `deploy/production`. It describes
what exists, the manual steps an operator must perform, and the additions needed
if you choose a different database or automated deployment. **A commit to `main`
does not deploy Previa. A release tag publishes artifacts; the operator deploys
them.**

The [release report](26-release-report.md) records local evidence and pending live
checks. It does not establish that a real production host is already running.
Examples below use reserved example domains and illustrative version tags;
replace them with your domains and an existing, tested release. Run commands one
step at a time and stop when a check fails.

## Contents

- [Deployment platform and external services](#deployment-platform-and-external-services)
- [DigitalOcean account, Droplet, firewall and SSH setup](#fresh-host-setup)
- [What runs](#what-runs)
- [First start, with staged certificates](#first-start-with-staged-certificates)
- [Verify the deployment](#verify-the-deployment)
- [Monitor live traffic, jobs and containers](#monitor-live-traffic-jobs-and-containers)
- [DigitalOcean host monitoring](#configure-digitalocean-host-monitoring)
- [DigitalOcean public uptime checks](#configure-digitalocean-uptime-for-external-https-and-certificate-checks)
- [Backups and restore](#backups-and-restore)
- [Droplet backups and snapshots](#step-b7--optional-digitalocean-droplet-backups-and-snapshots)
- [Upgrades and rollback](#upgrades-and-rollback)
- [Droplet resizing and ongoing operations](#digitalocean-capacity-billing-and-ongoing-operations)
- [CI and releases](#ci-and-releases)
- [Local staging smoke test](#local-staging-smoke-test)
- [Security notes](#security-notes)
- [Files behind these instructions](#files-behind-these-instructions)

For the first deployment, complete Steps 0–6, including backup-key/Spaces setup
B1–B3; then run Steps 7–10. B4 needs the running PostgreSQL/router and makes the
first actual backup. Configure host/Uptime/application notifications, rehearse
B5/B6 on isolated staging, then accept live traffic. Later releases use U1–U5
and the rollback paths, rather than repeating the first-time certificate reset.

## Deployment platform and external services

### The deployment to use for this implementation

The selected arrangement is **DigitalOcean Droplet + Docker Compose + local
PostgreSQL + DigitalOcean Spaces**, with Cloudflare as authoritative DNS.
DigitalOcean supplies the Linux server and backup storage; Previa manages Docker
previews. This preserves the single-VPS design in README/DESIGN and uses the
existing production scripts without adding a new runtime.

| Piece | Where it lives | Why it is needed |
| --- | --- | --- |
| VPS | One DigitalOcean Droplet dedicated to Previa | Provides a persistent Linux host, Docker socket, SSD, private Docker networks, and public ports 80/443. Dedicated to Previa means do not colocate unrelated sensitive workloads; choose shared or dedicated provider CPUs according to measured load. |
| Production PostgreSQL | `postgres` container on the VPS; Docker volume `previa_postgres` | Stores application state, sessions, encrypted repository secrets, build logs, durable operations, and River jobs. |
| Dashboard and API | `server` container on the VPS | Serves the UI, sign-in, repository controls, signed webhooks and live updates. |
| Background processing | `worker` container on the VPS | Runs River queues and creates builds, previews, routes, reports and cleanup. |
| Public routing and certificates | `router` container on the VPS | Traefik terminates HTTPS and forwards each hostname to its correct backend. |
| DNS | Cloudflare account and authoritative zones | Dashboard and wildcard preview records point to the VPS; the router uses a scoped token for DNS-01 certificate challenges. |
| GitHub integration | Separate GitHub OAuth App and GitHub App | OAuth signs people in; the App grants repository access, receives webhooks and posts PR feedback. |
| Release artifacts | GitHub Releases and GitHub Container Registry (GHCR) | Stores versioned binaries and the three Previa images. GitHub runners build them, then finish. |
| Backup destination | Private DigitalOcean Spaces Standard bucket, outside the Droplet | Holds encrypted database, configuration and certificate archives through the S3-compatible API; survives loss of the Droplet. |
| Monitoring | Prometheus and Grafana on the VPS | Scrapes metrics, evaluates alert rules and provides the Previa dashboard. |
| Host monitoring | DigitalOcean metrics agent on the Droplet; graphs/resource alerts in the control panel | Tracks overall server CPU, memory and root-disk use, independently of Previa's application metrics. |
| Public uptime | DigitalOcean Uptime check and configured notification | Checks HTTPS from outside the Droplet, including when monitoring containers on that Droplet are unavailable. |
| Alert delivery | Operator-configured Alertmanager and receiver | Sends firing alerts to your email, chat or incident service; this is not supplied as a running service by the base Compose file. |
| Backup encryption key | Private age identity in offline recovery storage | Decrypts backups during a restore; only its public recipient is needed on the VPS. |

There is no separate frontend deployment: templates and static browser assets are
compiled/generated and embedded into `previa-server`. Production does not need a
Node server, Vercel, Netlify or Air. There is no Redis service for Previa's queue:
River uses the same PostgreSQL database. There is no Kubernetes cluster, remote
BuildKit service, or production Terraform configuration in the repository.

Provider references: [Droplets](https://docs.digitalocean.com/products/droplets/),
[Droplet creation](https://docs.digitalocean.com/products/droplets/how-to/create/)
and [Spaces](https://docs.digitalocean.com/products/spaces/). Choose current
resources and location rather than copying an old SKU or price. Budget for
Droplet compute, storage, backup storage/transfer, optional provider backups,
uptime checks, domains and alert receivers. Containers share the rented host;
they consume its capacity rather than creating a per-PR DigitalOcean VM bill.

### Docker previews, Droplets and the deployment boundary

A **Droplet is the one rented Linux VM**. A **Docker image** packages the app,
and a **Docker container** runs that image as isolated processes sharing the
Droplet's kernel. Previa creates one serving app container per PR, plus temporary
builders and replacement candidates. The total can briefly exceed the number
of active PRs during builds and swaps.

The production worker controls the Droplet's Docker daemon through its socket.
Preview containers are siblings of server/worker/PostgreSQL, not children nested
inside the worker container. They receive separate private networks and limits;
only Traefik publishes HTTP/HTTPS. All visits to a preview consume this Droplet's
CPU/network resources.

Use DigitalOcean **Droplets** for this guide. App Platform, Kubernetes, a
DigitalOcean Container Registry, load balancer and a VM per preview are not
parts of the supplied deployment. A GitHub commit does not trigger App Platform
deployment here. Published platform images remain in GHCR; built PR images
remain on this Docker host. CPU/RAM growth requires resizing the host or a
separately implemented multi-host architecture.

### Why use PostgreSQL on the Droplet? What about Managed PostgreSQL?

The chosen production DB is the Compose **PostgreSQL 17 service**, not a
DigitalOcean Managed Database cluster. Compose starts it, migrations target it,
and the shipped backup/restore scripts operate on it. DigitalOcean hosts the
Droplet, while you maintain PostgreSQL updates, storage, backups and recovery.
Do not create a managed database just to follow these commands.

DigitalOcean also offers [Managed PostgreSQL](https://docs.digitalocean.com/products/databases/postgresql/).
It can reduce database operations later, but adopting it needs a separately
implemented and tested deployment change. Changing `DATABASE_URL` alone leaves
the current Compose dependencies and scripts targeting the local DB.

If choosing that future architecture, the required work is:

1. Create a compatible cluster close to the Droplet, preferably using its
   private/VPC connection. Configure database/user access and trusted sources,
   download the provider CA, and verify TLS/hostname checks from server,
   worker and migration containers. See [connection setup](https://docs.digitalocean.com/products/databases/postgresql/how-to/connect/)
   and [trusted-source restrictions](https://docs.digitalocean.com/products/databases/postgresql/how-to/secure/).
2. Verify migrations, River, connection lifetimes, advisory locks and PostgreSQL
   `LISTEN/NOTIFY` on the chosen endpoint. The server uses a dedicated LISTEN
   connection. Do not assume a transaction-pooling URL provides session semantics;
   choose an appropriate direct or session-compatible connection and test it.
3. Change Compose's local `postgres` dependencies and the migration lifecycle.
   Otherwise it still creates and waits for an unused local database.
4. Replace the database portions of backup and restore. `backup.sh` explicitly
   dumps `dc exec postgres ... -d previa`; `restore.sh` explicitly drops and
   recreates that local database. They do not follow an external `DATABASE_URL`.
5. Define the provider's backup retention, restore/PITR procedure, database roles,
   upgrade compatibility, recovery objective and monitoring. Continue backing up
   `/etc/previa` and ACME state separately; a managed DB backup contains neither.
6. Rehearse a complete restore and a rollback before switching live traffic.

Those changes are not implemented by this documentation update. A Managed
Database cluster is a separate resource, with a separate bill and recovery
procedure; deleting/recreating the Droplet does not perform a managed-DB restore.
For the commands below, use the provided **PostgreSQL 17 Compose service**.
PostgreSQL for each *previewed application* is a different matter: automatic per-PR databases,
Redis, setup commands and extra app processes remain planned features in the
[roadmap](27-roadmap.md). Previa's own production database is never a database
that an untrusted preview may connect to.

### What to create, and where to configure it

| Item | Exact configuration location | How it connects to Previa |
| --- | --- | --- |
| Project, Droplet, region, size and SSH public key | DigitalOcean control panel | Supplies the host; source/scripts are installed at `/opt/previa` over SSH. |
| Cloud Firewall | DigitalOcean → Networking → Firewalls | Apply inbound SSH/80/443 rules to the Droplet. This is separate from UFW and Docker's per-PR isolation. |
| Optional Reserved IP | DigitalOcean → Networking → Reserved IPs | If used, dashboard/wildcard DNS targets this address; reassign it deliberately during recovery. |
| Host monitoring and resource alerts | Droplet creation option and DigitalOcean → Monitoring | Agent sends host metrics; configure actual notification recipients. |
| External HTTPS checks | DigitalOcean → Uptime | Checks public dashboard and a maintained preview outside the host. |
| Private backup bucket and access key | DigitalOcean → Spaces Object Storage | Root's `rclone` uploads encrypted archives using a key scoped to the bucket. |
| Dashboard and wildcard preview records | Cloudflare authoritative zones | Both resolve to the chosen public/Reserved IPv4. |
| DNS certificate credential | Cloudflare token settings, then `/etc/previa/dns-credential` on the Droplet | Traefik mounts it as a file for DNS-01. |
| OAuth App and GitHub App | GitHub → Settings → Developer settings | Their IDs/secrets go in `/etc/previa/previa.env`; App PEM goes in `/etc/previa/github-app.pem`. |
| Application/release/runtime settings | `/etc/previa/previa.env` on the Droplet | Compose reads this explicitly; there is no Droplet application-env panel to populate. |
| Backup storage key/secret | Root's protected rclone configuration on the Droplet | Not Go application settings; retain a recovery copy separately. |
| Backup private age identity | Offline recovery storage | Temporarily brought to a recovery host only when decrypting. |
| User repositories' preview secrets | Previa → repository → Secrets | Encrypted in PostgreSQL; distinct from the platform's production credentials. |
| Release environment and package access | GitHub repository Settings / Actions / Packages | Protects artifact publication; does not provision or update the Droplet. |

The manual control-panel route requires no DigitalOcean API token in Previa or
GitHub Actions. Spaces keys, your SSH private key, the Cloudflare DNS token and
GitHub App credentials have different purposes and are not interchangeable.
Only a future DigitalOcean API/`doctl` automation needs a scoped provider token.

## What runs

### Traffic and execution paths

```mermaid
flowchart TD
    User[Users and GitHub webhooks] -->|HTTPS 443| Router[Traefik on the VPS]
    Router -->|dashboard hostname| Server[Go server: dashboard and webhooks]
    Reviewer[PR reviewer] -->|preview hostname, HTTPS 443| Router
    Router -->|PR private network| Preview[Live app preview container]
    Server --> DB[(PostgreSQL: state, sessions, logs, River)]
    Worker[Go worker] --> DB
    Worker -->|host Docker socket| Engine[Docker Engine on the VPS]
    Engine --> Builder[Temporary rootless BuildKit containers]
    Engine --> Preview
    Engine --> Egress[Shared Previa egress proxy]
    Worker -->|accepted route file| Router
    Worker -->|installation token| GitHub[GitHub repository and PR APIs]
    Prom[Prometheus] -.->|private metrics| Server
    Prom -.->|private metrics| Worker
    Prom -.->|private metrics| Router
    Grafana[Grafana through SSH tunnel] --> Prom
    Timer[Host systemd backup timer] --> Backup[pg_dump, config and ACME encrypted with age]
    Backup --> Storage[Private off-host object storage]
```

A dashboard request passes through Traefik to the Go server. A request to a
published preview passes through Traefik directly to that app's container; the Go
server and worker do not sit in that preview request path. A webhook records
state/authorized work in PostgreSQL and returns; River executes the work later.

### Compose services

All these services run on the **same DigitalOcean Droplet**. Third-party
infrastructure images in the current Compose file are pinned by digest; the
version names below identify them. Keep those pinned references. Previa's own
images use the selected release tag; record their resolved digests during an
upgrade so the exact previous artifacts can be identified.

| Service | Image / executable | Lifetime and responsibilities | Public exposure |
| --- | --- | --- | --- |
| `postgres` | `postgres:17.11-alpine` | Persistent PostgreSQL 17 database; healthchecked; stores data in `previa_postgres`. | No host port; `postgres:5432` on the control network only. |
| `migrate` | Previa server image, `previa-migrate up` | One-shot Goose migrations followed by River migrations. Exit 0 is success. Server/worker wait for it. | None. |
| `server` | `previa-server:<release>` | Persistent Go HTTP process. Has no Docker socket. Read-only root, UID 65532, temporary `/tmp`. | Internal HTTP `8080`; reached publicly through Traefik. Health/metrics `9090` stay private. |
| `worker` | `previa-worker:<release>` | Persistent Go River worker, not an HTTP app serving users. Only service with the Docker socket; has source and routing storage. | Health/metrics `9091` stay private. |
| `router-config` | Pinned Traefik image, shell command | One-shot writes `previa-dashboard.yml` before the router starts. Exit 0 is success. | None; no network. |
| `router` | `traefik:v3.7.13`, container `previa-router` | Persistent HTTPS proxy; watches dashboard and preview route files; renews certificates. No Docker socket. | Host TCP `80` and `443`; metrics `8082` on control network; API/ping `8080` on its own loopback. |
| `egress` | `previa-egress:<release>` | Image staging service under profile `images`. Use it to **pull or build the image**, not to run the production proxy. | Do not start this Compose service. |
| `prometheus` | `prom/prometheus:v3.5.0` | Optional `observability` profile; scrape/evaluation every 30 seconds, 30-day local metrics retention. | `prometheus:9090` private; no host port. |
| `grafana` | `grafana/grafana:12.1.1` | Optional `observability` profile; provisioned Prometheus datasource and Previa dashboard. | Host `127.0.0.1:3000`, accessed over SSH. |

The `server`, `worker` and `egress` release images come from the multi-stage
[Dockerfile](../Dockerfile). Runtime images contain static Go binaries; server and
worker use distroless images without a shell. Use their `healthcheck` and
`check-config` commands, not `docker exec ... sh`. The server image also contains
`previa-migrate` and `previa-secrets-reencrypt`; the worker image contains
`previa-route-sync` and `previa-housekeeping`. These maintenance commands are
run on demand, not deployed as additional always-running processes.

### Containers created by the worker

These are **sibling containers on the host's Docker daemon**, not nested
containers inside `worker`. They do not appear in `dc ps`, because Compose did
not create them. Use `docker ps` and their labels to inspect them.

| Object | Created by / lifetime | What it does |
| --- | --- | --- |
| `previa-egress` container and network | Worker startup; shared and persistent | Runs the Previa proxy and readiness prober. Attached to build/preview networks; permits proxy traffic to public addresses and blocks private/control-plane/metadata addresses. Requires its image to be downloaded beforehand. |
| Rootless BuildKit builder | A build attempt; removed afterwards | Fetches public base images/dependencies through the proxy, executes Dockerfile steps, builds the preview image. Its cache may persist in a labelled Docker volume. |
| Build network | A build attempt; removed afterwards | Internal bridge network containing the builder and attached egress proxy. |
| Preview image | A successful Dockerfile build | Kept in the VPS's Docker image store, not uploaded to GHCR by the PR build pipeline. |
| App preview container | A successful build; lives until replacement, teardown or expiry | Runs the app with bounded CPU/memory/PIDs, its configured internal port and appropriate repository secrets. No Docker socket or published host ports. |
| PR preview network | That PR's preview lifecycle | Internal bridge shared by that PR's candidates, Traefik and the egress proxy. Other PRs and the control plane are not attached. |

The preview config `.previewrc.yaml` points at a Dockerfile and the app's internal
port. It is not a file for deploying Previa's own control plane. A preview must
pass readiness before a route is published. A successful rebuild switches the
accepted route to the new candidate; a failed rebuild leaves the old serving
preview in place. Close/merge and explicit teardown remove routes/resources
through durable operations. Expiry and housekeeping provide additional cleanup.

### Persistent storage and networks

| Location | Contents | Included in the shipped backup? |
| --- | --- | --- |
| `/opt/previa` | Checkout of the exact release; scripts, Compose and monitoring definitions | Recover from GitHub/release artifacts. Keep this path stable: the systemd backup service references it. |
| `/etc/previa/previa.env` | Production settings and generated/application credentials | Yes, encrypted configuration archive. |
| `/etc/previa/github-app.pem` | GitHub App private key | Yes, encrypted configuration archive. |
| `/etc/previa/dns-credential` | DNS API token | Yes, encrypted configuration archive. |
| `/var/lib/previa/router` | `previa-dashboard.yml` and worker's `previa-routes.yml` | No; regenerated from config and accepted database routes. |
| `/var/lib/previa/backup/last-success` | Backup success timestamp read by the server | No; the next complete backup recreates it. |
| Docker volume `previa_postgres` | PostgreSQL data files | Logical database dump, not a raw copy of the live volume. |
| Docker volume `previa_acme` | Let's Encrypt accounts, keys and certificates | Yes, encrypted ACME archive. |
| Docker volume `previa_sources` | Temporary exact-commit source workspaces | No; refetched from GitHub. |
| Labelled build cache volumes and local preview images | Rebuildable build artifacts | No. |
| `previa_prometheus`, `previa_grafana` volumes | Metrics history and Grafana state | No; datasource/dashboard provisioning can be recreated, but local history/customizations are lost unless separately backed up. |
| Root's rclone and Docker login configuration | Storage credentials and optional private-registry login | Not by default: these live outside `/etc/previa`. Store recovery copies separately. |

Docker volumes normally live under the daemon's data root (`/var/lib/docker` on
the expected host). Confirm with `docker info --format '{{.DockerRootDir}}'`;
do not assume they live inside `/opt/previa` or on GitHub. Keeping volumes across a
container recreation preserves their data; deleting the VPS or its disk does not.

Compose uses project name `previa`, control network `previa_control`, subnet
`172.31.250.0/24`, router address `172.31.250.10` and server address
`172.31.250.20`. Verify this subnet does not conflict with the VPS's private
network/VPN. These addresses also occur in the dashboard route and trusted-proxy
configuration; changing only the subnet is insufficient.

## Fresh host setup

### Step 0 — Prepare the DigitalOcean account and record deployment inputs

1. Sign in to DigitalOcean, configure billing and protect account recovery.
   Use the team/project that should own both the Droplet and Spaces bucket.
   Create a project such as `previa-production`; use a separate staging project
   and host for first deployment and restore drills.
2. Have an SSH key on your administration workstation. Upload the **public**
   key during Droplet creation; the private key stays on your workstation.
   For a new key, use an unused output filename and a passphrase:

   ```sh
   ssh-keygen -t ed25519 -a 100 -f ~/.ssh/previa_digitalocean_ed25519 -C previa-admin
   cat ~/.ssh/previa_digitalocean_ed25519.pub
   ```

3. Have access to the dashboard domain and a separate preview domain, their
   registrar/Cloudflare zones, GitHub App/OAuth settings, the repository and
   a tested release. Domains are purchased through a registrar; creating a
   Droplet does not register a domain or configure GitHub.
4. Record the following **non-secret** inventory in your operations notes.
   Keep secret values separately in a password manager/recovery store:

   | Input | Example / what to record |
   | --- | --- |
   | Project and Droplet | `previa-production`, `previa-prod-01`, actual Droplet ID |
   | Region and resources | Actual datacenter slug, CPU class/count, RAM and disk |
   | Public endpoint | Actual public IPv4; optional Reserved IPv4 if selected |
   | Admin source | Your current public administration IP/CIDR for SSH rules |
   | Dashboard | `previa.example.com` |
   | Preview domain | `preview.example.net`; wildcard `*.preview.example.net` |
   | Backup bucket | Globally available bucket name, represented below by `YOUR_BUCKET` |
   | Spaces endpoint | Actual regional origin, e.g. `nyc3.digitaloceanspaces.com` |
   | Release | Existing tested tag and exact Git commit; never assume an example tag exists |
   | Receiver/recovery contacts | Monitored email, alert destination and recovery owner |

Use the same concrete values throughout this guide. `YOUR_BUCKET`,
`YOUR_SERVER_IP`, domain names and version examples are placeholders.
Never substitute a Spaces URL for the database hostname.

### Step 1 — Choose the host and release

In the DigitalOcean control panel, open your production project and choose
**Create → Droplets**. Configure the initial host using the following choices.
Labels can change in the provider UI; the actual values/resources are what
matter. See [DigitalOcean's creation instructions](https://docs.digitalocean.com/products/droplets/how-to/create/).

| Creation choice | Value / reason |
| --- | --- |
| Image | Plain **Ubuntu 24.04 LTS, x86-64**. The existing setup script prepares it; an App Platform deployment or Kubernetes image is unnecessary. |
| Region | Close to the expected operators/users and dependency endpoints. Check Spaces availability too. Keep PostgreSQL, worker and previews on this one host. |
| CPU/RAM/disk | Meet the initial budget below. Prefer dedicated CPU for sustained CPU-heavy builds; shared CPU can suit staging/light traffic but requires measurement. |
| SSH authentication | Select the public key from Step 0. Preserve the matching private key and console recovery access. |
| Monitoring | Enable the metrics-agent/improved-monitoring option; verify collection later. |
| Provider backups | Optional additional paid disk recovery. It does not replace Previa's encrypted DB backups; see B7. |
| Hostname/tags/project | `previa-prod-01`, tags such as `previa` and `production`, project `previa-production`. Tags organize resources and can select firewall/alert targets. |
| Public networking | Public IPv4 required for the documented DNS/HTTP path. Publish IPv6 only after verifying it end to end. |
| Quantity | **One** Droplet. Previa creates Docker preview containers on it. |

The previous 4 vCPU / 8 GiB RAM / 80 GiB disk baseline is a lower bound, not a
promise that all defaults fit. In particular, **two default builds can each use
4 GiB**, while previews default to a budget of 60% of host memory. PostgreSQL,
monitoring, Docker and Linux need memory too. Explicitly budget them together.

A conservative first-host example is **at least 8 vCPU, 16 GiB RAM and 160 GiB
SSD**, with **one 2 GiB build at a time**, a **4 GiB preview memory budget** and
**4 CPUs of total preview limits**. It leaves room for three standard previews
and a standard replacement candidate if other previews do not consume that
budget. Other tiers change the calculation. These are operator starting
allocations, not a benchmark or promise of a specific request rate.

The Compose ceilings are separately 2 GiB for PostgreSQL, 512 MiB server,
1 GiB worker, 512 MiB router, 1 GiB Prometheus and 512 MiB Grafana. Also leave
capacity for Linux, Docker, egress, image builds, buffers and filesystem cache.
Container memory limits are ceilings rather than a reservation of that entire
amount. Inspect actual usage before raising `MAX_CONCURRENT_BUILDS` or preview
budgets. The CPU/memory admission counts declared preview limits, not only
instantaneous utilization. See Step 5.

Keep Docker data on the Droplet's persistent SSD initially. Spaces is not a
filesystem for PostgreSQL/Docker. A separately attached block-storage volume is
optional and requires an explicit mount/data-root/backup design; the shipped
setup does not move Docker data to it.

#### Attach a DigitalOcean Cloud Firewall

In **Networking → Firewalls**, create `previa-production` and apply it to this
Droplet or its intended production tag. Use allow rules with these boundaries:

| Direction | Protocol/port | Source or destination |
| --- | --- | --- |
| Inbound | TCP 22 | Your administration public IPv4/CIDR; add necessary administrator IPv6 only when used. |
| Inbound | TCP 80 | Internet IPv4; IPv6 too only when that public path is configured. |
| Inbound | TCP 443 | Same as HTTP; GitHub webhooks and reviewers must reach it. |
| Outbound | TCP, UDP and ICMP | Allow the normal outbound defaults initially; DNS, registries, GitHub, package downloads, certificate requests and Spaces must work. |

Do not add inbound DB, internal API or monitoring ports: 5432, 8080, 8082,
9090/9091, 9093 and 3000 are not public services in this deployment.
Confirm the firewall is actually attached. If your admin IP changes, update the
SSH source rule through the control panel/console before expecting SSH to work.
[Cloud Firewall setup](https://docs.digitalocean.com/products/networking/firewalls/how-to/create/)
and [rule configuration](https://docs.digitalocean.com/products/networking/firewalls/how-to/configure-rules/).

`setup.sh` also configures UFW for standard OpenSSH. If using a custom SSH port,
arrange the matching provider/UFW rules before running it. Docker's published
ports use its own firewall chains; keep the provider firewall and narrow Compose
bindings. Cloud Firewall protects the Droplet boundary; it does not implement
Previa's per-PR Docker network isolation.

#### Optional: reserve the public address

The Droplet's assigned public IPv4 is sufficient for the initial deployment.
An optional **Reserved IPv4** lets you deliberately reassign the public endpoint
to a replacement Droplet in the same datacenter. Create/assign it through
**Networking → Reserved IPs**, record it as the chosen endpoint, and point both
DNS records at it instead of mixing addresses. Test ports 80/443 at that address.
This is a recovery convenience, not automatic failover or a second running host.
[Reserved IP quickstart](https://docs.digitalocean.com/products/networking/reserved-ips/getting-started/quickstart/).

#### First SSH login and the administrator account

DigitalOcean's plain Ubuntu image initially exposes `root` for the selected SSH
key; the later `ops` examples require creating that user. On your workstation:

```sh
ssh -i ~/.ssh/previa_digitalocean_ed25519 root@YOUR_SERVER_IP
```

Verify the host fingerprint using trusted console access on first connection.
On this new Droplet, as root:

```sh
adduser ops
usermod -aG sudo ops
install -d -o ops -g ops -m 0700 /home/ops/.ssh
install -o ops -g ops -m 0600 /root/.ssh/authorized_keys /home/ops/.ssh/authorized_keys
```

Before closing the root session, open a second workstation terminal and verify:

```sh
ssh -i ~/.ssh/previa_digitalocean_ed25519 ops@YOUR_SERVER_IP
sudo -v
```

The `ops` account needs the password configured by `adduser` for `sudo`.
Later SSH/SCP examples assume this key is selected by your SSH agent or local
SSH configuration; add `-i ~/.ssh/previa_digitalocean_ed25519` when necessary.
Keep the DigitalOcean console/recovery path available.

Use a separate staging VPS for first deployment, restores and upgrade drills.
Choose a release only after checking the [CI and releases](#ci-and-releases)
section and the [local staging smoke test](#local-staging-smoke-test).

### Step 2 — Set up domains, DNS and the DNS token

For example, own `example.com` for the dashboard and a separate registrable domain
`example.net` for untrusted previews. Set up their authoritative DNS first:

| Record | Value | Previa setting |
| --- | --- | --- |
| `A previa.example.com` | VPS public IPv4 | `APP_BASE_URL=https://previa.example.com`, `PREVIA_DASHBOARD_HOST=previa.example.com` |
| `A *.preview.example.net` | Same VPS public IPv4 | `PREVIEW_BASE_DOMAIN=preview.example.net` |

The wildcard covers `<generated-label>.preview.example.net`, not the base domain
itself. An additional base-domain A record is optional. Only add AAAA records if
you actually configure and verify the corresponding public IPv6 path.

For Cloudflare, add each zone, complete the registrar's nameserver change, then
create the records as **DNS only**. Direct access is the tested routing model;
the dashboard's TLS-ALPN-01 challenge needs the internet to reach Traefik on 443.
Cloudflare proxying or another TLS-terminating load balancer requires a separate
certificate/proxy design and is not covered by copying these records unchanged.

Create a Cloudflare API token with `Zone / DNS / Edit` and `Zone / Zone / Read`,
scoped to the preview zone. One token with both permissions is supported by the
[lego Cloudflare provider](https://go-acme.github.io/lego/dns/cloudflare/).
Keep the token in a password manager until Step 4 installs it on the server.
Set `ACME_DNS_PROVIDER=cloudflare` and
`ACME_DNS_CREDENTIAL_VARIABLE=CF_DNS_API_TOKEN`; do not put `_FILE` in that
setting, because Compose appends it. Other providers require their corresponding
lego credential variable and possibly additional configuration.

Confirm DNS from your workstation before certificate issuance:

```sh
dig +short previa.example.com A
dig +short any-label.preview.example.net A
```

Both should resolve to the VPS IP. `dig` can be installed through your OS's DNS
utilities package. Also check restrictive CAA records if certificate issuance
fails: they must permit the chosen certificate authority.

### Step 3 — Install the checkout and prepare the host

**On your workstation**, SSH to the server:

```sh
ssh ops@YOUR_SERVER_IP
```

**On the VPS**, enter an administrative shell. The remaining VPS examples run as
root so file modes, Docker and systemd commands are consistent:

```sh
sudo -i
apt-get update
apt-get install -y git nano

git clone https://github.com/ajaykathwate/Previa.git /opt/previa
cd /opt/previa
git fetch --tags origin
PREVIA_RELEASE=v0.1.0
# Replace the example above with the tested tag you actually published.
git checkout --detach "$PREVIA_RELEASE"

deploy/production/setup.sh
systemctl stop previa-backup.timer
```

For a private repository, authenticate the checkout through an appropriate
read-only credential, without embedding a token in the clone URL. Use a clean
release checkout rather than editing application source on the production host.

`setup.sh` installs Docker Engine/Compose/Buildx when Docker is absent, age,
rclone, OpenSSL and UFW; enables Docker at boot; creates the state directories;
and creates `/etc/previa/previa.env` only if it does not exist. If Docker already
exists, verify its Compose and Buildx plugins yourself:

```sh
docker version
docker compose version
docker buildx version
systemctl is-enabled docker
stat -c '%g' /var/run/docker.sock
```

It generates database, session, webhook, repository-encryption and Grafana
secrets and fills `DOCKER_GID` from the socket. It also installs/enables the backup
timer immediately. The explicit stop above prevents scheduled failures while
you are still configuring storage and the application. Re-enable it after the
first successful backup. Re-running setup does not regenerate an existing env
file, but does update its Docker group and credential file permissions.

Before running builds on an Ubuntu 24.04 Docker host, configure the
[RootlessKit user-namespace prerequisite](https://github.com/moby/buildkit/blob/master/docs/rootless.md#ubuntu-2404-or-later)
as root. `setup.sh` does not configure this sysctl:

```sh
if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
    printf '%s\n' 'kernel.apparmor_restrict_unprivileged_userns=0' \
        > /etc/sysctl.d/90-previa-rootless.conf
    sysctl -p /etc/sysctl.d/90-previa-rootless.conf
fi
```

**Expected:** on a host with that setting, sysctl prints
`kernel.apparmor_restrict_unprivileged_userns = 0`. The file reapplies it at boot.
This changes the dedicated Docker host's handling of unprivileged user
namespaces used by the rootless builder. Container capability, network and
resource policies are configured separately by Previa. Rehearse a real preview
build on staging after configuring the host.

### Step 4 — Register GitHub integrations and install credential files

Create separate production registrations instead of changing working local
callbacks accidentally. The dashboard uses a GitHub **OAuth App**, while repo
access uses a GitHub **App**; their client IDs/secrets are different.

1. In GitHub **Settings → Developer settings → OAuth Apps → New OAuth App**, use
   `https://previa.example.com` as the homepage and
   `https://previa.example.com/auth/github/callback` as the authorization callback.
   Copy the OAuth client ID and create a client secret.
2. In **GitHub Apps → New GitHub App**, use the same homepage. Set setup URL
   `https://previa.example.com/installations/callback`, enable **Redirect on
   update**, and set callback URL
   `https://previa.example.com/installations/authorize/callback`. Leave **Request
   user authorization during installation** unchecked: Previa starts that flow.
3. Grant repository permissions **Contents: read**, **Pull requests: write**,
   **Checks: write**; Metadata read is automatic. Subscribe to **Pull request**.
   GitHub also delivers installation lifecycle events used by the implementation.
4. Set webhook URL `https://previa.example.com/webhooks/github`. Copy the generated
   `GITHUB_WEBHOOK_SECRET` from the host's env file into the App's webhook secret
   field. Keep verification enabled. Webhooks with staging-CA certificates will
   not be a successful live proof; verify deliveries after production TLS works.
5. Save the App ID, App slug, App client ID and newly generated App client secret.
   Generate and download an App private key. Install the App on a disposable test
   repository first; sign-in alone does not grant that repository access.

See [04 · GitHub sign-in and App installation](04-github-sign-in-and-app.md)
for the distinction and permission rationale.

Upload the private key from your **workstation** to your SSH user's home:

```sh
scp /path/to/downloaded-app-key.pem ops@YOUR_SERVER_IP:github-app.pem
```

Then, as root on the **VPS**, install it and create the DNS-token file:

```sh
install -o 65532 -g 65532 -m 0600 /home/ops/github-app.pem /etc/previa/github-app.pem
rm /home/ops/github-app.pem
install -o root -g root -m 0600 /dev/null /etc/previa/dns-credential
nano /etc/previa/dns-credential
```

Adjust `/home/ops` to your SSH user's actual home directory. Paste only the DNS
token into the DNS file. Do not place these files in the repository. The App key
is a real PEM file, not a PEM string in an environment variable. It must be owned
by UID 65532 and mode 600 because the non-root server and worker read it; the DNS
token is read by the router and stays root-owned, mode 600. `/etc/previa` itself
is root-owned, mode 700. Compose bind-mounts the individual credential files.

### Step 5 — Put production environment variables in the correct file

Edit the file created by setup:

```sh
nano /etc/previa/previa.env
chmod 600 /etc/previa/previa.env
```

**This is the production env file.** The repository's `.env.example` and local
`.env` are for development. Neither the Go binaries nor a checkout automatically
load `.env`; production Compose explicitly supplies
`/etc/previa/previa.env`. You do not need to export all its secrets in your shell
or add them to GitHub Actions secrets.

Keep one unquoted `NAME=value` entry per setting. The backup script reads its
settings as literal text, so quoted values that Compose alone would understand
would break the shared format. The generated secret formats are suitable for
this file. For additional settings and validation see
[configuration setup](../technical-docs/config-setup.md) and
[development setup](02-development-setup.md); those files are references, not
additional production env files to copy.

| Settings in `previa.env` | What to set / where the value comes from |
| --- | --- |
| `PREVIA_VERSION` | Exact tested tag, e.g. `v0.1.0`; it must match the checkout and all three images. |
| `PREVIA_SERVER_IMAGE`, `PREVIA_WORKER_IMAGE`, `PREVIA_EGRESS_IMAGE` | For published images: `ghcr.io/ajaykathwate/previa-server`, `ghcr.io/ajaykathwate/previa-worker`, `ghcr.io/ajaykathwate/previa-egress`. These are image names **without tags**; Compose adds `:${PREVIA_VERSION}`. Use your actual lowercase owner if it changes. Leave absent for local-image builds. |
| `DOCKER_GID` | Generated from the host Docker socket. Not the worker's UID. |
| `POSTGRES_PASSWORD` | Generated by setup. Used when the database volume is initialized. |
| `DATABASE_URL` | Setup writes `postgres://previa:<generated-password>@postgres:5432/previa?sslmode=disable`. `postgres` is a private Compose hostname, not `localhost`. Keep the password consistent. |
| `APP_BASE_URL` | `https://previa.example.com`; an origin, without an extra path. |
| `PREVIA_DASHBOARD_HOST` | `previa.example.com`; no scheme or port for this deployment. Must match `APP_BASE_URL`'s hostname. |
| `SESSION_SECRET` | Generated random hex value; retain across restarts unless intentionally rotating. |
| `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET` | OAuth App credentials from Step 4; dashboard sign-in. |
| `GITHUB_APP_ID`, `GITHUB_APP_SLUG` | Numeric App ID and slug from the GitHub App page. |
| `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` | GitHub App credentials, used for installation authorization; not the OAuth App credentials. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Host path `/etc/previa/github-app.pem`; this tells Compose which file to mount. |
| `GITHUB_WEBHOOK_SECRET` | Generated secret, also set identically in GitHub's webhook configuration. |
| `SECRETS_ENCRYPTION_KEYS` | Generated `1:<base64-encoded-32-byte-key>`. Multiple rotation versions use commas: `1:<key>,2:<key>`. Back up every needed version offline. |
| `SECRETS_ACTIVE_KEY_VERSION` | `1` initially; the version used to encrypt new repository secrets. |
| `PREVIEW_BASE_DOMAIN` | `preview.example.net`; lowercase DNS name, no scheme or wildcard. |
| `ROUTER_ENTRYPOINT`, `ROUTER_API_ADDRESS` | Retain `websecure` and `127.0.0.1:8080`. This loopback is inside the router container. |
| `ACME_EMAIL` | A monitored operator email for the certificate account. |
| `ACME_DNS_PROVIDER`, `ACME_DNS_CREDENTIAL_VARIABLE` | `cloudflare` and `CF_DNS_API_TOKEN` for this example. |
| `ACME_DNS_CREDENTIAL_FILE` | Host path `/etc/previa/dns-credential`. Compose passes `CF_DNS_API_TOKEN_FILE=/run/secrets/dns_credential` to the router. |
| `ACME_CA_SERVER` | Let's Encrypt staging initially; switch to production only after staging issuance succeeds. |
| `ACME_DNS_RESOLVERS` | Optional; default `1.1.1.1:53,8.8.8.8:53`. Resolver reachability matters for DNS-01. |
| `BACKUP_AGE_RECIPIENT` | Public `age1...` recipient generated on your recovery workstation, as described below. |
| `BACKUP_REMOTE` | `previa-backups:YOUR_BUCKET/production`; `previa-backups` is the root-owned DigitalOcean Spaces rclone remote, `YOUR_BUCKET` is the actual bucket, and `production` is an object-key prefix. It is not an HTTPS endpoint. |
| `BACKUP_RETENTION_DAYS` | `30` initially; choose according to your recovery requirements. |
| `BACKUP_FAILURE_WEBHOOK` | Optional receiver URL accepting the script's JSON `text` message. This only reports backup failures; it is not the whole Prometheus alert system. |
| `GRAFANA_ADMIN_PASSWORD` | Generated password; initial Grafana admin login. |
| `LOG_LEVEL` | `info`; use `debug` temporarily during a diagnosed incident, then restore. |

#### Credential checklist: what is generated, supplied and kept offline

| Credential | Obtain it from | Production location / scope |
| --- | --- | --- |
| SSH private key | Your workstation's key generation | Workstation/SSH agent only; DigitalOcean and the Droplet receive the public key. |
| Database password | `setup.sh` generates it | `POSTGRES_PASSWORD` and password portion of `DATABASE_URL` in `previa.env`. Not a Spaces password. |
| Session secret | `setup.sh` generates it | `SESSION_SECRET` in `previa.env`. |
| Webhook signing secret | `setup.sh` generates it | `GITHUB_WEBHOOK_SECRET` in `previa.env` and the same value in GitHub App webhook settings. |
| OAuth client ID/secret | GitHub OAuth App | `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET` in `previa.env`. |
| App client ID/secret and App ID | GitHub App | `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` and `GITHUB_APP_ID` in `previa.env`. |
| App signing PEM | GitHub App private-key generation | `/etc/previa/github-app.pem`, UID 65532, mode 600. |
| Repository-secret encryption keyring | `setup.sh` generates the initial version | `SECRETS_ENCRYPTION_KEYS` in `previa.env` and a protected offline recovery copy. |
| DNS certificate token | Scoped Cloudflare API token | `/etc/previa/dns-credential`, root-owned, mode 600. |
| Spaces access key and secret | DigitalOcean Spaces access-key page | Root's protected rclone config; limited access to the production backup bucket. Separate recovery copy. |
| age public backup recipient | Recovery workstation `age-keygen` | `BACKUP_AGE_RECIPIENT` in `previa.env`; public value can remain on host. |
| age private recovery identity | Same recovery-key generation | Offline; not needed by the nightly service. |
| Grafana admin password | `setup.sh` generates it | `GRAFANA_ADMIN_PASSWORD` in `previa.env`; initialize/manage Grafana account separately. |
| GHCR pull credential | Authorized GitHub package-read credential, only if images are private | Root's Docker login configuration. It is not the GitHub App token. |
| Alert receiver secret | Selected notification service | Protected host Alertmanager config/file, if the receiver needs it. |

No DigitalOcean personal API token is required for this manual deployment.
Do not add production secrets to Droplet metadata/user-data, the Git checkout,
GitHub issue bodies, Docker build arguments or Actions secrets just to run the
existing workflows. Those workflows publish artifacts and use their own
`GITHUB_TOKEN`. Application env values belong on the Droplet as above.

Compose sets these application values itself; retain them unless you deliberately
change the deployment topology:

| Container setting | Provided value / purpose |
| --- | --- |
| `APP_ENV` | `production` on server and worker. |
| `HTTP_ADDR`, `INTERNAL_ADDR` | Server `0.0.0.0:8080`, `0.0.0.0:9090`; listens on its container interfaces, not host-published ports. |
| `WORKER_INTERNAL_ADDR` | Worker `0.0.0.0:9091`. |
| `TRUSTED_PROXIES` | `172.31.250.10`, the router's private address. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | `/run/secrets/github_app_key` **inside** server/worker; different from host `GITHUB_APP_PRIVATE_KEY_FILE`. |
| `SOURCE_WORKSPACE_ROOT` | `/var/lib/previa/sources`, mounted source volume. |
| `ROUTER_DYNAMIC_DIR`, `ROUTER_CONTAINER` | `/var/lib/previa/router`, `previa-router`. |
| `EGRESS_IMAGE` | The selected egress image name plus the release tag. |
| `BACKUP_STATUS_FILE` | Server reads `/var/lib/previa/backup/last-success`. |

The current shared `env_file` gives server, worker and migrate access to the same
file's values, although each Go config loader consumes only its own fields. This
is not a deployment with per-service secrets-manager isolation. Compose's explicit
`environment` entries override `env_file` entries; exported shell variables can
also override values used in Compose interpolation. Use a clean administrative
shell, edit the file deliberately, and avoid competing exported overrides.

For the 8 vCPU / 16 GiB example, edit or add these entries once each. These
operator settings replace the env template's two-build default:

```dotenv
MAX_CONCURRENT_BUILDS=1
BUILD_TIMEOUT=12m
BUILD_CPUS=2
BUILD_MEMORY=2GiB
BUILD_PIDS=2048
BUILD_DISK_BUDGET=20GiB
BUILD_MAX_IMAGE_SIZE=4GiB
BUILD_MIN_FREE_DISK=15GiB
BUILD_CACHE_BUDGET=20GiB
PREVIEW_MEMORY_BUDGET=4GiB
PREVIEW_CPU_BUDGET=4
PREVIEW_MAX_PER_REPOSITORY=3
ENV_TTL_HOURS=48
BUILDS_PAUSED=false
```

Each build runs in a separate builder container, so the worker's own 1 GiB limit
is not the build's memory limit. Likewise, preview limits are separate from the
server's limit. The usual repository `standard` tier is 1 vCPU / 1 GiB; the total
preview budgets can admit fewer previews than the per-repository cap. Allow room
for a replacement candidate while its old preview is still serving.

Other implemented optional controls retain their defaults unless you need them:

| Setting | Default / effect |
| --- | --- |
| `BUILDKIT_IMAGE` | Pinned rootless BuildKit image from `internal/platform/config/builder.go`; pulled on the first build if absent. |
| `PREVIEW_ALLOW_ROOT`, `PREVIEW_WRITABLE_ROOTFS` | `false`, `false`; compatibility switches chosen by the operator, not the PR. |
| `PREVIEW_TMPFS_SIZE`, `PREVIEW_PIDS` | `256MiB`, `512`; temporary writable space and process cap per preview. |
| `PREVIEW_STARTUP_TIMEOUT` | `120s`; deadline for initial readiness. |
| `PREVIEW_OCI_RUNTIME` | Empty: Docker's default runtime. `runsc` requires separately installing/configuring gVisor on the host. |
| `HOUSEKEEPING_DRY_RUN` | `false`; `true` observes deletion decisions without executing housekeeping deletions. |
| `HOUSEKEEPING_GRACE` | `15m` before unreferenced resources count as orphans. |
| `PREVIA_ENV_FILE` | Optional alternate host env-file path for container `env_file` and scripts. If changed, also pass that path to Compose `--env-file` and the backup systemd service. |
| `PREVIA_ROUTER_DIR` | Host bind directory, default `/var/lib/previa/router`; different from the worker's container path. |
| `PREVIA_BACKUP_STATUS_DIR` | Host bind directory, default `/var/lib/previa/backup`; keep backup writer and server mount consistent. |

Changing `POSTGRES_PASSWORD` in this file does **not** change the password in an
already initialized PostgreSQL volume. Password rotation needs a database role
password change too; use an interactive `psql` session and `\password previa`,
then update the URL and env together. Changing settings requires container
recreation: `dc restart` alone does not load a changed env file. Instructions for
safe recreation and key rotation appear below.

### Step 6 — Prepare backups before treating the host as production

Follow [Backups and restore](#backups-and-restore) to generate your age key, create
an off-host bucket, configure **root's** rclone credentials, and fill the backup
settings. Keep the timer stopped until the database/router are running and a
manual backup succeeds. Store the recovery identity and repository-encryption
keyring outside the VPS. Also retain the storage account recovery credentials;
they are required if the original host disappears.

## First start, with staged certificates

### Step 7 — Define the production commands and obtain images

Run this on the VPS, in the root shell. Recreate these functions after opening a
new SSH session; they are shell helpers, not installed commands:

```sh
cd /opt/previa
dc() {
  docker compose -p previa \
    -f /opt/previa/deploy/production/compose.yml \
    --env-file /etc/previa/previa.env --profile observability "$@"
}
probe() {
  docker run --rm --network previa_control curlimages/curl:8.15.0 \
    -fsS --max-time 15 "$@"
}
```

The `observability` profile is deliberately present on every normal `dc` command
in this guide. The `images` profile is used only while pulling/building images;
**do not enable it on `up`**, because the worker creates the real `previa-egress`
container itself.

For **published GHCR images**, uncomment/set the three `PREVIA_*_IMAGE` names in
`previa.env`, set the matching `PREVIA_VERSION`, then:

```sh
dc config --quiet
dc --profile images pull
```

`config --quiet` validates Compose without printing interpolated secrets. A normal
`dc config` dump contains secrets and should not be pasted into an issue.
For public GHCR packages, pulling normally needs no login. For private packages,
use an authorized token with package read access:

```sh
docker login ghcr.io -u YOUR_GITHUB_USERNAME
```

Enter the token at the password prompt rather than putting it in a command or
URL. The host's registry login is different from Previa's OAuth/App credentials.
A token used only to pull images is not needed inside application containers.

Alternatively, **build the exact checked-out source manually on the host**, with
the `PREVIA_*_IMAGE` overrides absent and `PREVIA_VERSION` set to your chosen tag:

```sh
dc config --quiet
dc --profile images build server worker egress
```

Docker runs the pinned Go/template/CSS build stages; Go/Node do not need to be
installed directly on the VPS. A binary release archive alone does not contain
the full source/Dockerfile needed for this build path. With an archive, use the
published images and retain its Compose/scripts/monitoring layout instead.

Check the environment before starting services:

```sh
dc run --rm --no-deps server check-config
dc run --rm --no-deps worker check-config
```

Both should print `configuration is valid` and exit 0. This validates config and
file checks; it does not prove GitHub access, successful certificate issuance,
valid encrypted repository data, database migrations or a complete preview.
Those are separate startup/live checks below.

### Step 8 — Start with Let's Encrypt staging

Retain the staging URL in the env file:

```dotenv
ACME_CA_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory
```

Start the stack:

```sh
dc up -d --wait --wait-timeout 180
dc ps -a
dc logs --tail 100 migrate router-config
dc logs --since 10m router
curl -ksS -D - -o /dev/null https://previa.example.com/
curl -ksS -D - -o /dev/null https://any-label.preview.example.net/
```

Expected behavior:

1. PostgreSQL initializes its persistent volume and becomes healthy.
2. `migrate` applies this release's embedded Goose schema and River schema, then
   exits successfully. The code reviewed here requires **Goose 20**; the release's
   migration status reports the River count as well.
3. `router-config` writes the dashboard route and exits 0; Traefik starts and
   watches the route directory.
4. Server starts only after migrations succeed. Worker starts after migrations
   and router health, ensures the egress proxy exists, synchronizes the accepted
   route table, starts River and records its capabilities.
5. Prometheus and Grafana start because this guide enabled `observability`.
6. Traefik issues the wildcard preview certificate through DNS-01 and the
   dashboard certificate through TLS-ALPN-01. Check both certificates; a green
   container health check alone is not proof of ACME success.

Staging certificates are deliberately untrusted by normal browsers/clients.
`curl -k` is only for this staging check. The unknown preview hostname should
redirect to the dashboard's unavailable page; a redirect or Traefik default
certificate alone does not prove the wildcard certificate was issued. Inspect
the TLS certificate shown by your client and the router's ACME logs.

### Step 9 — Switch to trusted production certificates

Once both staging issuances work, change the env entry to:

```dotenv
ACME_CA_SERVER=https://acme-v02.api.letsencrypt.org/directory
```

For this **first staging-to-production conversion only**, remove the disposable
staging ACME state so Traefik obtains production certificates:

```sh
dc stop router
dc rm -f router
docker volume rm previa_acme
dc up -d --wait --wait-timeout 180
curl -fsS -D - -o /dev/null https://previa.example.com/
curl -sS -D - -o /dev/null https://any-label.preview.example.net/
```

The router stop briefly interrupts dashboard and preview routing. The volume
removal deletes the staging account/certificates; do not reuse this procedure
for routine upgrades or production certificate renewal. If staging data must be
retained, archive it before this step. Never remove `previa_postgres` here.

A normal client should now accept both certificates without `-k`. Resolve any DNS,
firewall, DNS-token or ACME errors before enabling real GitHub webhook traffic or
calling the service live. Traefik subsequently renews certificates itself; keep
the credential and ACME volume available.

## Verify the deployment

### Step 10 — Check each process and the complete user flow

On the VPS:

```sh
dc ps -a
probe http://server:9090/readyz
probe http://worker:9091/readyz
dc exec -T server /usr/local/bin/previa-server healthcheck
dc exec -T worker /usr/local/bin/previa-worker healthcheck
dc run --rm --no-deps migrate status
docker ps --filter label=io.previa.managed=true
```

Server readiness should report `ready` and the intended version. Worker readiness
must also show **discovery, execution, routing, reporting and housekeeping enabled**
for a full deployment, and `paused=false`. A deliberately paused worker may be
healthy while `execution=false`; a partially configured worker can also be ready
for fewer capabilities. Check capability values rather than accepting HTTP 200
as proof that it can build.

`migrate` and `router-config` being `Exited (0)` is expected. An exit code other
than 0 needs investigation. The egress container appears in `docker ps`, not
`dc ps`. It must exist before accepting real builds.

Then, from an external browser and GitHub:

1. Open `https://previa.example.com` with trusted TLS. Sign in through GitHub.
2. Install/link the GitHub App on a disposable repository you own. Wait for PR
   discovery to complete; the Overview should show the required pipeline stages
   connected.
3. On that repository, commit a compatible Dockerfile and `.previewrc.yaml`.
   The app must listen on `0.0.0.0` at its declared internal port, tolerate the
   non-root/read-only runtime, and pass its health check. See
   [source/configuration](06-source-and-configuration.md) and
   [preview runtime](10-preview-runtime.md).
4. Open a pull request. In the App's **Advanced → Recent deliveries**, verify
   the signed delivery succeeds. A bad signature usually means the webhook secret
   differs; a TLS error means the public certificate path is not ready.
5. Repositories default to **Manual**. Select **Build latest commit**; opening
   a PR or pushing a commit by itself does not authorize a manual-mode build.
6. Watch the real build log and status. Confirm one living PR comment and an
   exact-commit check run, then open the preview link from another machine.
7. Push a changed commit and build it manually. Confirm the preview updates.
   Verify a deliberately failed rebuild keeps the older successful preview.
8. If you want Automatic, enable it for this repository: eligible head changes
   build after **five quiet minutes**, not immediately after every push. Test a
   burst of changes and ensure only the latest eligible commit is published.
9. Tear down from the PR page, then test close/merge cleanup. The URL should
   become unavailable and the owned containers/networks should be cleaned up.
10. Take an encrypted backup, confirm off-host storage, rehearse a restore on a
    different disposable host, and verify actual alert delivery.

The host/runtime recovery and live hosted GitHub/DNS checks remain deployment
acceptance steps; local passing tests cannot substitute for them.

## Monitor live traffic, jobs and containers

### What keeps services running after you disconnect?

`dc up -d` leaves containers running under Docker; your SSH shell is not their
parent supervisor. Docker is enabled at boot. Persistent Compose services use
`restart: unless-stopped`; one-shot setup/migration services use `restart: no`.
If a persistent process exits, Docker restarts it unless it was deliberately
stopped. **A health check turning unhealthy does not itself restart a running
process in this plain Compose setup.** Investigate its readiness and logs.
See [Docker restart policy behavior](https://docs.docker.com/engine/containers/start-containers-automatically/).

Preview containers have a different policy: **on failure, at most three
restarts**. They are not promised to restart automatically on a Docker-daemon
restart. After a host reboot, inspect them and the dashboard; missing/dead
previews are reconciled and may need an owner rebuild. Do not apply
`restart: unless-stopped` to all previews as an undocumented lifecycle change.
Builders are disposable and recover through durable build jobs. The shared egress
container uses `unless-stopped` and is also ensured on worker startup.

A worker shutdown drains jobs for 30 seconds, then cancels remaining work within
its bounded shutdown; durable work resumes/reconciles when the worker returns.
This is recovery, not a guarantee that a process restarts at the exact Dockerfile
instruction it was running. Existing healthy previews may continue serving during
a worker/server restart if Traefik and the preview containers remain running.

### Open Grafana and check metric collection

On your **workstation**:

```sh
ssh -N -L 3000:127.0.0.1:3000 ops@YOUR_SERVER_IP
```

Open `http://localhost:3000`, sign in as `admin` with the generated
`GRAFANA_ADMIN_PASSWORD`, and open the **Previa** dashboard (UID `previa`). The
provisioned datasource uses `http://prometheus:9090` on the private network.
If you change Grafana's initial admin-password env value after its database was
initialized, do not assume it rotates the stored password; manage that through
Grafana's account/password controls.

On the **VPS**, allow at least one scrape interval, then check:

```sh
probe http://prometheus:9090/-/ready
probe http://grafana:3000/api/health
probe http://prometheus:9090/api/v1/targets
probe -G --data-urlencode 'query=up' http://prometheus:9090/api/v1/query
probe http://prometheus:9090/api/v1/rules
probe http://prometheus:9090/api/v1/alerts
```

The three configured scrape jobs are `previa-server`, `previa-worker`, and
`traefik`; each should have `up=1`. Prometheus is not published on a host port.
Use its API through the private-network probe or Grafana Explore. Internal health
listeners and Traefik's control API are not public dashboards to expose.

| What to watch | Existing signals / where to look | What it tells you |
| --- | --- | --- |
| User-facing Previa traffic | Grafana HTTP panels; `previa_http_requests_total`, HTTP duration, ingress rejections and SSE streams | Request rates, error ratio, latency, refused requests and active streams in the Go server. |
| Traffic to actual preview apps | Traefik metrics at `router:8082/metrics`, queried in Grafana Explore | Requests traverse Traefik straight to the app, so Go-server HTTP counts do not count preview visits. Inspect Traefik service labels for dashboard versus preview backends. |
| Build progress and failures | Previa PR page/logs; Grafana builds and failure-code panels | Which attempt is running, why it failed, and whether a candidate became healthy. |
| Time to preview | `previa_request_to_preview_seconds` and dashboard p50/p95 | Time from eligibility to serving, excluding Automatic's initial quiet-period delay. |
| Background queue backlog | `previa_queue_jobs`, `previa_queue_oldest_available_seconds`, queue wait p95 | Queues growing, oldest due work waiting, distinction between scheduled work and a stalled queue. |
| Job execution | `previa_jobs_total`, `previa_job_duration_seconds`, correlated worker logs | Success, failure, snoozes and cancellations by job kind. A capacity/disk/paused wait is not necessarily a build failure. |
| Worker availability | `previa_workers_live`, `previa_worker_capability`, worker `/readyz`, Overview | Whether a worker is alive and capable of discovery, building, routing, reporting and cleanup. |
| Durable publication and teardown | `previa_operations`, `previa_environments`, operation logs | Route changes, GitHub reports and cleanup waiting/retrying independently of a build. |
| Capacity and housekeeping | Build waiting reasons, housekeeping action counts, `previa_build_disk_free_bytes` | Admission limits, cleanup behavior, free disk **at the last build check**, not a continuous host disk monitor. |
| GitHub and webhooks | Request/rate-limit metrics, webhook delivery outcomes, GitHub delivery screen | API failures, token/permission problems, exhausted API budget and signature failures. |
| Backup health | `previa_backup_last_success_timestamp_seconds`, systemd status and Spaces objects | Whether the entire encrypted backup and retention pass succeeded. |
| TLS | `traefik_tls_certs_not_after`, router ACME logs, external HTTPS check | Upcoming expiry, failed renewal and public certificate acceptance. |
| Database | `dc ps postgres`, readiness, PostgreSQL logs and read-only SQL checks | Connectivity/schema, active connections, volume disk pressure. A PostgreSQL exporter is not included. |

For preview traffic, inspect available service labels and use queries such as:

```promql
sum by (service) (rate(traefik_service_requests_total[5m]))
histogram_quantile(0.95, sum by (le, service) (rate(traefik_service_request_duration_seconds_bucket[5m])))
sum by (queue, state) (previa_queue_jobs)
max by (queue) (previa_queue_oldest_available_seconds)
```

The supplied Grafana dashboard primarily tracks the Previa pipeline and server;
it does not automatically create a per-preview application dashboard. Preview
apps' own business metrics, continuous host metrics, PostgreSQL query metrics,
central log storage, tracing, and external uptime checks are separate integrations.
Traefik's current production config has **access logs disabled**; `dc logs router`
shows operational logs, not a complete visitor request log. Retain private backend
metrics access and review series/storage growth if monitoring many preview routes.

### Configure DigitalOcean host monitoring

Use provider host monitoring alongside the private Prometheus/Grafana deployment:
DigitalOcean reports the Droplet's overall resource use; Previa metrics explain
which builds, jobs, routes and operations are waiting or failing. Selecting the
creation-time monitoring option does not create queue/backup alerts automatically.

On the Droplet, verify the selected agent:

```sh
systemctl status do-agent --no-pager
journalctl -u do-agent --since '30 minutes ago' --no-pager
```

If absent, follow DigitalOcean's installation route. Download and inspect the
installer on this host before running it:

```sh
curl -fsSL https://repos.insights.digitalocean.com/install.sh -o /root/previa-do-agent-install.sh
less /root/previa-do-agent-install.sh
bash /root/previa-do-agent-install.sh
systemctl status do-agent --no-pager
```

Reference: [DigitalOcean metrics-agent installation](https://docs.digitalocean.com/products/monitoring/how-to/install-metrics-agent/).
No Previa application secret or DigitalOcean API token is needed in
`previa.env` for this control-panel/agent setup.

Open the Droplet's **Insights/metrics** view and confirm recent CPU, memory and
root-disk data. In **Monitoring**, create resource alerts for this Droplet or its
production tag, select an actual email/notification destination, then tune
thresholds after measuring normal use. Initial **operator examples**, not
provider defaults or workload guarantees:

| Resource alert | Example starting threshold | Response |
| --- | --- | --- |
| CPU utilization | Above 85% for 10 minutes | Compare running builds/previews with `docker stats`; reduce concurrency or resize after measurement. |
| Memory utilization | Above 80% for 10 minutes | Inspect PostgreSQL, builders and previews; retain headroom for the OS and replacement candidates. |
| Root disk utilization | Above 75% for 10 minutes | Inspect Docker images/cache, DB/log growth and backup temporary space before the build disk gate is reached. |
| Disk utilization, urgent | Above 85% for 5 minutes | Pause new builds and follow controlled capacity recovery; do not broadly prune live objects. |

Reference: [resource alert creation and notifications](https://docs.digitalocean.com/products/monitoring/how-to/manage-resource-alerts/).
Confirm the target and recipient on each alert. If your Docker data root is
moved to an attached volume later, root-disk monitoring alone does not establish
that volume's free space; add its own collection/alerts.

Host CPU/bandwidth graphs do not tell you which PR URL was visited or which
River job failed. Use Traefik metrics for preview HTTP traffic, Previa metrics
and logs for the pipeline, and `docker stats` for individual containers.

### Configure DigitalOcean Uptime for external HTTPS and certificate checks

After trusted production TLS is working, open **Uptime → Create Uptime Check**,
choose **HTTPS**, enter `https://previa.example.com/`, select relevant probe
regions and name it `previa-production-dashboard`. Confirm that this public
unauthenticated URL actually returns 2xx; Uptime counts responses outside
200–299 as failures. Do not expose internal `/livez`, `/readyz` or `/metrics`
just to create this check.
[Check creation](https://docs.digitalocean.com/products/uptime/how-to/create-checks/).

In that check's status page, create separate alerts and recipients:

| Alert | Example operator choice |
| --- | --- |
| Downtime | Notify after 2 minutes of failed checks. |
| Latency | Notify above 2,000 ms for 5 minutes; tune after normal traffic is measured. |
| Certificate expiry | Notify when fewer than 14 days remain. |

Latency alerts do not replace downtime alerts. Set the monitored email/Slack
destination explicitly and verify notifications with a staging check; creating
a check alone does not configure every notification.
[Uptime alerts](https://docs.digitalocean.com/products/uptime/how-to/create-alerts/).

Add an HTTPS check for an actual maintained test-preview URL to exercise the
preview routing path. Its app should have a stable 2xx public endpoint. Update
or disable that check when the PR closes, the preview expires or its URL changes;
intentional lifecycle cleanup is not a production outage. Rootless builds,
queues and GitHub feedback still need their application alerts even when this
URL is reachable.

Uptime runs outside the Droplet and can detect its public path failing while
local Prometheus/Grafana/Alertmanager are down. Review its current pricing and
regional availability before enabling checks. A successful dashboard check does
not establish that PostgreSQL, the worker and every PR preview are healthy.

### Inspect logs, containers and PostgreSQL

Run these on the VPS:

```sh
dc ps -a
dc logs --since 30m --tail 200 server worker router postgres
dc logs -f --tail 100 worker
docker ps -a --filter label=io.previa.managed=true
docker stats --no-stream
docker system df
df -h /var/lib/docker /var/lib/previa
journalctl -u docker --since '30 minutes ago'
```

`docker stats` includes dynamic builders/previews; `dc ps` does not. For one app
container, select its ID from the labelled inventory, then use
`docker logs --since 10m <container-id>` and `docker inspect` restricted to the
fields you need. Full container inspection contains environment values and can
expose repository secrets. Previa's build-log stream applies redaction, but an
app's arbitrary raw stdout from `docker logs` is not guaranteed to be redacted.

Control-plane logging uses structured JSON and correlation fields such as
`job_id`, `job_kind`, `pr_id`, `generation`, `revision`, `sha` and `operation_id`.
The logged `pr_id` is the internal database PR ID, not necessarily GitHub's PR
number. Use the PR page and recorded context to correlate the correct attempt.
Compose rotates most core service logs at 20 MB × 5 files; preview stdout uses
10 MB × 2 files. Build logs in PostgreSQL have 14-day retention, pruned hourly.
Egress and builder log configuration does not inherit Compose's core-service
rotation, so include those objects in host disk observations as well.

Read-only job and DB inspection, without dumping job payloads or secret tables:

```sh
dc exec -T postgres psql -U previa -d previa -c \
  "SELECT queue, state, count(*) FROM river_job GROUP BY queue, state ORDER BY queue, state;"
dc exec -T postgres psql -U previa -d previa -c \
  "SELECT id, kind, queue, state, attempt, max_attempts, scheduled_at, attempted_at FROM river_job WHERE state IN ('available','scheduled','retryable','running') ORDER BY scheduled_at LIMIT 30;"
dc exec -T postgres psql -U previa -d previa -c \
  "SELECT kind, state, count(*) FROM environment_operations GROUP BY kind, state ORDER BY kind, state;"
dc exec -T postgres psql -U previa -d previa -c \
  "SELECT application_name, state, count(*) FROM pg_stat_activity WHERE datname = 'previa' GROUP BY application_name, state;"
dc exec -T postgres psql -U previa -d previa -c \
  "SELECT pg_size_pretty(pg_database_size('previa'));"
```

River and durable operations handle retries; do not manually delete job/operation
rows to make a dashboard look clear. The production developer queue-ping tools
are deliberately disabled. Diagnose actual workloads through these metrics,
logs and read-only state instead.

### What scheduled work is live?

These run inside the worker through River, not in separate cron containers:

| Work | Current schedule / trigger |
| --- | --- |
| Authorized builds | Owner request, or Automatic eligibility after five quiet minutes. Separate bounded build queue. |
| PR discovery / mergeability | Installation/sync/webhook-driven jobs with bounded retries. |
| Route activation/removal, reports, artifact cleanup | Durable operations in separate route/report/cleanup queues. |
| Recovery of lost wake-ups and expired leases | Every minute, and at River leader startup. |
| Container/image/network reconciliation | Every five minutes, and at leader startup. |
| PR reconciliation | Every 15 minutes, and at leader startup; this is not the planned full installation-state reconciler. |
| Preview expiry | Every ten minutes; `ENV_TTL_HOURS=48` by default after latest publication, `0` disables TTL expiry. |
| Cache/workspace pruning | Hourly, with emergency pruning at most every five minutes when builds wait for disk. |
| Build log retention | Hourly; retains completed-attempt logs for 14 days. |
| Database/config/ACME backup | Host systemd timer; independent of River, described below. |
| TLS renewal | Traefik ACME; independent of River. |

### Make alerts reach a person

The repository loads **24 alert rules**, including server/worker availability,
queue delays, build failures, capacity, webhook errors, certificate expiry and
backup staleness. Prometheus evaluates them, but the stock `prometheus.yml`
comments out the Alertmanager target. Grafana charts or a firing Prometheus rule
are not proof that a notification was delivered.

Use an existing reachable Alertmanager, or deploy one separately. The following
is an **optional host configuration**, not a service already shipped in Compose.
Select an Alertmanager image version/digest you have tested and an actual receiver
before running it; consult the [official receiver configuration](https://prometheus.io/docs/alerting/latest/configuration/).

1. Create `/etc/previa/alertmanager.yml`. For a receiver that accepts Alertmanager
   webhook JSON, a minimal configuration is:

   ```yaml
   route:
     receiver: operations
     group_by: [alertname]
     group_wait: 30s
     group_interval: 5m
     repeat_interval: 4h
   receivers:
     - name: operations
       webhook_configs:
         - url: https://YOUR_ALERT_RECEIVER/alertmanager
           send_resolved: true
   ```

   A Slack/Discord/email receiver needs its native Alertmanager configuration,
   not this generic webhook payload. Keep receiver credentials in the protected
   config or mounted credential files; `/etc/previa` is backed up encrypted.
2. Make that file readable by the non-root user of the example container:

   ```sh
   chown 65534:65534 /etc/previa/alertmanager.yml
   chmod 600 /etc/previa/alertmanager.yml
   PREVIA_ALERTMANAGER_IMAGE='prom/alertmanager:<tested-tag>@sha256:<verified-digest>'
   # Replace both placeholders above with an actual verified image reference.
   docker run -d --name previa-alertmanager --restart unless-stopped \
     --network previa_control --user 65534:65534 \
     --read-only --cap-drop ALL --security-opt no-new-privileges:true \
     -v /etc/previa/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro \
     -v previa_alertmanager:/alertmanager \
     "$PREVIA_ALERTMANAGER_IMAGE" \
     --config.file=/etc/alertmanager/alertmanager.yml \
     --storage.path=/alertmanager --web.listen-address=:9093
   ```

   There is no published host port. Check container logs/readiness and the
   writable volume permissions for the selected image.
3. Copy `/opt/previa/deploy/observability/prometheus.yml` to
   `/etc/previa/prometheus.yml`. Enable its commented `alerting` block with target
   `previa-alertmanager:9093`, preserving the existing rule and scrape sections:

   ```yaml
   alerting:
     alertmanagers:
       - static_configs:
           - targets: ["previa-alertmanager:9093"]
   ```

4. Create `/etc/previa/observability.override.yml` to mount that host-specific
   Prometheus file, leaving the repository unchanged:

   ```yaml
   services:
     prometheus:
       volumes:
         - /etc/previa/prometheus.yml:/etc/prometheus/prometheus.yml:ro
   ```

   Prometheus's config must be readable inside its container, for example mode
   644 with no secrets embedded in that file. Redefine `dc` with the override for
   subsequent operations on this host:

   ```sh
   dc() {
     docker compose -p previa \
       -f /opt/previa/deploy/production/compose.yml \
       -f /etc/previa/observability.override.yml \
       --env-file /etc/previa/previa.env --profile observability "$@"
   }
   dc up -d --force-recreate --no-deps prometheus
   probe http://previa-alertmanager:9093/-/ready
   probe http://prometheus:9090/api/v1/alertmanagers
   ```

   If you already use an external Alertmanager, use its private reachable
   address instead and omit the container. Keep the override documented for a
   restore. The base backup/restore scripts still use their own base Compose
   command; this monitoring-only override does not change PostgreSQL.
5. Send a synthetic alert through the selected Alertmanager and verify receipt
   and resolution in the actual receiver. For the private container example:

   ```sh
   probe -H 'Content-Type: application/json' \
     -d '[{"labels":{"alertname":"PreviaNotificationTest","severity":"info"},"annotations":{"summary":"Previa alert delivery test"}}]' \
     http://previa-alertmanager:9093/api/v2/alerts
   ```

   Allow the configured grouping delay, check the notification, then confirm it
   resolves. This tests Alertmanager-to-receiver delivery; separately verify that
   Prometheus discovers Alertmanager and that a real test rule reaches it on
   staging. Do not stop live services merely to test a notification.

Use the DigitalOcean Uptime and host-monitoring steps above alongside actual
Alertmanager delivery. Prometheus and Alertmanager on the same failed VPS cannot
report that whole-host outage. PostgreSQL/node/container exporters and Loki are
not currently in the supplied stack; deployment of those is an additional choice.

Use [23 · Observability](23-observability-and-hardening.md) for metric details and
[24 · Incident runbook](24-incident-runbook.md) to respond to a firing alert.

## Backups and restore

### What database are we backing up, and where is it?

The production database is **the `previa` database inside the DigitalOcean
Droplet's `postgres` Compose service**, persisted in `previa_postgres` on the
Droplet's SSD. It is not a DigitalOcean Managed Database or a database in Spaces.
Spaces stores encrypted copies of PostgreSQL logical backups, configuration and
ACME archives. Uploading a dump there does not create another running database.

The backup includes application tables and River's job/state tables from one
consistent `pg_dump` snapshot. PostgreSQL continues serving while this dump runs.
Build containers, app filesystems, build caches and local images are not a second
source of durable application state; they are not copied by this backup.

### Step B1 — Generate a recovery key outside the VPS

On a **recovery workstation**, with age installed, outside the repository:

```sh
umask 077
mkdir -p ~/previa-recovery
age-keygen -o ~/previa-recovery/backup.agekey
age-keygen -y ~/previa-recovery/backup.agekey
```

Copy the printed **public recipient** (`age1...`) into the VPS's
`BACKUP_AGE_RECIPIENT` setting. Save the private `backup.agekey` in protected offline
recovery storage and keep a second recovery copy. The private identity is not
needed for normal backups and should not remain on the production VPS.

Separately escrow the complete repository keyring (`SECRETS_ENCRYPTION_KEYS`),
GitHub/DNS recovery credentials and production configuration. The age identity
opens the backup archives; the repository keyring opens the AES-GCM values inside
the database. They serve different purposes. Losing either can make part of the
restore unusable. Retain old key versions and identities while retained backups
still require them.

### Step B2 — Create a private DigitalOcean Spaces bucket and access key

In your DigitalOcean project, open **Spaces Object Storage → Create Bucket**:

1. Select **Standard Storage** for the documented upload/read/delete/restore
   workflow. Choose an available region near the Droplet and record its regional
   origin endpoint. `nyc3.digitaloceanspaces.com` below is only an example.
   Off-Droplet storage survives host loss; an additional copy in another region
   or provider is a separate recovery decision.
2. Select a unique bucket name, represented here by `YOUR_BUCKET`, and the
   production project. Keep objects and file listing private; do not add a
   public-read policy. Leave CDN disabled for these encrypted recovery files.
3. Create the bucket before configuring the restricted backup client. Save the
   actual bucket name and **origin** endpoint; the CDN hostname or
   `YOUR_BUCKET.region.digitaloceanspaces.com` is not the rclone API endpoint
   requested below.

The bucket lives outside the Droplet and is billed separately. PostgreSQL data
files remain on the Droplet. Reference: [Spaces bucket creation](https://docs.digitalocean.com/products/spaces/how-to/create/).

In **Spaces Object Storage → Access Keys → Create Access Key**, choose
**Limited access**, select this production bucket, grant **Read/Write/Delete**
and name the key `previa-production-backups`. Save the secret when shown; keep
a protected recovery copy. This grants the read/upload/delete operations used
by verification, restore and retention, without granting unrelated-bucket access.
[Spaces access keys and permissions](https://docs.digitalocean.com/products/spaces/how-to/manage-access/).

Limited keys are scoped to a bucket, not automatically to the `production/`
prefix. Use a different bucket/key for staging if those operators should not
access production archives. Current DigitalOcean limited-access keys cannot be
combined with `PutBucketPolicy` bucket policies; do not paste an AWS IAM policy
into this setup or create an AWS access key. Keep private object ACLs and test
the actual limited key.

Use `no_check_bucket=true` in rclone because the bucket already exists and this
key need not create or enumerate all account buckets. Avoid `rclone lsd
previa-backups:` as the permission test; list the selected bucket/prefix instead.

The script deletes current backup objects after `BACKUP_RETENTION_DAYS`.
If you enable versioning, separately manage noncurrent-version retention and
verify storage growth. A policy that prevents the script's deletion makes its
retention pass fail. Encryption protects archive contents, not deletion by a
credential with delete permission; a separately controlled second copy requires
its own backup/recovery procedure.

An absolute `BACKUP_REMOTE=/some/directory` works for a local drill, but a
directory on this Droplet's disk is not an off-server production backup.

### Step B3 — Configure rclone as the backup service's user

On the **VPS as root**:

```sh
rclone config
rclone config file
```

In the interactive editor, select options by their names rather than copying
version-dependent menu numbers:

| rclone field | Value |
| --- | --- |
| Remote name | `previa-backups` |
| Storage/backend | `s3`, the S3-compatible backend |
| Provider | `DigitalOcean` |
| Environment/runtime authentication | `false`; enter the selected Spaces key/secret interactively |
| Access key / secret access key | Values created in B2; never the DigitalOcean account password/API token |
| Region / location constraint | Leave empty for this provider; the actual region is in the endpoint |
| Endpoint | Actual regional origin, e.g. `nyc3.digitaloceanspaces.com`; no bucket/prefix or CDN domain |
| ACL | `private` |
| Advanced `no_check_bucket` | `true`; pre-created bucket with a limited key |

The protected config should have this shape; these credential values are
placeholders, not usable keys:

```ini
[previa-backups]
type = s3
provider = DigitalOcean
env_auth = false
access_key_id = YOUR_SPACES_ACCESS_KEY
secret_access_key = YOUR_SPACES_SECRET_KEY
region =
endpoint = nyc3.digitaloceanspaces.com
location_constraint =
acl = private
no_check_bucket = true
```

Keep the actual endpoint consistent with the selected bucket region.
[rclone's DigitalOcean configuration](https://rclone.org/s3/#digitalocean-spaces)
documents the blank region/location constraint and regional endpoint.

The default root configuration is normally
`/root/.config/rclone/rclone.conf`; the `config file` command confirms its actual
path. Protect it with mode 600. Configuring rclone only as your SSH user leaves
root's systemd backup service without that remote. Do not put the storage key in
Previa's repository secrets UI or a committed Compose file.

For the normal confirmed path:

```sh
chown root:root /root/.config/rclone/rclone.conf
chmod 600 /root/.config/rclone/rclone.conf
rclone lsf previa-backups:YOUR_BUCKET/production
```

An empty listing before the first backup is normal. A forbidden/signature error
is not: verify bucket scope, endpoint and credentials. The service runs without
your interactive shell exports; a remote only configured for `ops` or through
a temporary `RCLONE_CONFIG` export does not configure root's timer service.
Do not enable interactive config-password prompts for an unattended service
without separately provisioning its unlock mechanism.

Set these values in `/etc/previa/previa.env`:

```dotenv
BACKUP_AGE_RECIPIENT=age1YOUR_REAL_PUBLIC_RECIPIENT
BACKUP_REMOTE=previa-backups:YOUR_BUCKET/production
BACKUP_RETENTION_DAYS=30
```

Verify access on a disposable object in that prefix:

```sh
printf 'Previa backup access check\n' > /var/lib/previa/backup/access-check.txt
rclone copyto /var/lib/previa/backup/access-check.txt previa-backups:YOUR_BUCKET/production/access-check.txt
rclone cat previa-backups:YOUR_BUCKET/production/access-check.txt
rclone deletefile previa-backups:YOUR_BUCKET/production/access-check.txt
rm /var/lib/previa/backup/access-check.txt
```

Confirm upload, read and deletion succeed. This is a connectivity/permission test,
not yet a backup/restore proof.

### Step B4 — Take a real backup and enable the schedule

With the production PostgreSQL service and router running:

```sh
systemctl start previa-backup.service
systemctl status previa-backup.service --no-pager
journalctl -u previa-backup.service -n 50 --no-pager
rclone lsf previa-backups:YOUR_BUCKET/production
cat /var/lib/previa/backup/last-success
probe http://server:9090/metrics
```

A successful run stores **three files with the same UTC timestamp**, for example:

```text
previa-20261006T023000Z.dump.age
previa-acme-20261006T023000Z.tar.gz.age
previa-config-20261006T023000Z.tar.gz.age
```

`backup.sh` performs these operations in order:

1. Runs `pg_dump -U previa -d previa --format=custom --compress=6 --no-owner
   --no-privileges` in the PostgreSQL container and streams it into age encryption.
2. Archives `previa_acme` through the router image and encrypts the archive.
3. Archives `/etc/previa` and encrypts it, including its env file, App key, DNS
   token and any optional host-specific monitoring configuration stored there.
4. Copies the encrypted files to the remote. It removes encrypted copies older
   than the configured retention period.
5. Only after all steps succeed, writes `last-success` atomically. The server
   exports that file's modification time as
   `previa_backup_last_success_timestamp_seconds`.

Only encrypted files are uploaded. A failed run can leave partial files on the
remote; do not assume one `.dump.age` file means the entire three-file set
completed. Check matching artifacts, service success, timestamp and a restore.
The settings/ACME archives are not transactionally frozen at the exact database
snapshot instant; avoid rotating credentials/config while making a recovery set.

After the first complete backup succeeds:

```sh
systemctl enable --now previa-backup.timer
systemctl list-timers --all previa-backup.timer
systemctl cat previa-backup.timer
```

The installed timer starts at **02:30 UTC**, with up to **15 minutes randomized
delay**: **08:00–08:15 IST**. `Persistent=true` makes a missed scheduled run eligible
after the host returns; it does not produce backups while the machine is down.
To change the schedule, use `systemctl edit previa-backup.timer`, add a `[Timer]`
section that first clears `OnCalendar=` and then sets your new calendar expression,
reload systemd and restart the timer. Validate it with `systemd-analyze calendar`
and a fresh manual backup. A later full `setup.sh` writes the base units again,
so keep custom schedules in a systemd drop-in.

### Backup failure, retention and recovery objectives

`PreviaBackupStale` fires when the last complete backup is over 26 hours old;
`PreviaBackupNeverRecorded` covers a missing timestamp after its grace period.
Those notifications still require configured alert delivery. Optional
`BACKUP_FAILURE_WEBHOOK` receives the script's failure message even without
Alertmanager, provided the host and outbound connectivity are still alive.

If a run fails:

```sh
systemctl status previa-backup.service --no-pager
journalctl -u previa-backup.service -n 100 --no-pager
```

Check PostgreSQL/router availability, bucket permissions, root's rclone remote,
token expiry, age recipient and retention-deletion failures. Fix the cause, then
run `systemctl start previa-backup.service` again. Do not manually touch
`last-success` to silence an alert.

With successful nightly backups, the recovery point can be roughly **one day
old**, plus schedule jitter and backup duration; failures make that window longer.
Everything committed after the chosen dump's snapshot is lost in a database
restore. This setup does not archive WAL and does not provide continuous PITR.
For a smaller data-loss window, increase the tested backup frequency or design a
managed/PITR database deployment and revise backup/restore accordingly.

The historical small local drill took about 1 second to back up and 21 seconds to
restore. Those are test observations, not a production recovery-time guarantee.
Production recovery also includes finding keys, provisioning a new host,
downloading backups, DNS propagation and rebuilding previews. Measure it on your
actual database and off-host remote.

### Step B5 — Restore the database on an existing host

This is a **destructive database restore**, not an application restart. It
replaces current rows, jobs and sessions with the selected backup. Confirm the
intended recovery point and preserve a fresh backup of readable current state
before replacing it. Use a maintenance window and stop the backup timer/service
so it does not race recovery.

1. Choose the release that should run after recovery, and obtain its matching
   checkout/images. The restore script runs **that checkout's migrations**. For
   a schema rollback, select the previous release before running restore; do not
   restore a pre-upgrade dump with the unwanted new release's migration binary.
2. Put `BUILDS_PAUSED=true` in `/etc/previa/previa.env`. It must be one persistent
   entry, not only an ephemeral shell override. The script starts paused too,
   but later Compose commands would otherwise resume according to the file.
3. Stop the timer/service and retrieve the chosen encrypted set into a root-only
   directory. Example timestamp; substitute a real completed backup:

   ```sh
   systemctl stop previa-backup.timer
   systemctl stop previa-backup.service
   install -d -m 0700 /root/previa-restore
   PREVIA_BACKUP_STAMP=20261006T023000Z
   rclone copy previa-backups:YOUR_BUCKET/production /root/previa-restore \
     --include "*${PREVIA_BACKUP_STAMP}*"
   ```

4. Temporarily install the age identity from offline recovery storage as
   `/root/previa-restore/backup.agekey`, mode 600. Confirm the database dump and,
   when needed, matching config/ACME archives are present.
5. Stop app processes and the router, then move the existing generated preview
   route file out of Traefik's watched directory before restoring:

   ```sh
   dc stop server worker router
   if [ -f /var/lib/previa/router/previa-routes.yml ]; then
     mv /var/lib/previa/router/previa-routes.yml \
       /root/previa-restore/previa-routes-before-restore.yml
   fi
   ```

   Use your configured host router directory if it differs. Leave the dashboard
   route file intact. Route-file writes are deliberately monotonic: the current
   code refuses to replace a newer on-disk route version with an older restored
   database version. `restore.sh` does not reset this file, so omitting this
   preparation can leave stale preview routes after a successful DB restore.
   Keep the saved file outside the watched directory; Traefik loads every YAML
   file in that directory. The worker can now generate the restored table afresh.
6. If the current configuration/keyring remains valid for this dump, run the
   database restore:

   ```sh
   cd /opt/previa
   deploy/production/restore.sh \
     --identity /root/previa-restore/backup.agekey --yes \
     "/root/previa-restore/previa-${PREVIA_BACKUP_STAMP}.dump.age"
   ```

The script decrypts into a private temporary directory, stops server and worker,
ensures PostgreSQL is running, terminates connections to `previa`, drops/recreates
that database, runs `pg_restore`, applies the selected release's Goose/River
migrations, and starts worker/server with builds paused. It stops on an error;
inspect the failure before attempting to start everything again.

**It restores only the database dump.** It does not restore `/etc/previa`, ACME
files, Docker images, preview containers, metrics or rclone credentials. If you
need settings/certificates or have lost the whole host, follow B6 too.

After the restore:

```sh
probe http://server:9090/readyz
probe http://worker:9091/readyz
dc run --rm --no-deps migrate status
dc logs --since 10m worker
docker ps -a --filter label=io.previa.managed=true
```

The worker rewrites accepted route state and runs recovery/resource
reconciliation. Old database rows and the containers currently present may
differ; missing previews become unavailable and orphans may be cleaned after the
grace period. For investigation on an existing Docker host, use
`HOUSEKEEPING_DRY_RUN=true` before starting recovery, review the inventory, then
turn it off deliberately. A fresh host has no backed-up preview containers;
rebuild its previews from authorized owner requests after the control plane is
healthy. Do not promise restoration of a preview app's ephemeral filesystem data.

Once readiness, routes, permissions and repository-secret decryption are verified,
set `BUILDS_PAUSED=false` (and disable any temporary housekeeping dry run), then:

```sh
dc up -d --force-recreate --no-deps worker
probe http://worker:9091/readyz
systemctl start previa-backup.service
systemctl start previa-backup.timer
```

Confirm the new backup succeeds, restore the temporary age identity to offline
custody, and remove temporary decrypted files/private keys from the host. Retain
the original encrypted recovery set according to your retention policy.

### Step B6 — Recover configuration/certificates or replace the whole VPS

For a lost host, create a new **DigitalOcean Droplet** with a plain supported OS
and install the **chosen recovery release** at `/opt/previa` using Steps 0–3.
This primary recovery path uses the logical Spaces archives, not an assumption
that a provider disk snapshot contains the latest PostgreSQL state.

Record the replacement's ID/IP, attach the intended Cloud Firewall, verify SSH,
agent and disk capacity, and keep the backup timer stopped. If using a Reserved
IP, create the replacement in the same datacenter but **do not reassign it yet**.
Recover root's rclone access from offline credentials; this config was not in
the default archive. The bucket and its region/endpoint stay the same even though
the Droplet changed.

Define `dc` and `probe` from Step 7 in this new root SSH session. Retrieve the
exact matching encrypted set using B5's download step, and temporarily stage
the offline age identity using B5's key-installation step. Do not run the first
deployment's application startup or certificate reset during this recovery.

If the old Droplet still exists, stop/isolate its server, worker and router
before starting a replacement against the restored production identity. Two
independent copies of the database/worker must not process and report work for
the same production GitHub registrations. A provider outage does not prove the
old machine can never return.

Before starting app processes, retrieve/decrypt the matching archives in a
root-only directory. Stop server, worker and router if they exist:

```sh
dc stop server worker router
age -d -i /root/previa-restore/backup.agekey \
  -o /root/previa-restore/config.tar.gz \
  "/root/previa-restore/previa-config-${PREVIA_BACKUP_STAMP}.tar.gz.age"
# Inspect filenames privately; extracting replaces the current settings.
tar -tzf /root/previa-restore/config.tar.gz
tar -xzf /root/previa-restore/config.tar.gz -C /etc/previa
```

Review the restored env without publishing its values. Retain all encryption key
versions required by the database. Update any expired GitHub/DNS/storage
credentials, domains that changed, selected release/image names and
`DOCKER_GID` for the replacement host. Re-running `setup.sh` can fix directory/key
modes and Docker group, but it enables the timer again: stop it until recovery is
complete and reapply `BUILDS_PAUSED=true` before app startup.

On a **fresh** PostgreSQL volume, the restored `POSTGRES_PASSWORD` initializes the
role correctly. On an **existing** database volume, the env file alone does not
reset that role password; align it deliberately before expecting the application
URL to connect. Also recreate any expired private-registry login.

Restore ACME only into the correct host's certificate volume, with the router
stopped. For the provided pinned router image:

```sh
age -d -i /root/previa-restore/backup.agekey \
  -o /root/previa-restore/acme.tar.gz \
  "/root/previa-restore/previa-acme-${PREVIA_BACKUP_STAMP}.tar.gz.age"
docker volume create previa_acme
docker run --rm --network none -i --entrypoint tar \
  -v previa_acme:/acme \
  traefik:v3.7.13@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0 \
  -C /acme -xzf - < /root/previa-restore/acme.tar.gz
```

Use the router image from your chosen release if it differs. Preserve ACME's
restrictive file permissions. Do not delete production ACME state as part of
routine recovery; restoring a staging archive with a production env setting is
not a substitute for valid production issuance.

Pull/build all three release images, run the two `check-config` commands, and
start the database/router prerequisites:

```sh
dc up -d --wait postgres router
```

Then follow B5's route-file preparation and database restore command. On a fresh
host there may be no preview route file to move; on an existing host that step
removes the old route version. Bring back monitoring with
`dc up -d --wait` **while the file still says `BUILDS_PAUSED=true`**, and restore
any optional monitoring override/Alertmanager service. Validate internal readiness,
external HTTPS, GitHub delivery, repository-secret decryption and a real test
preview. For a replacement Droplet, finish the public cutover deliberately:

1. With valid restored certificates, test the new server before changing DNS,
   from your workstation:

   ```sh
   curl --resolve previa.example.com:443:YOUR_NEW_DROPLET_IPV4 \
     -fsS -D - -o /dev/null https://previa.example.com/
   ```

   This preserves the hostname/TLS check while selecting the new IP. If the
   certificate is expired/missing, validate private readiness first and complete
   production issuance after DNS reaches the new host; TLS-ALPN cannot validate
   a new dashboard host that the public DNS still sends elsewhere.
2. For ordinary public IPv4, change **both** Cloudflare dashboard and wildcard
   A records to the new address. For a Reserved IPv4, reassign it to this
   replacement through DigitalOcean Networking; DNS can retain that address.
   Reserved-IP recovery is manual and limited to its documented datacenter.
3. Verify `dig` from outside, normal HTTPS without `-k`, GitHub callback/webhook
   delivery, preview routing and Uptime checks. The GitHub registrations can
   retain their URLs when hostnames stay the same. Account for cached DNS and
   any external allowlist that used the old outbound IP.
4. Rebuild missing previews through authorized requests, resume deliberately and
   take a complete new Spaces backup. Enable the timer and verify monitor/alert
   targets reference the replacement Droplet/tag.
5. Keep the retired host stopped while validating recovery. Preserve needed
   evidence/backups before deleting it; inspect separately billed resources
   such as volumes, provider backup images, snapshots and Reserved IPs.

The live DB still has private hostname `postgres`; moving the Droplet does not
require changing it into a public IP or a Spaces URL. Data after the selected
dump is not recovered by changing DNS.

Rehearse this on isolated staging infrastructure. A copied production database
still names production installations/PRs: `BUILDS_PAUSED` does **not** mute GitHub
reporting or teardown. Use disposable GitHub registrations/repositories and test
domains, or an isolated drill such as the smoke harness with the App disabled;
do not let a restore rehearsal report to real production PRs accidentally.

### Step B7 — Optional DigitalOcean Droplet backups and snapshots

There are three different backup/recovery objects:

| Object | Contents / storage | Intended use |
| --- | --- | --- |
| Previa's encrypted archives | Logical PostgreSQL dump, `/etc/previa`, ACME; private Spaces bucket | The primary database/config recovery workflow in B1–B6. |
| DigitalOcean automated Droplet backup | Provider disk image, managed in the Droplet's Backups view | An additional OS/disk recovery option with the plan's own schedule and retention. |
| DigitalOcean manual snapshot | Disk image you deliberately capture and retain | An additional maintenance checkpoint; it is not the previous application release or continuous DB recovery. |

To enable provider backups, open this Droplet's **Backups** tab, choose the
available backup plan/schedule/retention, review its cost and enable it. Confirm
successful provider images and their timestamps in the control panel.
Their cadence is independent of `previa-backup.timer`. They do not upload the
three `*.age` files to your Spaces bucket or update Previa's backup-success
metric. [Enable Droplet backups](https://docs.digitalocean.com/products/backups/how-to/enable/).

For a deliberate maintenance snapshot:

1. Follow U1/U2: record the release/digests, pause builds, drain work and complete
   a fresh encrypted Spaces backup. Record its timestamp and chosen recovery
   release outside the Droplet.
2. Stop the backup timer/service and stop Compose services, so the snapshot
   does not capture active DB writes or an unexpectedly running worker:

   ```sh
   systemctl disable --now previa-backup.timer
   systemctl stop previa-backup.service
   dc stop
   shutdown -h now
   ```

   This is a maintenance outage and disconnects SSH. Dynamic containers may
   have been running until OS shutdown; a snapshot is not a guarantee that
   their ephemeral preview state will be restored or served correctly.
3. In the powered-off Droplet's **Snapshots** tab, create a named snapshot such
   as `previa-before-v0.2.0-20261006`. Record its ID and source disk size.
   [Droplet snapshot procedure](https://docs.digitalocean.com/products/snapshots/how-to/snapshot-droplets/).
4. After capture, power the Droplet on, reconnect, redefine the `dc` helper,
   confirm `BUILDS_PAUSED=true`, and start the stack with `dc up -d --wait`.
   Verify routing/resources/readiness, rebuild missing previews if necessary,
   then resume through U5. Run a fresh backup and enable the timer again.

Snapshots/disk backups contain host credentials and an earlier copy of the DB.
Restrict provider account access. Restoring one can start old services/settings
and rewind *all* captured disk state. A live provider backup does not prove
application-level consistency or restore of the latest DB transaction.

If using a provider image during recovery, create an isolated replacement first;
prevent outbound production integrations until the old services are stopped,
the intended configuration/release is selected and builds are paused. Do not
restore an arbitrary image over the healthy live Droplet as an image rollback.
For actual DB recovery, prefer the latest completed encrypted dump and B5/B6's
schema/keyring/route reconciliation. Test provider-image recovery separately
before relying on it.

Provider backups/snapshots do not supply WAL-based PITR for this self-hosted DB.
Spaces in the same provider/account protects against Droplet loss, but it is not
an independent account/provider recovery copy. Keep offline identities/account
recovery and add a separately controlled copy if that failure scope must be
covered.

## Upgrades and rollback

### What an upgrade changes

A Previa upgrade changes the selected checkout, three versioned images and,
possibly, application/River migrations and Compose configuration. Pulling images
alone does not replace running containers. `dc up` reconciles their definitions;
`dc restart` only restarts their existing definitions. Changing only a GitHub
release or pushing code to GitHub does nothing to the running VPS.

This is a single-host deployment with one server, worker and router. It does not
provide a rolling multi-server deployment or automatic traffic failover. Plan a
maintenance window for restarts, schema changes and especially router/egress work.
Healthy existing previews can survive a core server/worker upgrade, but a router
restart interrupts their request path and an egress restart can interrupt their
outbound dependency calls.

### Step U1 — Prepare and record the previous release

Before touching the host:

1. Confirm the candidate commit passed CI and the release notes describe schema,
   configuration, third-party image and runtime changes. Test the exact candidate
   and an upgrade/restore on staging. A successful release workflow alone does
   not certify these checks.
2. Record the current checkout commit/tag, server/worker readiness versions,
   actual running image names/digests, schema versions and any host override.
   Keep the previous images available; do not prune them until the rollback
   window has ended.
3. Confirm you can retrieve/decrypt backups and have the old release artifacts.
   Decide whether this release supports image rollback on the newer schema or
   requires a database restore.
4. Notify your operators of the intended maintenance and choose a recovery point.

On the VPS, capture non-secret version/state information:

```sh
cd /opt/previa
git rev-parse HEAD
git describe --tags --exact-match
dc images
probe http://server:9090/readyz
probe http://worker:9091/readyz
dc run --rm --no-deps migrate status
docker inspect --format '{{.Config.Image}} {{.Image}}' previa-egress
```

Keep a protected copy of the current env/config as well as its encrypted backup,
but avoid posting those values with the non-secret deployment notes.

### Step U2 — Pause new builds, drain and make a fresh backup

Set `BUILDS_PAUSED=true` in `/etc/previa/previa.env`, then:

```sh
dc up -d --force-recreate --no-deps worker
probe http://worker:9091/readyz
systemctl start previa-backup.service
systemctl status previa-backup.service --no-pager
journalctl -u previa-backup.service -n 30 --no-pager
```

Confirm the worker is paused, running builds drained/cancelled, and all three
fresh encrypted backup artifacts reached off-host storage. New build requests
wait while paused; cleanup, routing, reports and housekeeping keep running.
For a migration that needs an entirely quiet database, stop server/worker after
the backup and before migrating, as rehearsed on staging. Pausing builds alone
is not a database write freeze.

### Step U3 — Select, validate and start the candidate

Fetch/check out the intended release. Example only; use your actual tested tag:

```sh
cd /opt/previa
git fetch --tags origin
PREVIA_NEXT_RELEASE=v0.2.0
git checkout --detach "$PREVIA_NEXT_RELEASE"
nano /etc/previa/previa.env
```

Set `PREVIA_VERSION` to that same tag. Apply documented required settings while
retaining the database password, encryption keyring, session secret and
production ACME state. Keep `BUILDS_PAUSED=true` during validation.

For published images:

```sh
dc config --quiet
dc --profile images pull
dc run --rm --no-deps server check-config
dc run --rm --no-deps worker check-config
```

For a manual source deployment, replace the pull command with
`dc --profile images build server worker egress` from the chosen checkout.
Then, once the running PostgreSQL is healthy, run an explicit migration gate:

```sh
dc run --rm --no-deps migrate
dc up -d --wait --wait-timeout 180
probe http://server:9090/readyz
probe http://worker:9091/readyz
dc run --rm --no-deps migrate status
```

The regular `up` also honors Compose's migration dependency; repeating `up`
migrations is idempotent. If the migration fails, do not resume builds or assume
all pending migrations are undone. Earlier migrations may already have committed.
Inspect the migration logs/status and choose the rollback path below.

These commands upgrade Previa; they are not a PostgreSQL **major-version upgrade**
procedure. The supplied service is PostgreSQL 17. A future database-major change
needs its own tested dump/restore or `pg_upgrade` plan and compatible clients,
not simply a replacement image tag on the old data directory.

### Step U4 — Account for the separately managed egress proxy

**Important lifecycle detail:** the worker's `Gateway.Ensure` reuses an existing
`previa-egress` container by name. Updating `EGRESS_IMAGE` or pulling its new tag
is not a check/replacement of that running container's image. Compare:

```sh
docker inspect --format '{{.Config.Image}} {{.Image}}' previa-egress
```

If the existing proxy remains compatible and the release does not require its
replacement, record the intentional version difference. If the egress binary
changed and must be upgraded, use a separate, rehearsed maintenance procedure:

1. Keep new builds paused and wait until no builder is running. Stop the worker
   so it cannot race proxy creation while you inspect/remove the old proxy.
2. Save the proxy's network names and **IPv4 addresses** privately:

   ```sh
   docker inspect --format '{{json .NetworkSettings.Networks}}' previa-egress \
     > /root/previa-egress-networks-before.json
   ```

3. Confirm the selected new egress image is present locally. Remove only the old
   `previa-egress` container, then start the worker paused so its startup creates
   the proxy from the selected image. Do not remove the preview networks/images.
4. Reattach it to each surviving preview network using the recorded IP where
   required. Preview containers already contain their proxy's IP in environment
   variables; assigning a different address does not rewrite those variables.
   The relevant manual Docker operation is:

   ```sh
   docker network connect --ip SAVED_IPV4_ADDRESS SAVED_PREVIEW_NETWORK previa-egress
   ```

   Substitute the recorded address/name and verify that address is available.
   If the new proxy was attached at a different address, stop work and deliberately
   reconcile that attachment; do not blindly issue duplicate connections.
   Alternatively, tear down/rebuild affected previews during the maintenance
   window so their new runtime environment uses the new endpoint.
5. Verify the proxy image, preview readiness, outbound access and isolation, then
   resume builds. Include the same proxy/network plan if later rolling back it.

This is not an automatic zero-downtime proxy rollout. The shared container and
hardcoded host topology also mean that blindly scaling workers onto another VPS
is not supported by this deployment; the roadmap's multi-host runtime work needs
its own architecture and tests.

### Step U5 — Resume only after live checks pass

Verify public HTTPS, sign-in, GitHub delivery, a disposable build, PR feedback,
monitoring and cleanup. Check that existing preview routes still work. Then set
`BUILDS_PAUSED=false` and recreate the worker:

```sh
dc up -d --force-recreate --no-deps worker
probe http://worker:9091/readyz
systemctl start previa-backup.service
```

Record the deployed commit/tag, actual image versions, schema, checks, backup
stamp and any remaining limitations. Keep the previous release and pre-upgrade
backup until you no longer need that rollback path.

### If something goes wrong: choose the recovery action

| Failure | First action | Does it need database restoration? |
| --- | --- | --- |
| Mistyped env value, wrong key permissions or unreachable registry | Keep builds paused, inspect logs and `check-config`, correct config/files and recreate the affected service. | Usually no. |
| New application binary is broken, schema is compatible with the previous release | Revert checkout and image tag, keep the current database and keys, recreate server/worker and verify. | No, if compatibility has been verified. |
| New schema/data change cannot be read safely by previous binaries | Select the previous release and restore the pre-upgrade encrypted backup. | Yes; writes after that recovery point are lost. |
| Database corruption or accidental deletion | Preserve evidence/readable state, choose a known completed backup and use B5/B6. | Yes. |
| Host lost or compromised | Provision a replacement host, rotate affected credentials and use B6. | Restore on the replacement host. |
| One preview app has a bad change | Its failed rebuild normally keeps the old preview; rebuild an appropriate commit through the authorized app workflow. | Not a reason to roll back Previa's database. |
| Docker disk/memory exhausted | Pause builds, inspect capacity, use controlled teardown/pruning and adjust limits. | Usually no; see the incident runbook before deleting data. |
| Traefik DNS/certificate failure | Check public DNS, port 443, token permissions and ACME logs; repair the routing/certificate path. | Usually no; preserve production ACME state. |

### Application/image rollback, preserving the database

Production migrations run **forward only**. `previa-migrate` accepts `up` and
`status`, not `down`. `database.CheckSchema` refuses an older schema but accepts a
newer one; this version check does not prove every older binary understands every
new schema. The repository's intended compatibility window is the previous
release, and must be verified in release notes/tests for the actual candidate.

While builds remain paused, return to the previously recorded checkout/tag and
set `PREVIA_VERSION` back to that tag. Example:

```sh
cd /opt/previa
PREVIA_PREVIOUS_RELEASE=v0.1.0
git checkout --detach "$PREVIA_PREVIOUS_RELEASE"
nano /etc/previa/previa.env
# Set PREVIA_VERSION to the previous tag; keep BUILDS_PAUSED=true.
dc --profile images pull
dc run --rm --no-deps server check-config
dc run --rm --no-deps worker check-config
dc up -d --wait --wait-timeout 180
probe http://server:9090/readyz
probe http://worker:9091/readyz
```

Use previous local images/builds instead if this host did not deploy from GHCR.
Restoring the previous checkout also restores its Compose/config definitions;
check your host-specific overlay remains compatible. Keep encryption key versions
needed for data written since the upgrade rather than blindly restoring an old
keyring. The newer compatible schema stays in place. Handle an upgraded egress
proxy separately if its compatibility requires reverting it too.

Once external HTTPS, current state and a test preview work, resume builds using
U5. There is no automatic rollback controller in this repository.

### Schema/data rollback using the pre-upgrade backup

1. Keep `BUILDS_PAUSED=true`; stop accepting writes as required by your incident
   plan. Preserve logs and a fresh readable-state backup if possible.
2. Select the **previous** release checkout/images and its compatible settings.
   Confirm the pre-upgrade backup timestamp and that offline keys open it.
3. Restore that database using B5, and matching config/ACME state using B6 only
   where needed. The selected old migration executable will bring that dump to
   the old release, rather than reapplying the unwanted new migration.
4. Verify restored schema, repository secrets, routes, real GitHub access and
   reconciliation. Expect previews/rows created after the dump to differ or be
   absent, and expect lost writes after the snapshot.
5. Rebuild missing previews, resume deliberately, take a fresh full backup and
   record the loss/recovery point.

Do not use development `make migrate-down` as a production rollback substitute.
Database restoration and binary rollback have different data-loss consequences.
Use [24 · Incident runbook](24-incident-runbook.md) for diagnosis before selecting
one of them.

### Preserve preview networks when recreating Traefik

Docker connections added by the worker to `previa-router` are not in Compose's
static `networks: control` definition. A plain router restart retains them, but
**recreating** its container loses the extra preview-network attachments. Startup
route synchronization writes YAML; it does not automatically reattach all accepted
preview networks. Router health can be green while those backends are unreachable.

Before a router recreation on a host with live previews, pause builds, stop the
worker to prevent concurrent network changes, and save the router's network list:

```sh
dc stop worker
docker inspect --format '{{json .NetworkSettings.Networks}}' previa-router \
  > /root/previa-router-networks-before.json
dc up -d --force-recreate --no-deps --wait router
```

For every saved preview network that still exists, reattach the new router:

```sh
docker network connect SAVED_PREVIEW_NETWORK previa-router
```

Substitute each real network name from the saved inventory. Do not reattach
`previa_control`, which Compose already attached. The router's preview-side IP
need not match its old value; its control-side static IP must still match the
trusted-proxy/dashboard configuration. Verify each surviving preview externally,
then restart the worker with the persistent pause setting still enabled and
resume deliberately after checks. If the original router container is already
gone, inspect surviving managed preview networks and the accepted route table
to reconstruct the required attachments. A fresh host instead needs its missing
previews rebuilt.

This procedure applies to router image/config changes, DNS-token mount refreshes
and rollback/restore operations that recreate the router. The first staging-to-
production conversion happens before real previews are built, so it has no live
preview attachments to preserve.

### Configuration and credential changes between releases

For an env-only change with no schema dependency, validate config and use
`dc up -d --force-recreate --no-deps server worker` as appropriate for the affected
processes. Replacing a mounted credential file can change its inode; recreate the
consumers so the bind mount opens the new file. Do not rely on `restart` to refresh
changed env values or a replaced credential mount.

- **GitHub App key:** generate/install the new key with UID 65532 and mode 600,
  recreate server/worker, verify GitHub calls, then retire the old App key.
- **Webhook secret:** change the host setting and App webhook secret together,
  recreate server, then redeliver any deliveries that failed during the change.
- **OAuth/session secret:** update and recreate server; verify sign-in. Changing
  session-related secrets may require users to authenticate again.
- **DNS token:** replace its protected file and recreate router during the planned
  routing interruption, preserving preview network attachments as above; verify
  issuance/renewal behavior without deleting ACME.
- **Spaces backup key:** create a replacement limited key for the same bucket,
  update root's `rclone config` interactively, test listing/upload/read/delete,
  then run a full backup and verify its timestamp/artifacts. Only then retire
  the old key in DigitalOcean and update offline recovery credentials. This
  does not require recreating server/worker; it changes the backup client's
  authentication. Revoke a compromised key immediately and diagnose any gap.
- **Repository encryption keys:** add a new version, make it active on both
  server/worker, recreate them, and run the maintenance command:

  ```sh
  dc run --rm --no-deps --entrypoint /usr/local/bin/previa-secrets-reencrypt server
  ```

  Retain old key versions until re-encryption reports no current values using
  them, **and** retain a recovery copy for older backups. Take a fresh backup after
  rotation. See [07 · Repository secrets](07-repository-secrets.md).

## DigitalOcean capacity, billing and ongoing operations

### Resize the existing Droplet when measured demand needs it

First distinguish the constraint: queue concurrency, configured preview budget,
host CPU/RAM, disk free space and a slow Dockerfile are different bottlenecks.
Look at DigitalOcean metrics, `docker stats`, Grafana queue/build panels and
waiting reasons. Raising concurrency without resources can make every build
slower or exhaust memory.

For a planned resize:

1. Use U1/U2 to record the release/config, pause and drain builds, and complete
   a new encrypted Spaces backup. Retain the tested recovery path. An optional
   pre-maintenance snapshot uses B7.
2. Disable the backup timer while services are stopped, stop core services and
   gracefully shut down the Droplet:

   ```sh
   systemctl disable --now previa-backup.timer
   systemctl stop previa-backup.service
   dc stop
   shutdown -h now
   ```

3. In the Droplet's **Settings → Resize configuration**, or its **Actions →
   Resize** option, choose a supported larger resource configuration. CPU/RAM-only
   changes preserve more disk flexibility; disk increases cannot be reversed.
   Available moves depend on its current configuration family. Plan a maintenance
   outage rather than assuming this operation is live.
   [DigitalOcean resize procedure](https://docs.digitalocean.com/products/droplets/how-to/resize/).
4. Power on, reconnect, redefine the production helpers and inspect
   `docker info`, `free -h` and `df -h /var/lib/docker`. Confirm the chosen address,
   firewall and monitoring target. Keep `BUILDS_PAUSED=true` until checks pass.
5. Start core services with `dc up -d --wait`. Check PostgreSQL schema, both
   processes' readiness, router/network attachments, egress and each surviving
   preview. Reboot does not promise automatic restart of on-failure previews;
   rebuild unavailable previews through their authorized workflow.
6. If increasing application capacity, deliberately edit `BUILD_MEMORY`,
   `MAX_CONCURRENT_BUILDS`, `PREVIEW_MEMORY_BUDGET`, `PREVIEW_CPU_BUDGET` and the
   per-repository cap, leaving host/replacement headroom. The guide uses fixed
   budgets: resizing alone does not change them. Validate and recreate the worker,
   then resume through U5, take a new backup and enable the timer.

A second Droplet does not automatically share this host's Docker images,
accepted routes, preview networks or disk inventory. Do not enable a Droplet
autoscale pool or deploy extra workers on unrelated Docker hosts as though the
current code implements a distributed preview runtime.

### Track the resources that remain billed

| Resource | Why it is present / what to review |
| --- | --- |
| Droplet | Compute/storage allocation for the running platform and all previews. Stopping containers or powering off the Droplet is not the same as deleting its billed allocation. |
| Spaces | Encrypted archives, retained objects/versions and relevant transfer; independently billed from the server. |
| Provider backups/snapshots | Optional disk recovery and retained images; inspect old checkpoints after their recovery window ends. |
| Reserved IP / attached volumes | Optional independent resources; review their allocation and current pricing when retiring a host. |
| Uptime | Public checks and notification configuration; review its check plan. |
| Domains / GitHub packages / notification service | Separate services with their own access and usage rules. |

Use current [Droplet pricing](https://docs.digitalocean.com/products/droplets/details/pricing/),
[Spaces pricing](https://docs.digitalocean.com/products/spaces/details/pricing/)
and [Uptime pricing](https://docs.digitalocean.com/products/uptime/details/pricing/)
for the selected resources/region. This guide does not assume historical free
tiers or quote a fixed monthly total. Creating another Docker preview draws from
the existing Droplet; its storage/traffic still contributes to usage.

### Maintain the host and prove recovery

Keep Ubuntu, Docker, pinned infrastructure images and Previa releases maintained.
Use a tested maintenance window for kernel/Docker restarts, with a backup and
the reboot checks above. Test rootless BuildKit and isolation on staging after
relevant kernel/runtime updates.

Review failed jobs, disk/cache growth, active preview lifetimes, certificate
renewal, complete backup timestamps and alert recipients routinely. Rehearse a
Spaces restore on a separate isolated Droplet and record the actual recovery
time/data-loss window. Keep operator SSH/account recovery and offline encryption
identities current. Compose restart policies do not provide automatic failover
when this single server fails.

## CI and releases

### Exactly which events trigger what?

| Event | Workflow / result | Deploys to the VPS? |
| --- | --- | --- |
| Push/merge a commit into `main` | `CI` runs validation, test jobs and temporary runner image builds/scans. | **No.** |
| Open/update a pull request | `CI` runs; fork PRs do not run its privileged real-Docker suite. | **No.** |
| Run `CI` manually in Actions | `workflow_dispatch` runs the same validation workflow. | **No.** |
| Push a tag matching `v*.*.*`, such as `v0.1.0` | `Release` builds release archives/SBOMs and then pushes all three versioned multiarch images to GHCR. | **No.** |
| Publish the draft GitHub Release | Makes the release page/assets visible according to GitHub release behavior. | **No.** |
| Re-run a failed Release workflow job | Retries the selected publication steps. | **No.** |
| Operator changes the VPS checkout/env and runs pull/build plus `dc up` | Uses the selected release on the VPS. | **Yes, manually.** |
| Push to a repository being previewed by a running Previa instance | Previa webhook policy: Manual waits for an owner request; Automatic waits five quiet minutes. | May update a **PR preview**, not Previa's own server deployment. |

`CI`'s push filter is specifically `branches: [main]`, not every branch/tag.
The tag-driven `Release` workflow does **not** depend on a successful `CI`
workflow, and creating a tag is not a substitute for running CI on its exact
commit. There is no workflow that SSHes into the VPS, changes its env file,
executes its Compose commands, or automatically restores a failed rollout.

DigitalOcean's role here is hosting the Droplet and Spaces bucket. There is no
App Platform repository integration or provider deploy button managing this
Compose application. A reboot restarts/reconciles the installed stack; it does
not fetch the latest GitHub commit. The operator chooses and applies releases.

### Set up GitHub before the first release

1. Ensure Actions is enabled and `.github/workflows/ci.yml` and `release.yml` are
   present on the repository's default branch. Open a PR or push `main`, then
   inspect all results in **Actions**.
2. Configure main-branch protections/rulesets with the required CI checks your
   repository exposes. Review skipped fork-only checks rather than treating a
   skipped real-Docker job as a successful integration proof.
3. Create the GitHub environment named **`release`**, configure allowed release
   tags and required reviewers where your GitHub plan/repository visibility
   supports them. The workflow's `environment: release` alone does not configure
   an approval gate. See [GitHub environment protection](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).
4. Confirm the workflow can use its declared job permissions. It uses the
   automatically supplied **`GITHUB_TOKEN`** for Releases/packages/attestations;
   there is no repository-specific PAT or live production credential required
   by the checked-in workflows.
5. Decide whether GHCR packages are public or private. Public source does not mean
   every package is automatically public. Check package access after publication;
   configure the VPS's read credential if private.
6. Validate the exact release candidate and document migration/rollback behavior,
   configuration changes and the pending live checks. Run the smoke test against
   that candidate's images and confirm all current schema checks pass.

Keep **production** OAuth/App secrets, database password, DNS token, repository
keyring and backup identity out of these workflows. The workflows use disposable
PostgreSQL settings and do not deploy the production env file. A future deployment
workflow would need its own explicit host access credentials and authorization;
that is not part of the current CI.

### What CI actually checks

The `checks` job first generates template Go packages so a fresh checkout can
resolve local imports, verifies modules are tidy, generates assets/templates/sqlc code,
builds, runs `go vet`/golangci-lint and govulncheck, migrates disposable PostgreSQL,
runs race-enabled Go tests, and checks Prometheus alert rules and dashboard queries.
A separate browser job installs isolated Playwright tooling and runs UI regressions.
The image job builds server/worker/egress images on the runner and scans the
server and worker with the digest-pinned official Trivy container for fixable
HIGH/CRITICAL vulnerabilities. It shares a temporary vulnerability-DB cache
between scans and avoids downloading a separate scanner binary. These CI
image builds are not pushed to the production registry by `ci.yml`.

The `real-docker` job runs builder, runtime, routing, housekeeping, complete
pipeline and production-stack smoke suites. It runs on pushes/manual runs and
same-repository PRs, not on fork PRs, and needs the checks job. Those tests use
runner Docker resources and disposable data, not the production VPS.

### Publish a versioned release

From your workstation, on a tested commit that has been pushed to GitHub:

```sh
git switch main
git pull --ff-only origin main
git status
git rev-parse HEAD
# Confirm this exact commit passed CI before tagging it.
PREVIA_RELEASE=v0.1.0
git tag -a "$PREVIA_RELEASE" -m "Previa $PREVIA_RELEASE"
git push origin "$PREVIA_RELEASE"
```

Use a new version that does not already exist. In **Actions → Release**, approve
the environment jobs if protection requires it, then verify completion:

- GoReleaser creates a **draft** release with Linux amd64/arm64 command archives,
  docs/deploy files, SHA-256 checksums and SBOMs. The commands include server,
  worker, migrate, route-sync, housekeeping, secrets-reencrypt and egress.
- After the binary job, the image matrix pushes
  `ghcr.io/<actual-owner>/previa-server:<tag>`, `previa-worker:<tag>` and
  `previa-egress:<tag>` for `linux/amd64` and `linux/arm64`, with provenance/SBOM
  attestations. The owner is derived from the GitHub repository owner, not from
  the differently spelled historical Go module import path.
- Images are pushed even while the GitHub Release is still a draft. A maintainer
  inspects assets, checksums, notes and image tags and deliberately publishes
  the draft. Nothing then changes on the VPS until manual deployment.

A first version such as `v0.1.0-rc.1` is still a matching tag pattern; use it if
publishing a staging candidate. Verify candidate handling and release notes rather
than presenting an RC as production acceptance.

### Deploy manually, with or without the release workflow

**From a published release:** SSH to the VPS, use Fresh host setup for a new host
or U1–U5 for an existing host, select the same tag/checkout, edit the protected env,
pull the images, validate, migrate, start paused, perform live checks and resume.
Only the host needs production secrets. **Publishing first and deploying later
is the current intended workflow.**

**Without publishing images:** check out the reviewed/tested source on the VPS,
choose an explicit version identifier in its env, and build:

```sh
dc --profile images build server worker egress
```

Then use the same config/migration/start/verification gates. For an existing host,
still take a backup and retain the prior local images/checkout before replacement.
A tag name is not enough to verify a locally built image: record its source commit
and actual digest. Do not build from uncommitted production edits.

**Manually run CI:** use GitHub **Actions → CI → Run workflow** and select the
appropriate branch. This does not deploy. Release has no `workflow_dispatch`
trigger in its checked-in YAML; its normal entry point is a pushed matching tag.
A failed existing run can be retried in Actions after diagnosing it.

### If automatic deployment is wanted later

It needs a new, explicitly designed deployment workflow or supervised deployment
agent. No such code is added here. Its necessary behavior would be to select a
verified release, serialize deployments to this one host, enforce any desired
approval/environment gate, authenticate to the host/registry, take and verify a
fresh backup, apply version/config, validate/migrate, recreate services, verify
internal and external health, resume deliberately and report the result. It also
needs an explicit distinction between compatible image rollback and destructive
schema recovery; automatically restoring old data on any failed health check is
not the current behavior.

Put any future SSH/deployment token into that deployment environment's protected
secrets, with host identity verification and scoped access. That future token is
different from the built-in `GITHUB_TOKEN` and from Previa's GitHub App key.
Until such a workflow is implemented/tested, every production rollout is manual.

For a future SSH deployment to this Droplet, the environment would normally hold
the production host/user, a dedicated deployment SSH private key and verified
SSH host-key/known-hosts data; a private GHCR pull credential is separate.
DigitalOcean API access is only needed if the workflow actually provisions or
changes provider resources. Do not give the application a provider token simply
to run Docker, and do not assume hosted GitHub runners can reach an SSH rule
restricted to your workstation IP. The runner's authorized network path and the
deployment account's Docker/root privileges need explicit implementation.
These are design requirements, not secrets consumed by the current workflows.

## Local staging smoke test

On a **development/staging machine**, with Docker and age available:

```sh
make release-images PREVIA_VERSION=YOUR_TEST_TAG
make prod-smoke PREVIA_VERSION=YOUR_TEST_TAG
```

The harness uses the production Compose file plus `compose.smoke.yml`, project
`previa-smoke`, isolated temporary state, loopback ports `18080/18443`, Grafana
`13000`, fake/disabled GitHub configuration and an unreachable ACME endpoint. It
rehearses startup, security headers, private listeners, metrics, graceful stops,
core-image upgrade/rollback and encrypted database backup/restore. It is not a
hosted GitHub or real Let's Encrypt test. It also does not prove rolling out a
changed egress proxy to surviving real previews.

`SMOKE_OLD_DUMP` can name an older `pg_dump -Fc` file for the older-schema restore
drill. `SMOKE_KEEP=1` retains the isolated stack for inspection rather than normal
cleanup. Use disposable data and a separate host; the harness is not a production
backup or acceptance substitute.

The tracked `smoke.sh` asserts Goose schema **20** after initial migration,
upgrade and an optional older-backup restore, matching
`internal/platform/database/schema.go` and the current migration set. When adding
a migration, update those three expectations alongside the application's schema
requirement. The earlier 43/43 result in the release report is historical; run the
current harness against the exact candidate images before treating its gate as
passed.

## Security notes

- Use a host dedicated to Previa. The worker's Docker socket is root-equivalent
  access by design; the server, router, builders and previews are not given that
  socket. Do not share the daemon with unrelated sensitive workloads.
- Keep ports 5432, 8080, 8082, 9090/9091, Prometheus and Alertmanager private.
  Grafana's host binding stays loopback. Only SSH and Traefik 80/443 are required
  inbound on this host; preview container ports are never published individually.
- Untrusted previews run on their own networks, with non-root/read-only defaults,
  bounded resources and public-address-only proxy egress. Rootless BuildKit has
  its own documented namespace/security-profile requirements; it is not granted
  a privileged Docker socket. Validate build isolation on the actual host kernel.
- Keep previews on a separate registrable domain. Previa uses host-only secure
  session cookies; do not put the dashboard beneath the wildcard preview domain.
- Add repository secrets through the authenticated **repository Secrets** UI,
  not the production control-plane env file. They are encrypted in PostgreSQL,
  injected only into authorized preview runtimes, withheld from fork sources and
  not provided as Dockerfile build secrets. Do not reuse production DB/App/DNS
  credentials as a preview application's credentials.
- Current public-only proxy egress does not implement owner-approved private or
  raw TCP database access for preview apps. A managed DB URL added as a preview
  secret does not automatically make that protocol/network reachable. Backend
  previews and controlled outbound connections remain roadmap work.
- Restrict GitHub/App and DNS access to their intended repositories/zones. Keep
  key files and the env file out of Git, CI output and full inspection dumps.
  Admin Docker access can still read container environments; file modes do not
  turn the Docker administrator into an untrusted user.
- Keep OS/Docker/provider credentials maintained, review image vulnerability
  results and retain recovery access. Host-level availability and a full restore
  drill are operational responsibilities, not guarantees provided by Compose.
- Do not run `dc down -v`, remove the PostgreSQL volume, or broadly prune all
  Docker objects to resolve a live incident. Use the incident runbook, controlled
  teardown and instance-labelled cleanup. Pause builds before capacity recovery.

## Files behind these instructions

| Source | Behavior checked for this runbook |
| --- | --- |
| [README](../README.md), [DESIGN](../DESIGN.md) | Single-VPS architecture, PostgreSQL/River, runtime isolation and scope. |
| [Production Compose](../deploy/production/compose.yml) | Services, profiles, health dependencies, mounts, ports, resources and pinned images. |
| [Production env template](../deploy/production/previa.env.example), [config loaders](../internal/platform/config/config.go) | Required/generated settings, defaults, file checks and production validation. |
| [Setup script](../deploy/production/setup.sh) | Packages, directory ownership, generated secrets, Docker group and backup timer. |
| [Dockerfile](../Dockerfile), [GoReleaser](../.goreleaser.yaml) | Embedded presentation, image targets, static commands and release archives. |
| [Server](../internal/app/server.go), [worker](../internal/app/worker.go), [pipeline](../internal/app/pipeline.go) | Webhooks/queue production, actual execution dependencies, capabilities, startup and shutdown. |
| [Runtime hardening](../internal/runtime/hardening.go), [builder](../internal/builds/sandbox.go), [egress lifecycle](../internal/platform/egress/gateway.go) | Dynamic Docker objects, restart policies, resource boundaries and proxy reuse. |
| [Routing](../internal/routing/controller.go), [housekeeping schedules](../internal/housekeeping/settings.go) | Accepted routes, reconciliation/expiry/pruning and recovery. |
| [Metrics](../internal/platform/metrics/metrics.go), [state collection](../internal/app/observe.go), [River observations](../internal/platform/queue/observe.go) | Traffic/build/job counters, queue state, capability and backup gauges. |
| [Prometheus](../deploy/observability/prometheus.yml), [alerts](../deploy/observability/alerts.yml), [Grafana](../deploy/observability/grafana/dashboards/previa.json) | Actual scrape targets, missing alert-delivery configuration and provisioned panels. |
| [Backup](../deploy/production/backup.sh), [restore](../deploy/production/restore.sh) | Local PostgreSQL target, encrypted artifacts, retention, destructive restore and paused startup. |
| [Migration runner](../internal/app/migrate.go), [schema requirement](../internal/platform/database/schema.go) | Forward-only embedded migrations and the current schema gate. |
| [CI](../.github/workflows/ci.yml), [Release](../.github/workflows/release.yml) | Exact triggers, jobs, tokens, draft artifacts and absence of deployment automation. |
| [Smoke harness](../deploy/production/smoke.sh), [release report](26-release-report.md) | Candidate rehearsal, historical local evidence and live acceptance work. |

For daily actions use [22 · Operating the MVP](22-mvp-operations.md); for response
to failures use [24 · Incident runbook](24-incident-runbook.md). This runbook expands
the implemented deployment; it does not change its architecture, add deployment
code, or claim that any external infrastructure has been provisioned.
