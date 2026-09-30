# PR Preview Environments — Requirements & Design

For local prerequisites, setup commands, and verification steps, see
[Development setup](docs/DEVELOPMENT_SETUP.md). The implemented generation fence
and proposed durable publication architecture are documented in
[the current ordering guide](docs/GENERATION_ORDERING.md) and
[the publication design](docs/DESIGN_DURABLE_PUBLICATION.md).

## 1. Overview

A self-hosted service that automatically builds and deploys an isolated, publicly
reachable preview environment for every pull request on a connected GitHub repo,
and tears it down when the PR closes.

This document is written for the builder (me), not an end user — every step
explains what happens internally at that stage, not just what the user sees.

---

## 2. Requirements

### 2.1 Actors

- **Repo Owner** — installs the app on their GitHub repo, configures it.
- **Contributor / Reviewer** — opens PRs, reviews preview links, pushes commits.
- **The System** — everything I'm building: webhook receiver, build orchestrator,
  router, GitHub API client.

### 2.2 Authentication — GitHub only, MVP scope

Login for the MVP is **"login with GitHub" only**. No separate account system,
no email/password. Here's exactly what that means and what happens at each step,
because "login with GitHub" is actually two different GitHub integrations that
get confused with each other if you don't separate them clearly:

**A. GitHub OAuth App — for human login to *your* dashboard**
This is what lets a Repo Owner log into your web dashboard (see 2.4) using their
GitHub identity, the same way "Sign in with Google" works elsewhere.

Steps:
1. Repo Owner clicks "Login with GitHub" on your site.
2. Your app redirects them to GitHub's OAuth authorize URL, with your registered
   OAuth App's `client_id` and a `redirect_uri` pointing back to your server.
3. GitHub shows the user a consent screen ("This app wants to know your GitHub
   identity").
4. User approves. GitHub redirects back to your `redirect_uri` with a temporary
   `code` in the URL.
5. Your backend exchanges that `code` (plus your `client_secret`) for an
   **access token**, by calling GitHub's token endpoint server-to-server.
6. Your backend uses that access token to call GitHub's `/user` API, gets the
   user's GitHub username/ID/email, and creates or finds a matching user record
   in your own database.
7. Your backend creates its own session for this user (a signed cookie or a JWT)
   — from this point on, the user is talking to *your* session system, not
   GitHub, for every subsequent request to your dashboard.

This step **only establishes identity**. It does NOT give you permission to read
repos, receive webhooks, or create comments — that's a separate integration (B).

**B. GitHub App — for repo access, webhooks, and posting comments/checks**
This is the actual integration that does the work: receiving PR events and
acting on repos. It is installed *per repository*, separately from the OAuth
login above.

Steps:
1. You register a **GitHub App** (not an OAuth App) once, in your GitHub
   developer settings. At registration you declare exactly what it needs:
   - Permissions: Pull requests (read), Contents (read — to pull code),
     Checks (write — to post status), Issues/PR comments (write).
   - Events to subscribe to: `pull_request`.
   - A webhook URL: `https://yourdomain.com/webhooks/github`.
2. Repo Owner, from your dashboard (after logging in via A), clicks
   "Install on a repo" — this sends them to GitHub's App installation flow.
3. GitHub asks the Repo Owner to pick which repo(s) to install it on, and shows
   them exactly what permissions it's requesting.
4. Repo Owner approves. GitHub creates an "installation" record and redirects
   back to your app with an `installation_id`.
5. Your backend stores that `installation_id` against the repo, in your
   database, associated with the logged-in user's account.
6. From now on, GitHub will send webhook events (signed with a shared secret you
   configured) to your `/webhooks/github` endpoint whenever anything relevant
   happens on that repo (a PR opened, updated, closed).
7. Whenever your backend needs to *act* on that repo (clone code, post a
   comment), it doesn't use the user's OAuth token from step A — it generates
   a short-lived **installation access token** by authenticating as the GitHub
   App itself (using a private key you hold) and requesting a token scoped to
   that specific installation. This is the credential actually used to call
   GitHub's API on the repo's behalf.

**Why both are needed:** (A) proves "this human is who they say they are" so
they can use your dashboard. (B) is what actually grants repo-level permissions
and triggers webhooks — it's tied to the *repo*, not the person, and survives
even if the person who installed it changes their password or leaves the team.

### 2.3 Core user flow — end to end, every step explained

**Step 0 — Setup (once per repo)**
1. Repo Owner logs in (2.2-A).
2. Repo Owner installs the GitHub App on their repo (2.2-B).
3. Repo Owner adds a config file to their repo root, e.g. `.previewrc.yaml`,
   telling your system how to run their app. MVP: it must declare a Dockerfile
   path and the port the app listens on. Example:
   ```yaml
   dockerfile: ./Dockerfile
   port: 3000
   env:
     - NODE_ENV=preview
   ```
4. Nothing else happens yet — the system is now "armed" for this repo.

**Step 1 — PR opened**
1. Contributor opens a PR on GitHub.
2. GitHub sends a `pull_request` webhook event, type `opened`, to your
   `/webhooks/github` endpoint. The payload includes the repo, PR number,
   head branch name, and head commit SHA.
3. Your webhook handler verifies the payload signature (using the shared
   webhook secret) to confirm it really came from GitHub, not a spoofed request.
4. Your handler looks up whether this repo has an active installation and a
   valid `.previewrc.yaml`. If not, it stops here (and optionally comments
   explaining what's missing).
5. Your handler enqueues a **build job** — do not build synchronously inside
   the webhook handler; GitHub expects a fast response (a few seconds) or it
   considers the delivery failed. The webhook handler's only job is: verify,
   validate, enqueue, return `200 OK`.
6. A separate **worker process** picks up the build job from the queue.
7. The worker generates an installation access token (2.2-B step 7) and uses it
   to clone/download the repo at the PR's head commit SHA.
8. The worker reads `.previewrc.yaml`, runs `docker build` using the specified
   Dockerfile, tagged uniquely per PR (e.g. `preview-repo123-pr42`).
9. On successful build, the worker starts a container from that image, with
   resource limits applied (CPU/memory caps) and the declared port exposed
   internally (not directly to the internet).
10. The worker registers this container with the **router** — a mapping of
    `pr-42.yourpreviewdomain.com` → this container's internal address/port.
11. The worker calls the GitHub API (using the installation token) to post a
    comment on the PR: "✅ Preview ready: https://pr-42.yourpreviewdomain.com".
12. If any step from 7-9 fails, the worker instead posts a comment showing the
    failure (and ideally build logs, or a link to them).

**Step 2 — New commit pushed to the same PR**
1. GitHub sends `pull_request` webhook, type `synchronize`, with the new head
   SHA.
2. Same verification/validation as Step 1.
3. Your handler checks: is there already a running preview for this PR? If yes,
   enqueue a **rebuild job** referencing the existing PR's environment, not a
   new one.
4. Worker repeats steps 7-9 from above, using the new commit SHA, and replaces
   the running container for that PR (build new one, swap the router's mapping
   to it, then stop the old one — avoid a gap where the URL points to nothing).
5. Worker **edits the existing PR comment** (using its stored comment ID from
   Step 1) rather than posting a new one — keeps the PR thread clean.

**Step 3 — PR closed or merged**
1. GitHub sends `pull_request` webhook, type `closed`.
2. Handler enqueues a **teardown job**.
3. Worker stops and removes the container, removes the router's mapping for
   that PR's subdomain, and deletes the built image (or marks it for cleanup)
   to reclaim disk space.

**Step 4 — Housekeeping (runs independently, not triggered by webhooks)**
1. A scheduled background job runs periodically (e.g. every hour).
2. It checks all currently-running preview environments against their last
   activity time (or simply their age).
3. Any environment older than a configured threshold (e.g. 24-48 hours) with
   an open PR gets torn down anyway, and a comment is posted noting it expired
   and can be redeployed by pushing a new commit.
4. This exists specifically to prevent silently leaking compute if a PR is
   opened and forgotten.

### 2.4 Is a web dashboard needed?

For the MVP, keep it minimal but not zero — you need *somewhere* for the
GitHub OAuth login (2.2-A) and GitHub App installation (2.2-B) flows to
redirect to and from, since both require your app to have at least one web
page. So yes, a small web interface is required, but its MVP scope is just:

- A login page ("Login with GitHub").
- A page listing the repos the logged-in user has installed the app on, with
  an "Install on another repo" button (linking to GitHub's install flow).
- Optionally, a simple list of currently active preview environments per repo,
  with their URLs and a manual "tear down now" button.

Everything else (build status, links) is surfaced via PR comments — the
dashboard is not the primary interface, GitHub is.

### 2.5 Explicit MVP boundaries (what's out of scope for v1)

- Multi-service apps (`docker-compose.yml` support) — Dockerfile-only for MVP.
- Auto-detecting frameworks without a Dockerfile (buildpacks) — not in MVP.
- Team/role management beyond "whoever installed the app" — no granular
  permissions in MVP.
- Custom domains per repo — subdomains on your own domain only.
- Horizontal scaling of the build worker — single worker is fine for MVP.

---

## 3. High-Level Design

### 3.1 Components

- **Webhook receiver** — a small HTTP endpoint. Verifies GitHub signatures,
  validates payloads, enqueues jobs. Stateless, must respond fast.
- **Job queue** — decouples webhook receipt from build execution (e.g. Redis-
  backed queue). Needed so GitHub's webhook delivery never times out waiting
  on a slow build.
- **Build worker** — pulls jobs from the queue, does the clone → build →
  run → register → comment sequence. Can run as one process for MVP.
- **Router** — reverse proxy that maps `pr-N.yourdomain.com` → the correct
  container's internal address. Updates its routing table as environments are
  created/replaced/torn down.
- **GitHub API client** — wraps GitHub App authentication (generating
  installation tokens) and the calls to post/edit comments, create check runs.
- **State store** — a database tracking: installations, repos, PRs, their
  associated container/image names, comment IDs (for editing), and
  created/last-active timestamps (for the housekeeping job).
- **Web dashboard** — thin layer for login, install flow, and environment
  visibility (2.4).
- **Housekeeping scheduler** — periodic job for expiring stale environments.

### 3.2 Data flow summary

```
GitHub PR event
   → Webhook receiver (verify + validate)
   → Job queue
   → Build worker (clone → docker build → docker run → register with router)
   → GitHub API (post/edit comment)

Reviewer clicks preview link
   → Router (subdomain → container)
   → Running container serves the request directly
```

### 3.3 Key design decisions worth being able to explain

- **Why a job queue instead of building inline in the webhook handler:**
  GitHub expects webhook responses within seconds; builds take much longer.
  Decoupling also lets you control concurrency (limit simultaneous builds) and
  survive worker crashes without losing the event.
- **Why Docker's API rather than hand-rolled namespaces/cgroups:** the hard
  problem in this project is orchestration and lifecycle correctness, not
  container internals — using Docker keeps scope focused on that.
- **Why edit comments instead of posting new ones per push:** keeps the PR
  thread readable; requires storing the comment ID per PR in the state store.
- **Why a separate housekeeping job instead of relying only on the `closed`
  webhook:** webhooks can be missed (delivery failures, downtime) — a
  reconciling background job is a standard resilience pattern for exactly
  this kind of event-driven system.

---

## 4. Tech Stack

This section was chosen purely on the project's technical needs: event-driven, I/O-heavy, drives Docker programmatically, talks to GitHub constantly, and must route traffic dynamically as containers come and go.

### 4.1 Comparisons

**Backend language**

| | Go | TypeScript (Node) | Rust | Java/Kotlin | Python |
|---|---|---|---|---|---|
| Docker SDK | Official (Docker is written in Go) | Community (dockerode) | Community (bollard) | Community (docker-java) | Official (docker-py) |
| Many concurrent builds/jobs | Goroutines, very cheap | Fine for I/O, awkward for CPU work | Excellent, heavy to write | Strong (virtual threads) | GIL / asyncio friction |
| GitHub App tooling | go-github + ghinstallation | Best in class (Octokit, Probot) | octocrab | Adequate | githubkit / PyGithub |
| Deployment | One static binary | Runtime + node_modules | One static binary | JVM | Interpreter + venv |
| Infra ecosystem | Traefik, Caddy, Docker, K8s, Nomad are Go; libraries embeddable | Weak | Growing | Moderate | Moderate |

**Verdict: Go.** Node is the only real competitor (thanks to Probot), but Go wins on Docker integration, concurrency model and deployment simplicity.

**Routing layer**

| | Traefik | Caddy | Nginx | Custom Go proxy |
|---|---|---|---|---|
| Dynamic routes as containers churn | Automatic via Docker labels, no reloads | Admin API (you push config) | Config rewrite + reload | Full control, you build everything |
| Wildcard TLS | Yes (DNS-01) | Yes (DNS-01, simplest config) | Manual (certbot) | You build it |
| Gap risk on redeploy | Low | Low | Higher | Depends on your implementation |

**Verdict: Traefik.** Container labels make routing declarative, so the "register with the router" step mostly disappears.

**Job queue**

| | River (Postgres) | asynq (Redis) | NATS JetStream | RabbitMQ |
|---|---|---|---|---|
| Extra infrastructure | None (uses Postgres) | Redis | NATS server | RabbitMQ server |
| Transactional enqueue with your state | Yes | No | No | No |
| Retries, scheduling, uniqueness | Built in | Built in | Mostly manual | Mostly manual |

**Verdict: River.** Enqueueing a job inside the same DB transaction as the state change removes the "state saved but job lost" class of bugs.

**Runtime / orchestration**

| | Docker on one host | Kubernetes (namespace per PR) | Nomad | Firecracker microVMs |
|---|---|---|---|---|
| Complexity | Low | High | Medium | Very high |
| Isolation for untrusted code | Container-level (hardenable with gVisor) | Container-level | Container-level | Strongest |
| Fits MVP | Yes | Overkill | Overkill | Overkill |

**Verdict: Docker on one host**, behind a `Runtime` interface so Kubernetes can be added later without rewriting the worker.

**Frontend (thin dashboard: login, repo list, active previews, live build logs)**

| | HTMX + templ | React + Vite | Next.js | SvelteKit |
|---|---|---|---|---|
| Separate build/deploy | No, ships inside the Go binary | Yes | Yes, plus a Node server | Yes |
| Live logs / status | SSE, trivial | Works, more code | Works | Works |
| Fit for a dashboard this small | Best | Good | Overkill | Good |

**Verdict: HTMX + templ.** If the dashboard grows into a real product, React + Vite is the natural upgrade path.

### 4.2 Recommended stack (summary)

| Layer | Choice |
|---|---|
| Backend | Go 1.22+, `net/http` + `chi` |
| Database | PostgreSQL |
| Job queue | River (Postgres-backed) |
| Routing / TLS | Traefik v3 + Let's Encrypt wildcard cert (DNS-01) |
| Container runtime | Docker Engine, hardened, optionally gVisor |
| Image builds | BuildKit |
| Frontend | templ + HTMX + SSE + Tailwind |
| Deployment | Single VPS, Docker Compose |
| Observability | Prometheus + Grafana (+ optional OpenTelemetry) |

### 4.3 Libraries in detail

**Backend**
- HTTP routing and middleware: `github.com/go-chi/chi/v5`
- Config: `github.com/knadh/koanf` (or `envconfig`); logging: stdlib `log/slog`
- GitHub API + webhook payload validation: `github.com/google/go-github`
- GitHub App installation tokens: `github.com/bradleyfalzon/ghinstallation`
- OAuth login: `golang.org/x/oauth2` with the GitHub endpoint
- Sessions: `github.com/alexedwards/scs` with its Postgres store
- `.previewrc.yaml` parsing: `github.com/goccy/go-yaml`; validation: `github.com/go-playground/validator`
- Docker: `github.com/docker/docker/client`
- Image builds: `github.com/moby/buildkit` client (layer caching)
- Jobs: `github.com/riverqueue/river` with the `riverpgxv5` driver
- Encryption of stored secrets: stdlib `crypto/aes` + `crypto/cipher` (AES-GCM), or `filippo.io/age`

**Database**
- Driver: `github.com/jackc/pgx/v5`
- Queries: `sqlc` (SQL in, type-safe Go out)
- Migrations: `goose`

**Frontend**
- Templates: `github.com/a-h/templ`
- Interactivity: HTMX (+ SSE extension for live build logs)
- Styling: Tailwind CSS via the standalone CLI (no Node toolchain needed)

**Observability**
- Metrics: `github.com/prometheus/client_golang`
- Tracing (optional): OpenTelemetry Go SDK

**Testing**
- `testing` + `testify`
- `testcontainers-go` for real Postgres and Docker in integration tests
- Recorded GitHub webhook payloads as fixtures
- A permanent sample repo with a Dockerfile as the end-to-end target
- Local webhook development: `smee.io` or a Cloudflare Tunnel to your laptop

### 4.4 Suggested repository layout

```
/cmd
  /server        # HTTP server: webhooks, dashboard, OAuth
  /worker        # River worker process: build/deploy/teardown jobs
/internal
  /github        # App auth, API client, webhook handling
  /jobs          # River job definitions (build, rebuild, teardown, expire)
  /runtime       # Runtime interface + Docker implementation
  /router        # Traefik label generation, subdomain allocation
  /store         # sqlc-generated queries, migrations
  /web           # templ components, handlers, SSE
  /config        # config loading, .previewrc.yaml parsing
/deploy          # docker-compose.yml, Traefik config, setup script
/migrations
```

Keeping `Runtime` as an interface in `/internal/runtime` is what lets you swap Docker for Kubernetes later.

### 4.5 Data model sketch

- `users` — id, github_id, username, created_at
- `installations` — id, github_installation_id, owner_user_id
- `repos` — id, installation_id, full_name, default_config_hash
- `pull_requests` — id, repo_id, number, head_sha, state, github_comment_id
- `environments` — id, pull_request_id, container_id, image_tag, subdomain, status (building / running / failed / torn_down), created_at, last_active_at, expires_at
- `build_logs` — id, environment_id, sequence, line, created_at
- `env_secrets` — id, repo_id, key, encrypted_value

A unique constraint on (repo_id, number) for `pull_requests` and on `subdomain` for `environments` prevents duplicate environments when webhooks arrive twice.

### 4.6 Container isolation (most important security section)

This system runs other people's code on your machine, so isolation is a core requirement, not a polish item.

- Per container: memory and CPU caps, `pids-limit`, read-only root filesystem where possible, `no-new-privileges`, all Linux capabilities dropped
- Network: previews live on an isolated Docker network with no route to the host, your control plane, Postgres, or cloud metadata endpoints (169.254.169.254)
- Runtime: gVisor (`runsc`) as the Docker runtime for a large isolation upgrade at low cost
- Never run builds for fork PRs with secrets available; consider requiring maintainer approval for PRs from forks
- Docker socket: only the worker touches it; never mount it into a preview container

### 4.7 Secrets handling

- Users' env vars for their previewed apps are stored encrypted (AES-GCM, key from server environment) in `env_secrets`, and injected at container start
- They are never read from the repo's `.previewrc.yaml` and never printed to build logs
- GitHub App private key and webhook secret come from environment or a secrets file on the host, never committed

### 4.8 Configuration (environment variables)

- `DATABASE_URL`
- `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_PATH`, `GITHUB_WEBHOOK_SECRET`
- `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`
- `PREVIEW_BASE_DOMAIN` (e.g. `preview.yourdomain.com`)
- `SESSION_SECRET`, `SECRETS_ENCRYPTION_KEY`
- `MAX_CONCURRENT_BUILDS`, `ENV_TTL_HOURS`
- `DNS_PROVIDER_API_TOKEN` (for Traefik's wildcard cert challenge)

### 4.9 Infrastructure and deployment

- One VPS (Hetzner or similar) running Docker; control plane (Go server, worker, Postgres, Traefik) via Docker Compose
- DNS: a wildcard A record (`*.preview.yourdomain.com`) pointing at the VPS
- Provisioning: Terraform for the VPS and DNS, or a documented setup script for the MVP
- CI/CD: GitHub Actions for tests and image builds; `goreleaser` for versioned releases
- Backups: nightly `pg_dump` to object storage

### 4.10 Operational limits to set on day one

- Worker pool size in River caps simultaneous builds so ten PRs opening at once cannot take down the host
- Disk pruning job for old images and build cache (disk exhaustion is the most likely real-world failure)
- Per-repo cap on concurrent active previews
- Build timeout and image size limit

### 4.11 Upgrade paths (post-MVP)

- Dashboard grows: move the frontend to React + Vite
- More capacity: run workers on multiple hosts, add a scheduler, or move to Kubernetes via the `Runtime` interface
- Stronger isolation: Firecracker microVMs
- Bigger logs: move build logs to S3/MinIO or Loki
- Multi-service previews: add `docker-compose.yml` support
