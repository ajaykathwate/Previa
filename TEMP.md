# Local configuration, processes, and queue verification

This is the operational handoff for the queue setup. For reusable installation
instructions, see [DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md).

## What is implemented

- PostgreSQL configuration, connection pooling, startup health checks, application
  migrations, and generated queries.
- HTTP server, request logging/IDs, panic recovery, templ layout, local HTMX assets,
  and Air development reload.
- GitHub OAuth login, persisted users, server-side sessions, login rotation,
  protected routes, CSRF protection, and logout.
- River v0.47.0 producer in the server and a separate worker process. The only
  configured production job kind is `ping`. Generation-fenced build/teardown and
  durable environment-operation workers are implemented behind interfaces and
  exercised in PostgreSQL tests; they are not registered in the production worker.
  Docker, routing, GitHub reporting, and webhook HTTP adapters remain unbuilt.
- Goose migration 4 adds `environment_operations`, pending candidate fields, and
  the durable publication state machine. `Repository.Apply` atomically stores a
  generation-guarded state transition, operation row, and River wake-up. Operation
  calls run outside database transactions; final database writes are generation
  checked. The bounded reconciler contract exists but is not scheduled in app.
- Authenticated, CSRF-protected `POST /queue/ping`, exposed through the protected dashboard's
  queue-check form. It returns HTTP 202 with `job_id` and `duplicate`.

## Configuration: what to put in `.env`

Copy `.env.example` once if `.env` does not exist. Never commit `.env` or private keys.
Each process loads only its own settings: `SharedConfig` supplies `DATABASE_URL`,
`ServerConfig` adds HTTP/OAuth/session settings, and `WorkerConfig` adds queue
concurrency and an optional build timeout. River uses the same database connection.
The worker needs no OAuth variables. Real OAuth credentials are required for browser
sign-in.

| Variable | Local value / source | Why it exists and when it is used |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080`; choose a free local port | Server's listening address; worker does not listen for HTTP. |
| `APP_BASE_URL` | `http://localhost:8080` | Browser origin, OAuth callback construction, cookie security, and CSRF origin checks. Use this exact hostname in the browser. |
| `DATABASE_URL` | `postgres://previa:change-me@localhost:5432/previa?sslmode=disable` | Both processes, application migrations, and River migrations use this PostgreSQL database. Values match `deploy/docker-compose.yml`. |
| `GITHUB_OAUTH_CLIENT_ID` | Your GitHub **OAuth App** Client ID | Server starts the sign-in flow. Obtain it using the steps below. |
| `GITHUB_OAUTH_CLIENT_SECRET` | Your OAuth App client secret | Server exchanges the authorization code; never expose it in browser code. |
| `SESSION_SECRET` | Generate with `openssl rand -hex 32` | Signs OAuth state; must be at least 32 characters. SCS stores opaque session tokens in PostgreSQL. |
| `MAX_CONCURRENT_BUILDS` | `2` (positive integer) | Sets the queue worker concurrency limit per worker process. |
| `BUILD_TIMEOUT` | `12m` by default; optionally set e.g. `15m` | Worker-only build execution budget; positive duration up to 24h, with 1m additional completion/cleanup grace. |

Commands above print newly generated secrets: copy them into `.env`, not into Git.
Keep `.env` as shell assignments; quote values containing shell special characters.

### Obtain GitHub OAuth values now

1. Open GitHub → your avatar → **Settings** → **Developer settings** → **OAuth Apps**.
2. Select **New OAuth App** (or open your existing local development app).
3. Set Homepage URL to `http://localhost:8080`.
4. Set Authorization callback URL to **`http://localhost:8080/auth/github/callback`**.
5. Register the app. Copy its Client ID into `GITHUB_OAUTH_CLIENT_ID`.
6. Generate a client secret and copy it into `GITHUB_OAUTH_CLIENT_SECRET`.
7. Generate `SESSION_SECRET`, save `.env`, and restart the server after edits.
   If you change the public origin/port, update both `.env` and the registered callback.

The detailed login checks are in [docs/AUTH_TESTING.md](docs/AUTH_TESTING.md).

### Settings for future integrations

GitHub App, webhook, preview domain, encryption, TTL, and DNS settings listed in
README Section 4.8 will be added to the relevant process when their features are
implemented. They are absent from the config structs and `.env.example`; existing
values in a local `.env` are ignored. HTTP/OAuth/session settings are server-only;
`MAX_CONCURRENT_BUILDS` and `BUILD_TIMEOUT` are worker-only; `DATABASE_URL` is shared.

## Processes and their purpose

| Process / command | Purpose | Needed for |
| --- | --- | --- |
| Docker daemon / Docker Desktop | Runs the local PostgreSQL container | Local database availability. |
| Compose `postgres` service | Persists users, sessions, application state, and River jobs | Both server and worker; queued work survives process restarts. |
| `make run-server` | HTTP requests, OAuth, sessions, and job insertion | Browser access and enqueueing; does not execute jobs. |
| `make run-worker` | Claims and executes queued ping jobs | Completion, retries, and River maintenance. Environment publication workers are not yet registered. |
| `make dev` (Air) | Compiles CSS, generates templates, builds, and restarts the server on changes | Optional alternative to `make run-server`; do not run both on the same port. |
| `make migrate-up` | Runs Goose, then the environment-based River migration adapter | One-time schema changes after checkout/update; not a background process. |
| `make assets` | Compiles official Tailwind CSS v4.1.18 into embedded static CSS | Automatically included in server/build/test/lint; no background process or Node.js needed. |
| `make generate` | Generates templ and sqlc Go code | Build preparation; already included in build/run targets. |

River's maintenance/leader services run inside the worker, not as a separate
daemon. PostgreSQL is the queue backend; Redis and Sidekiq are not required.
Air reloads the server only. Restart `make run-worker` manually after worker changes.

## Start and verify, step by step

1. Start Docker, then PostgreSQL and migrations from the repository root:

   ```sh
   docker compose -f deploy/docker-compose.yml up -d --wait postgres
   make migrate-up
   make river-status
   ```

   Expected: PostgreSQL is healthy; Goose and River apply pending migrations or
   report none pending. River currently has migrations 001–007. The adapter reads `DATABASE_URL` from
   the environment; Goose uses `GOOSE_DBSTRING`. Neither receives credentials in
   process arguments. River owns its
   `river_*` tables/history independently of Goose. `make migrate-down` only
   rolls back an application migration; it does not remove River's tables.

2. **Terminal A — server:**

   ```sh
   make run-server
   ```

   Expected: `database health check passed`, then `HTTP server listening`.
   The run target loads `.env` automatically; already exported variables take
   precedence. Use `ENV_FILE=.env.local` to select another file. Direct `go run`
   still needs manual exports. Use `make dev` for server live reload.

3. **Terminal B — worker:**

   ```sh
   make run-worker
   ```

   Expected: `database health check passed`, then `River worker started` with
   `queue=ping` and `max_workers=2` (or your configured limit).
   Keep one worker running for this local setup: multiple worker processes each
   receive their own concurrency allowance.

4. Open `http://localhost:8080`, log in through GitHub, and submit the ping form.
   Use a synthetic key such as `demo:pr:1`; it does not require a real PR record.
   Allowed characters: letters, digits, `_`, `.`, `/`, `:`, `#`, `-`; 1–128 characters.
   Expected response: `{"job_id":123,"duplicate":false}` (actual ID varies).
   The worker logs `ping job processed` with that ID and key.

5. Inspect PostgreSQL using the commands below. The job should show `completed`,
   `attempt=1`, and a non-null `finalized_at`. An HTTP 202 alone proves insertion,
   not execution. Use `Ctrl+C` to stop server/worker when finished.

### Inspect queued jobs and durable environment operations in PostgreSQL

Open an interactive SQL console:

```sh
docker compose -f deploy/docker-compose.yml exec postgres psql -U previa -d previa
```

Inside `psql`:

```sql
SELECT id, kind, queue, args->>'pr_key' AS pr_key, state,
       attempt, max_attempts, created_at, attempted_at, finalized_at
FROM river_job
ORDER BY id DESC LIMIT 20;

SELECT queue, state, count(*) FROM river_job GROUP BY queue, state ORDER BY queue, state;

SELECT id, pull_request_id, generation, kind, state, attempts, available_at,
       lease_expires_at, last_error_code
FROM environment_operations
ORDER BY id DESC LIMIT 20;
```

- Enter `\watch 2` after a query to refresh every two seconds; `Ctrl+C` stops watching.
- Inspect failures: `SELECT id, state, errors FROM river_job WHERE cardinality(errors) > 0;`
- Exit with `\q`. A PostgreSQL GUI connected using `DATABASE_URL` can inspect the
  same tables. No River UI is included; inspect these PostgreSQL tables directly.
- Typical states: `available` waits for a worker, `running` executes, `completed`
  succeeded, `retryable` awaits another attempt, `discarded` exhausted attempts,
  and `cancelled` was permanently cancelled.
- River removes completed jobs after its retention period (default 24 hours), so
  `river_job` is an operational view rather than a permanent audit log.
  `environment_operations` retains publication intent/outcome beyond River
  retention and is the durable inspection point for that work.

### Check uniqueness and transaction behavior

1. Stop the worker while keeping the server running.
2. Submit the same key twice. The first response gives a new ID; the second
   returns that same ID with `duplicate=true`. SQL shows one active job.
3. Start the worker; that job completes. Submitting the same key afterward creates
   a new job because terminal states no longer reserve the uniqueness key.

The shared uniqueness policy selects only the `pr_key` argument, excludes kind and
queue, and covers `available`, `pending`, `running`, `retryable`, and `scheduled`.
Build/teardown use that policy and capture PR generation in job arguments. An
active job hands off to the latest requested generation atomically on completion.
Insertion uses River `InsertTx`; uncommitted jobs are invisible to workers and
rollback removes them. Generation recording, guarded environment results, durable
operations, retry policy, and remaining adapter work are explained in
[GENERATION_ORDERING.md](docs/GENERATION_ORDERING.md) and
[DESIGN_DURABLE_PUBLICATION.md](docs/DESIGN_DURABLE_PUBLICATION.md).
Retries use River's default backoff, with five attempts for ping; invalid ping
arguments cancel permanently. Ping is idempotent. Future jobs must also tolerate
re-execution because workers provide at-least-once processing.

## Verification performed

- Goose migrations 3 and 4 add PR/result generations, deployed SHA, pending
  candidate fields, and durable operation records. Ordering tests
  use real PostgreSQL/River and simulated runtime/publication adapters; see the
  generation document for scope and test coverage.

- Live server → separate worker → PostgreSQL: job **10** was accepted over HTTP,
  processed, and recorded as `completed`, attempt **1**, with a finalization time.
  A temporary authenticated test session was used; its session/user were removed.
  The completed ping row remains available for inspection until normal retention.
- Build, vet, golangci-lint, and all existing tests including database integration
  tests passed. Queue tests cover completion, cross-kind/cross-queue active
  uniqueness, terminal-state release, transaction visibility/rollback, concurrency,
  and permanent cancellation. HTTP tests verify login/CSRF and key validation.
- `make migrate-up` applied Goose migration 4 successfully; `river-status`
  reports River version 007. Entrypoints have no dedicated unit tests; app migration
  orchestration has integration tests. Worker startup/shutdown were exercised live.
- Config split verified with a worker environment containing only `DATABASE_URL`
  and `MAX_CONCURRENT_BUILDS`: startup, DB health check, and shutdown passed.
- Migration credential check: 39 `ps auxww` snapshots observed both actual Goose
  and River migration binaries while `make migrate-up` succeeded. Neither the
  database URL nor password appeared in arguments. Fresh-schema migration and
  idempotence tests also pass; temporary test schemas are removed.
- Server and worker were stopped after verification; PostgreSQL was left running.

The current run also covers outbox rollback, durable operation retry/finalization,
lost-wakeup/expired-lease recovery, pending-artifact protection, and proves that an
external executor held behind a fake does not block a new PR event. It does not
validate real router generation fencing or external-system recovery.

River owns its schema using its pinned migrations, following the
[official migration workflow](https://riverqueue.com/docs/migrations). No conflicting
architecture was introduced; external build/teardown adapters remain unregistered.

## UI organization and dependency decision

- All templates now live under `web/pages`, `web/layouts`, and `web/components`.
  Feature handlers/services/repos remain under `internal`; page packages do not
  import them. This is the approved exception to feature template colocation.
- Official Tailwind CSS v4.1.18 is installed automatically with pinned SHA-256
  checksums. Stable templUI v1.13.2 supplies copied button/card/alert/input source;
  neither official React shadcn/ui nor the shadcn-templ v2 beta is installed.
- Native input behavior is retained; unused password-toggle JS/icons are omitted.
  Source, license, and adaptations are documented in `web/components/README.md`.
- `make dev` remains one server command: CSS compilation precedes generation and
  rebuild; generated output is excluded from watching. `.env` loading is unchanged.
- Component source ownership means upstream fixes require explicit review. v1
  ongoing support and v2 compatibility are not guaranteed. Any beta adoption or
  migration remains a future decision requiring approval; no such change is made.
- `README.md` is intentionally unchanged per instruction; its older coarse layout
  sketch differs from the approved DESIGN Section 20. No additional cleanup or
  architectural expansion is proposed in this task.

UI verification: build/tests/vet/lint passed; real HTTP CSS serving and Air reload
for both template utilities and CSS source passed. Temporary edits restored and
smoke server stopped. Browser visual review and external GitHub login were not
rerun; existing HTTP auth/session/CSRF tests passed. No database schema changed.


## Home page and dashboard presentation

- `/` is a public landing page with a warm paper/terracotta/forest theme, original
  CSS illustrations, responsive content, and reduced-motion support. Planned preview
  workflows are labeled as concepts; there are no fabricated statistics or activity.
- **Get started with GitHub** uses the existing OAuth flow. Its successful callback
  now redirects to protected `/dashboard`; logout returns to the public home page.
- The shared authenticated layout has desktop sidebar navigation, a mobile workspace
  menu, and an account menu for session details/logout. Existing CSRF/session checks
  stay in backend handlers/middleware. Dashboard identity comes from the session.
- The existing ping endpoint is unchanged. The queue form displays returned job IDs
  and duplicate status inline, with pending/error feedback; it does not claim worker
  completion. Without JavaScript, the ordinary form still submits to the JSON endpoint.
- No new integrations, dependencies, fake repositories, or preview functionality
  were added. Root README.md is unchanged.
- Chromium checks passed across 320–1440px; screenshots reviewed for desktop/mobile.
  The browser used a temporary fake GitHub provider through the real session/CSRF
  middleware to exercise redirects, dashboard access, logout, queue feedback,
  keyboard menu dismissal, and reduced motion. The fixture was removed. Real GitHub
  consent and PostgreSQL integration tests were not rerun for this presentation task.
- Final `make build test lint` passed, including Go build, the existing test suite,
  `go vet`, and golangci-lint (zero issues). `git diff --check` passed.

## Approved dashboard refinement

- Dashboard uses a sticky top navbar, fixed desktop sidebar, and collapsible mobile
  navigation. Upcoming Repositories/Environments/Builds links target explanatory
  dashboard sections; Connect a repository is disabled. No backend features or
  fabricated activity were added. Developer tools keeps the queue check collapsed.
- Home reveals sections once, pauses the hero offscreen/in hidden tabs, and honors
  reduced motion. Content stays visible without JavaScript. Touch targets and
  sticky-navigation anchor spacing were refined.
- Existing colors and fonts are unchanged; AGENTS.md records the user-approved
  visual theme as a rule for future work. No new runtime dependencies/configuration.
- Browser verification passed at 320–1440px: sticky navbar while scrolling, mobile
  anchor navigation, valid placeholder targets, queue success/error feedback, reveal
  entry, offscreen hero pause, reduced motion, and no-JavaScript content visibility.
  Desktop/mobile screenshots were reviewed. A temporary presentation fixture was
  removed; external GitHub consent and DB integration were not rerun for this change.
- Final `make build test lint` passed (Go build/tests/vet and golangci-lint, zero
  issues). `git diff --check` passed. Root README.md remains unchanged.

## Template diagnostics and modal confirmation

- Reported undefined `BranchIcon`/dashboard `Index` diagnostics did not reproduce
  in a direct Go build. Both generated functions were present with correct signatures.
  Regenerated all presentation code; do not edit generated Go or remove functions
  to accommodate stale editor diagnostics. Setup guide explains generation and
  language-server recovery. Added `make templates` and an optional polling
  `make watch-templates` (Air, no app configuration) for editor-only work.
- Confirmation UI uses native dialog overlays, preserving the current screen.
  Logout appears on both dashboard and session pages, with Cancel initial focus,
  focus trapping/restoration, Escape/close/backdrop dismissal, and CSRF-protected
  POST only on confirmation. The separate logout page was removed; old GET links
  redirect to the dashboard and open the modal. Authentication semantics remain.
- This UI rule is recorded in AGENTS.md; future confirmation prompts should reuse
  the accessible modal shell. No new library, credentials, schema, or deployment
  service is required. Root README.md is unchanged.
- Browser checks passed for page/scroll preservation, no request on dismissal,
  Cancel/Escape/backdrop, focus trapping/restoration, 320–1440px layouts, session-page
  confirmation, legacy GET redirect, reduced motion, and native no-JS dialog controls
  in Chromium. Confirmed logout used the real session/CSRF middleware with a fake
  GitHub provider; session deletion and protected-route rejection passed. Temporary
  preview/probe files were removed. Real GitHub and DB integration were not rerun.
- Template-only Air watcher detected a temporary source change and generated its
  Go component successfully. Original native fsnotify did not detect edits here;
  the final watcher uses the already-established Air polling mechanism.
- Final `make build test lint` passed: build, existing tests, vet, and golangci-lint
  (zero issues). `git diff --check` passed; generated component signatures are current.
