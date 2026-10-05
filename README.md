# PR Preview Environments — Requirements & Design

## 1. Overview

A self-hosted service that builds and deploys isolated, publicly reachable preview
environments for pull requests on connected GitHub repositories. Each repository
defaults to **Manual**: its owner requests a build for a specific PR. Owners can
enable **Automatic**, which builds the latest commit after a five-minute quiet
period. Previews are torn down when the PR closes or merges.

The full pipeline runs end to end as an operational **local/staging** MVP: the
worker composes source fetching, isolated builds, hardened previews, fenced HTTPS
routing, PR feedback, durable teardown and housekeeping from validated settings, and
the dashboard's Build, Retry, Rebuild and Tear down controls drive it. It is
observable (correlated logs, Prometheus metrics, readiness, a Grafana dashboard
and alerts), hardened for production, and packaged as release images with a
production Compose file, host setup, encrypted backups with a rehearsed restore,
CI and releases. Live proofs against hosted GitHub, real DNS and a real host remain
before a public rollout. See [operating the MVP](docs/21-mvp-operations.md),
[production deployment](docs/24-production-deployment.md), the
[release report](docs/25-release-report.md) and the [roadmap](docs/26-roadmap.md). Start with the [documentation index](docs/README.md)
for a guided reading order. Quick references: [tech stack](technical-docs/tech-stack.md),
[user guide and manual tests](technical-docs/user-guide.md), and Mermaid
[class](technical-docs/class-diagram.md) and [database](technical-docs/database-schema-diagram.md) diagrams.

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
3. Repo Owner adds `.previewrc.yaml` to the repository root. The file is required,
   but individual settings have defaults: root `Dockerfile`, port `3000`, no env
   entries, no HTTP health path, and the `standard` resource tier. Example:
   ```yaml
   dockerfile: ./Dockerfile
   port: 3000
   env:
     - NODE_ENV=preview
   health_check_path: /health # optional; include only if the app provides it
   resource_tier: standard
   ```
4. The repository defaults to **Manual**. Its owner may enable **Automatic** in
   the dashboard; this policy is stored by the platform, not in PR-controlled YAML.
   Connecting a repository or enabling Automatic does not bulk-build existing PRs.

`{}` is a valid minimal file if these defaults match the app. The port describes
where the app already listens inside its container; it does not change the app's
listening port. Named tiers are `small` (0.5 vCPU / 512 MiB), `standard` (1 vCPU /
1 GiB, default), and `large` (2 vCPU / 2 GiB). Default platform ceilings are 2 vCPU
and 2 GiB per preview; lower platform ceilings clamp the resolved limits with a
notice. Numeric resource fields are not supported. These are runtime container
limits, separate from build concurrency/timeouts. An omitted health path does not
authorize publishing an unchecked container: the preview must still pass the
runtime's readiness check before it is routed.

Every build fetches and validates the exact commit's configuration first.
See [repository configuration](docs/06-source-and-configuration.md) for strict validation,
safe error categories, fixtures and effective config hashing. Build mode remains
platform-owned and cannot be set through this YAML.

**Step 1 — PR opened**
1. Contributor opens a PR on GitHub.
2. GitHub sends a `pull_request` webhook event, type `opened`, to your
   `/webhooks/github` endpoint. The payload includes the repo, PR number,
   head branch name, and head commit SHA.
3. Your webhook handler verifies the payload signature (using the shared
   webhook secret) to confirm it really came from GitHub, not a spoofed request.
4. Your handler checks the linked installation and records the latest PR state
   and generation. In **Manual**, it creates no automatic build request. The owner
   selects that PR in the dashboard and chooses **Build latest commit**.
5. In **Automatic**, record durable scheduling intent for five minutes after the
   latest relevant change. Further head commits reset that PR's deadline; comments
   and reviews do not. Return promptly; config fetching/building happens later.
   Manual requests bypass this delay. All requests enforce ownership, repository
   access, PR state, capacity and generation checks before execution.
6. Once eligible, a separate **worker process** picks up the build job. PR state,
   authorized request and River wake-up are recorded transactionally; an observed
   PR update alone must not authorize a build in Manual mode.
7. The worker generates an installation access token (2.2-B step 7) and uses it
   to clone/download the repo at the PR's head commit SHA.
8. The worker reads `.previewrc.yaml`, runs `docker build` using the specified
   Dockerfile, tagged uniquely per PR (e.g. `preview-repo123-pr42`).
9. On successful build, the worker starts a container from that image, with
   resource limits applied (CPU/memory caps) and the declared port exposed
   internally (not directly to the internet).
10. The worker registers this container with the **router** — a mapping of
    the PR's stable hostname (for example `pr-42-k2j4ld-7xq9m2fa.yourpreviewdomain.com`)
    → this container's internal address/port. Previa's route controller accepts the
    mapping behind a generation fence and writes it to the router's configuration
    ([docs/11-routing-and-https.md](docs/11-routing-and-https.md)).
11. The worker calls the GitHub API (using the installation token) to post a
    comment on the PR: "✅ Preview ready: https://pr-42-k2j4ld-7xq9m2fa.yourpreviewdomain.com".
12. If any step from 7-9 fails, the worker instead posts a comment showing the
    failure (and ideally build logs, or a link to them).

**Step 2 — New commit pushed to the same PR**
1. GitHub sends `pull_request` webhook, type `synchronize`, with the new head
   SHA.
2. Same verification/validation as Step 1.
3. Record the latest SHA and increment the PR generation in either mode. Manual
   waits for an explicit build request; Automatic resets the five-minute deadline.
   Superseded results cannot publish, and the worker's successor handoff must obey
   authorized intent and the deadline rather than build every newer observed SHA.
   An existing preview keeps its environment identity and URL.
4. Worker repeats steps 7-9 from above, using the new commit SHA, and replaces
   the running container for that PR (build new one, swap the router's mapping
   to it, then stop the old one — avoid a gap where the URL points to nothing).
5. Worker **edits the existing PR comment** (using its stored comment ID from
   Step 1) rather than posting a new one — keeps the PR thread clean.

**Step 3 — PR closed or merged**
1. GitHub sends `pull_request` webhook, type `closed`.
2. Handler invalidates pending builds and records a **teardown job** immediately in
   either mode, without a quiet period. Generation fencing defeats in-flight builds.
3. Worker stops and removes the container, removes the router's mapping for
   that PR's subdomain, and deletes the built image (or marks it for cleanup)
   to reclaim disk space.

**Step 4 — Housekeeping (runs independently, not triggered by webhooks)**
1. A scheduled background job runs periodically (e.g. every hour).
2. It checks all currently-running preview environments against their last
   activity time (or simply their age).
3. Any environment older than a configured threshold (e.g. 24-48 hours) with
   an open PR gets torn down anyway, and a comment is posted noting it expired
   and can be redeployed with **Build latest commit**. In Automatic mode, a later
   relevant commit can also schedule a replacement after the quiet period.
4. This exists specifically to prevent silently leaking compute if a PR is
   opened and forgotten.

### 2.4 Is a web dashboard needed?

The dashboard supports login, installation linking and the accepted manual build
workflow. Owners need repository mode controls and a way to select a PR and request
its latest commit. GitHub comments/checks remain the review-facing feedback channel.

Current pages are Overview, Repositories, Builds, Environments and Developer Tools.
The workspace uses a "signal" theme (graphite neutrals, a lime accent, light and
dark modes that follow the OS with a manual toggle, system sans plus monospace for
commits and branches). A grouped sidebar collapses to an icon rail on medium screens
and a drawer on phones; unbuilt areas appear as labelled, non-link "Coming next"
items. Ctrl/⌘ K opens a command palette over the same links. Same-origin page
changes use cross-document view transitions, and workspace links are prefetched
on hover where the browser supports speculation rules.
Overview shows real installation/repository/access counts, a setup checklist
derived from those counts and a pipeline view marking which stages are connected.
Repositories shows a responsive repository card grid (up to three columns, one on
phones) inside a 1240px content column. Cards show the repository name, owner and
access state; search and access filters share a sticky toolbar.
Pagination appears only when more than one page is needed.
Selecting a source opens its dedicated repository page; selecting a PR opens its
dedicated detail page. Clickable owner/repository/PR breadcrumbs and Back links
preserve list filters. Search and filters share a row on desktop and stack on
narrow screens. The PR list page shows **Sync Pull Requests** and an **Enable
Automatic Builds** switch with a subtle background. These controls are absent
from individual PR detail pages. The switch is off in Manual mode; enabling
requires an owner-controlled, CSRF-protected confirmation modal. Search, sync
progress and policy saves update in place through HTMX, and committed changes
(sync progress, webhook and mergeability updates) are pushed to open repository
pages over Server-Sent Events rather than polled. Each repository also has a
**Secrets** tab where its owner stores encrypted runtime values (write-only, never
given to fork PRs). Page URLs are shareable
and navigation works without JavaScript.

PR discovery, latest/requested/deployed commit views and the owner's Build,
Build now, Retry, Rebuild and Tear down controls are implemented. Builds and
Environments show actual saved state (queued and running work, failure reasons,
preview URL, health and expiry) and link to each PR's **Build logs** page, which
shows the current attempt's output live over Server-Sent Events (bounded, redacted,
readable without JavaScript). When no worker that can build is running, the
controls say so and the endpoints refuse with 503 instead of recording work.
See [03-user-guide.md](docs/03-user-guide.md) for the current browser workflow.

- **Builds** describes current/latest execution work and its commit/logs, not a
  permanent historical build archive (see 4.5).
- **Environments** describes the preview deployment: URL, deployed SHA, health,
  expiry and teardown. A preview can serve an older commit while a newer build runs.
- All lists/actions are scoped to the logged-in installation owner. Mutations
  require CSRF protection; destructive confirmation uses a modal.

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
  deduplicates deliveries and records PR state plus policy-controlled intent.
  Must respond fast; Manual events do not automatically enqueue builds.
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
- **Web dashboard** — login, install flow, repository policy/manual build controls
  and visibility into PRs, current builds and environments (2.4).
- **Housekeeping scheduler** — periodic job for expiring stale environments.

### 3.2 Data flow summary

```
GitHub PR event → Webhook receiver → durable PR state
   → Manual: wait for an owner request
   → Automatic: persist/reset five-minute deadline
Owner selects a PR → Build latest commit (immediate intent)
Eligible authorized intent
   → Job queue (River/PostgreSQL)
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
- **Why Manual by default and a quiet period for Automatic:** avoids unnecessary
  builds and coalesces rapid commits. Durable deadlines survive restarts. Tracking
  a new PR state and authorizing compute are separate; successor jobs and retries
  must respect that distinction. Manual bypasses delay, not capacity/safety limits.
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

**Verdict: Traefik.** Routes come from its file provider, written by Previa's route
controller rather than from container labels: labels are fixed at container creation
and cannot be fenced by PR generation, and the Docker provider would need the Docker
socket in Traefik. The controller materializes a fenced route table that only moves
forward ([docs/11-routing-and-https.md](docs/11-routing-and-https.md)).

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
| Backend | Go 1.26 (1.26.8 or newer), `net/http` + `chi` |
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
  /routing       # route controller (Traefik file provider), subdomain allocation
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
- `repos` — id, installation_id, full_name, default_config_hash, build_mode (owner-controlled; Manual by default)
- `pull_requests` — id, repo_id, number, head_sha, state (open / closed / merged),
  mergeable (nullable boolean), mergeable_state, build_status (open / checking / conflicts), github_comment_id
- `environments` — id, pull_request_id, container_id, image_tag, subdomain, status (building / running / failed / torn_down), created_at, last_active_at, expires_at
- `build_logs` — id, environment_id, sequence, line, created_at
- `env_secrets` — id, repo_id, key, encrypted_value

**PR tracking scope:** connecting a repository automatically queues discovery of
only its currently open PRs. Initial discovery and every later sync use
`GET /repos/{owner}/{repo}/pulls?state=open&sort=created&direction=asc&per_page=100&page=N`.
Closed/merged history is never backfilled. A PR becomes known while open through
this discovery or a subsequent open/reopen/synchronize event. Closing or merging
an already tracked PR updates its existing row in place; an unknown closed/merged
PR creates no row. Tracked closed/merged records are retained. Missing PRs in an
open-only list are never inferred to be closed; webhooks update their lifecycle.

**Mergeability:** GitHub's nullable `mergeable` and `mergeable_state` are saved
alongside lifecycle state. Open PRs display `Checking` until a resolved `true` or
`clean` observation, `Conflicts` for `false` or `dirty`, and `Open` when buildable.
Conflicting signals favor `Conflicts`. Initial open/synchronize/reopen events
with unresolved status queue bounded background GET checks for that tracked PR;
exhausted checks remain `Checking` until another event or sync. Each push replaces
the old mergeability observation. Both manual authorization and scheduled dispatch
check the saved status; the generation/admission lock repeats the check immediately
before runtime execution. Conflicts and checking refuse builds with contributor
feedback in the PR dashboard. The worker reads the PR from GitHub once more before
admitting a build, outside database locks.

`build_intents` now stores one current authorized request per PR: revision, captured
generation/SHA, source, action, idempotency key, state and eligibility deadline.
Migrations 6–7 and ownership-scoped coordination APIs are implemented. Durable
five-minute wake-ups and intent-aware dispatch/handoff run in the production worker,
and the server records requests with their wake-ups. Repeated Build requests coalesce; explicit Retry after
failure or Rebuild after success creates a new revision without bumping PR
generation. Switching to Manual cancels unstarted Automatic intent; running work
may finish if still current. No historical builds archive is introduced. See
[15-build-policy-and-intents.md](docs/15-build-policy-and-intents.md) for request semantics and
[16-build-scheduling.md](docs/16-build-scheduling.md) for atomic admission, wake-up
uniqueness, revision fencing, worker admission checks and interruption.

A unique constraint on (repo_id, number) for `pull_requests` and on `subdomain` for `environments` prevents duplicate environments when webhooks arrive twice.

**Explicit scope decision:** once preview work is requested, a PR has one
`environment` row reused for its lifetime. An observed PR with no build request
does not need an environment row. Rebuilds update the same row and URL. There is
no persisted history of past builds—no separate `builds` table or browsing build
#1 after build #3 replaces it. A repo with N open PRs has at most N environments.
`build_logs` hold the environment's current build only; earlier logs are cleared
or truncated on rebuild rather than retained as an audit trail.

### 4.6 Container isolation (most important security section)

This system runs other people's code on your machine, so isolation is a core requirement, not a polish item.

- Per container: memory and CPU caps, `pids-limit`, read-only root filesystem where possible, `no-new-privileges`, all Linux capabilities dropped
- Network: previews live on an isolated Docker network with no route to the host, your control plane, Postgres, or cloud metadata endpoints (169.254.169.254)
- Runtime: gVisor (`runsc`) as the Docker runtime for a large isolation upgrade at low cost (optional `PREVIEW_OCI_RUNTIME`; Docker's runc by default)
- Never run builds for fork PRs with secrets available; consider requiring maintainer approval for PRs from forks
- Current implementation: previews run as an unprivileged user with a read-only root, no capabilities and `no-new-privileges` on their own internal network; outbound traffic reaches public addresses only through the egress proxy; probes from a running preview confirm Postgres, the host, metadata and other previews are unreachable. They share the host kernel (see [docs/10-preview-runtime.md](docs/10-preview-runtime.md))
- Docker socket: only the worker touches it; never mount it into a preview container. The router (Traefik) has no socket either: its routes come from the worker's file and the worker attaches it to preview networks
- Builds: each runs in a fresh rootless BuildKit container with CPU/memory/PID limits on a private network whose only exit is an egress proxy that reaches public addresses only (see [docs/09-image-builds.md](docs/09-image-builds.md))

### 4.7 Secrets handling

- Users' env vars for their previewed apps are stored encrypted (AES-GCM, key from server environment) in `env_secrets`, and injected at container start
- They are never read from the repo's `.previewrc.yaml` and never printed to build logs
- GitHub App private key and webhook secret come from environment or a secrets file on the host, never committed

### 4.8 Configuration (environment variables)

The authoritative list of current settings, and which process needs each one, is
in [docs/02-development-setup.md](docs/02-development-setup.md); which capability
each one turns on, and what happens without it, is in
[docs/21-mvp-operations.md](docs/21-mvp-operations.md).

- `DATABASE_URL`
- `APP_ENV` (`development` or `production`), `LOG_LEVEL`, `INTERNAL_ADDR` / `WORKER_INTERNAL_ADDR` (internal health and metrics listeners), `TRUSTED_PROXIES`, `BACKUP_STATUS_FILE` (server), `BUILDS_PAUSED` (worker; the execution kill switch); see [docs/22-observability-and-hardening.md](docs/22-observability-and-hardening.md)
- `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_PATH`, `GITHUB_WEBHOOK_SECRET`
- `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`
- `APP_BASE_URL` (server; also the worker once previews are routed: unknown preview hosts redirect to it)
- `PREVIEW_BASE_DOMAIN` (e.g. `preview.yourdomain.com`; server and worker), `ROUTER_DYNAMIC_DIR`, `ROUTER_CONTAINER`, `ROUTER_ENTRYPOINT`, `ROUTER_API_ADDRESS` (worker; route controller)
- `SESSION_SECRET` (server); `SECRETS_ENCRYPTION_KEYS`, `SECRETS_ACTIVE_KEY_VERSION` (server and worker; versioned keyring for repository secrets, optional for the worker, which then fails repositories with secrets closed)
- `MAX_CONCURRENT_BUILDS` (builds at once across all workers), `ENV_TTL_HOURS` (preview lifetime after its latest publication), `HOUSEKEEPING_DRY_RUN`, `HOUSEKEEPING_GRACE`, `BUILD_CACHE_BUDGET` (worker; housekeeping and capacity, see [docs/14-housekeeping-and-capacity.md](docs/14-housekeeping-and-capacity.md))
- `BUILD_TIMEOUT`, `BUILD_CPUS`, `BUILD_MEMORY`, `BUILD_PIDS`, `BUILD_DISK_BUDGET`, `BUILD_MAX_IMAGE_SIZE`, `BUILD_MIN_FREE_DISK`, `BUILDKIT_IMAGE`, `SOURCE_WORKSPACE_ROOT` (worker; isolated image builds and exact-commit source workspaces)
- `EGRESS_IMAGE`, `PREVIEW_ALLOW_ROOT`, `PREVIEW_WRITABLE_ROOTFS`, `PREVIEW_TMPFS_SIZE`, `PREVIEW_PIDS`, `PREVIEW_STARTUP_TIMEOUT`, `PREVIEW_MEMORY_BUDGET`, `PREVIEW_CPU_BUDGET`, `PREVIEW_MAX_PER_REPOSITORY`, `PREVIEW_OCI_RUNTIME` (worker; hardened previews and the shared egress proxy)
- `ACME_EMAIL`, `ACME_DNS_PROVIDER`, `ACME_DNS_CREDENTIAL_VARIABLE`, `ACME_DNS_CREDENTIAL_FILE`, `ACME_CA_SERVER` (router; Traefik's wildcard certificate over DNS-01, staging by default, the DNS token read from a protected file instead of the originally planned `DNS_PROVIDER_API_TOKEN` variable)

### 4.9 Infrastructure and deployment

- One VPS (Hetzner or similar) running Docker; control plane (Go server, worker, Postgres, Traefik) via Docker Compose
- DNS: a wildcard A record (`*.preview.yourdomain.com`) pointing at the VPS
- Provisioning: Terraform for the VPS and DNS, or a documented setup script for the MVP
- CI/CD: GitHub Actions for tests and image builds; `goreleaser` for versioned releases
- Backups: nightly `pg_dump` to object storage

Implemented as [docs/24-production-deployment.md](docs/24-production-deployment.md)
describes: `deploy/production/compose.yml` (PostgreSQL, a migration step, server,
worker, Traefik, optional Prometheus and Grafana), release images from the
`Dockerfile`, the documented setup script `deploy/production/setup.sh` (Terraform
is optional and not provided), GitHub Actions CI and tag-driven releases with
GoReleaser, and a nightly `pg_dump` encrypted with age before it is copied to
object storage through rclone, with a scripted, rehearsed restore.

### 4.10 Operational limits to set on day one

- Worker pool size in River caps simultaneous builds so ten PRs opening at once cannot take down the host
- Automatic builds become eligible after a five-minute trailing quiet period;
  manual requests bypass this delay but still wait for worker capacity.
- Disk pruning job for old images and build cache (disk exhaustion is the most likely real-world failure)
- Per-repo cap on concurrent active previews
- Build timeout and image size limit

### 4.11 Upgrade paths (post-MVP)

- Dashboard grows: move the frontend to React + Vite
- More capacity: run workers on multiple hosts, add a scheduler, or move to Kubernetes via the `Runtime` interface
- Stronger isolation: Firecracker microVMs
- Bigger logs: move build logs to S3/MinIO or Loki
- Multi-service previews: add `docker-compose.yml` support
