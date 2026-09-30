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
  executable job kind is `ping`; no build, deployment, or teardown jobs are added.
- Authenticated, CSRF-protected `POST /queue/ping`, exposed through the home page's
  ping form. It returns HTTP 202 with `job_id` and `duplicate`.

## Configuration: what to put in `.env`

Copy `.env.example` once if `.env` does not exist. Never commit `.env` or private keys.
The shared configuration loader validates all the variables below for **both**
server and worker, including settings reserved for later features. Those later
settings can retain the example placeholders for this local queue exercise.
Real OAuth credentials are required to sign in through GitHub.

| Variable | Local value / source | Why it exists and when it is used |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080`; choose a free local port | Server's listening address; worker does not listen for HTTP. |
| `APP_BASE_URL` | `http://localhost:8080` | Browser origin, OAuth callback construction, cookie security, and CSRF origin checks. Use this exact hostname in the browser. |
| `DATABASE_URL` | `postgres://previa:change-me@localhost:5432/previa?sslmode=disable` | Both processes, application migrations, and River migrations use this PostgreSQL database. Values match `deploy/docker-compose.yml`. |
| `GITHUB_OAUTH_CLIENT_ID` | Your GitHub **OAuth App** Client ID | Server starts the sign-in flow. Obtain it using the steps below. |
| `GITHUB_OAUTH_CLIENT_SECRET` | Your OAuth App client secret | Server exchanges the authorization code; never expose it in browser code. |
| `SESSION_SECRET` | Generate with `openssl rand -hex 32` | Signs OAuth state; must be at least 32 characters. SCS stores opaque session tokens in PostgreSQL. |
| `MAX_CONCURRENT_BUILDS` | `2` (positive integer) | Currently sets the `ping` queue's maximum concurrent jobs **per worker process**; will also configure the build queue when implemented. |
| `GITHUB_APP_ID` | Example `123456`; later replace with GitHub App ID | Reserved for repository installation/API access; separate from OAuth sign-in. Must be positive. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | Example `/path/to/github-app.private-key.pem` | Reserved for signing GitHub App authentication. No key file is opened by the ping flow. |
| `GITHUB_WEBHOOK_SECRET` | Example placeholder; later generate a random secret | Reserved for validating incoming GitHub webhook signatures. |
| `PREVIEW_BASE_DOMAIN` | Example `preview.example.com` | Reserved for preview hostnames; domain only, no scheme, port, or wildcard. |
| `SECRETS_ENCRYPTION_KEY` | Generate with `openssl rand -base64 32` | Reserved for encrypting stored preview application secrets; exactly 32 decoded bytes. Example key is unsuitable for real secrets. |
| `ENV_TTL_HOURS` | `24` (positive integer) | Reserved for preview expiry scheduling. |
| `DNS_PROVIDER_API_TOKEN` | Example placeholder; later your DNS provider's API token | Reserved for wildcard certificate DNS challenges. No DNS calls are made by ping jobs. |

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

### Obtain settings for later integrations when those features are built

- **GitHub App:** GitHub → Settings → Developer settings → GitHub Apps → your app
  (or New GitHub App). Copy the numeric App ID. Under Private keys, generate/download
  a PEM and set its local path in `.env`. Keep the PEM ignored and access restricted.
  Generate a webhook secret with `openssl rand -hex 32`, then use the same value in
  the app's Webhook secret field and `.env`. Webhook URL/permissions depend on the
  future integration; this phase does not provide a webhook endpoint.
- **Preview domain:** select a subdomain of a domain you control, such as
  `preview.your-domain.com`. Obtain/manage the parent domain through your registrar
  and DNS provider. DNS and wildcard certificate setup belong to the routing phase.
- **DNS token:** sign in to the DNS provider managing that domain, open its API
  token/credentials settings, and create a token scoped to the required DNS zone
  and record operations. Exact fields depend on the provider chosen for routing.
- **Encryption key:** run `openssl rand -base64 32` once and store the result
  securely. Preserve it when encrypted secrets are eventually stored; changing
  the key without a migration would make those secrets unreadable.
- **Concurrency / TTL:** choose positive integers yourself. They are operational
  limits, not values obtained from GitHub or another service.

## Processes and their purpose

| Process / command | Purpose | Needed for |
| --- | --- | --- |
| Docker daemon / Docker Desktop | Runs the local PostgreSQL container | Local database availability. |
| Compose `postgres` service | Persists users, sessions, application state, and River jobs | Both server and worker; queued work survives process restarts. |
| `make run-server` | HTTP requests, OAuth, sessions, and job insertion | Browser access and enqueueing; does not execute jobs. |
| `make run-worker` | Claims and executes queued ping jobs | Completion, retries, and River maintenance. Without it, jobs remain queued. |
| `make dev` (Air) | Generates templates, builds, and restarts the server on changes | Optional alternative to `make run-server`; do not run both on the same port. |
| `make migrate-up` | Runs Goose, then River's own migration tool | One-time schema changes after checkout/update; not a background process. |
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
   report none pending. River currently has migrations 001–007. River owns its
   `river_*` tables/history independently of Goose. `make migrate-down` only
   rolls back an application migration; it does not remove River's tables.

2. **Terminal A — server:**

   ```sh
   set -a
   . ./.env
   set +a
   make run-server
   ```

   Expected: `database health check passed`, then `HTTP server listening`.
   `set -a` exports assignments when sourcing `.env`; `set +a` turns that off.
   Repeat the export block in every new terminal. `make dev` can replace the
   final command if you want server live reload.

3. **Terminal B — worker:**

   ```sh
   set -a
   . ./.env
   set +a
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

### See queued jobs without a dashboard

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
```

- Enter `\watch 2` after a query to refresh every two seconds; `Ctrl+C` stops watching.
- Inspect failures: `SELECT id, state, errors FROM river_job WHERE cardinality(errors) > 0;`
- Exit with `\q`. A PostgreSQL GUI connected using `DATABASE_URL` can inspect the
  same tables. River UI, the Sidekiq-style option, is discussed for approval below.
- Typical states: `available` waits for a worker, `running` executes, `completed`
  succeeded, `retryable` awaits another attempt, `discarded` exhausted attempts,
  and `cancelled` was permanently cancelled.
- River removes completed jobs after its retention period (default 24 hours), so
  `river_job` is an operational view rather than a permanent audit log.

### Check uniqueness and transaction behavior

1. Stop the worker while keeping the server running.
2. Submit the same key twice. The first response gives a new ID; the second
   returns that same ID with `duplicate=true`. SQL shows one active job.
3. Start the worker; that job completes. Submitting the same key afterward creates
   a new job because terminal states no longer reserve the uniqueness key.

The shared uniqueness policy selects only the `pr_key` argument, excludes kind and
queue, and covers `available`, `pending`, `running`, `retryable`, and `scheduled`.
Future PR job kinds must use the same JSON field/tag and policy to participate.
Insertion uses River `InsertTx`; uncommitted jobs are invisible to workers and
rollback removes them. Future environment-state updates must share that same
transaction with their job insertion. This phase has no environment transition.
Retries use River's default backoff, with five attempts for ping; invalid ping
arguments cancel permanently. Ping is idempotent. Future jobs must also tolerate
re-execution because workers provide at-least-once processing.

## Verification performed

- Live server → separate worker → PostgreSQL: job **10** was accepted over HTTP,
  processed, and recorded as `completed`, attempt **1**, with a finalization time.
  A temporary authenticated test session was used; its session/user were removed.
  The completed ping row remains available for inspection until normal retention.
- Build, vet, golangci-lint, and all existing tests including database integration
  tests passed. Queue tests cover completion, cross-kind/cross-queue active
  uniqueness, terminal-state release, transaction visibility/rollback, concurrency,
  and permanent cancellation. HTTP tests verify login/CSRF and key validation.
- `make migrate-up` was rerun successfully with nothing pending; `river-status`
  reports version 007. Entrypoints/app orchestration have no dedicated unit tests;
  their startup and shutdown were exercised through the live processes.
- Server and worker were stopped after verification; PostgreSQL was left running.

## Proposals requiring approval — not implemented

1. **Add River UI for job inspection.** River provides a self-hosted job dashboard:
   [official River UI documentation](https://riverqueue.com/docs/river-ui).
   Proposed local setup: pin a release, run it as a separate process/container on
   `127.0.0.1:8081`, and connect it to the same database. A container must use the
   Compose database hostname `postgres`, not host `localhost`. Review authentication
   and who may retry/cancel jobs before exposing it outside localhost.
2. **Validate configuration per process/feature.** Both binaries currently require
   OAuth and future integration settings through the shared loader. Separating the
   required subsets would let the worker need only database and queue configuration
   and avoid placeholders for unbuilt integrations.
3. **Keep migration credentials out of process arguments.** River's pinned CLI
   receives `--database-url`; commands are not echoed, but local process inspection
   can reveal arguments. Use PostgreSQL environment settings or a small migration
   wrapper before running migrations with sensitive shared-database credentials.
4. **Define deployment-wide concurrency before adding worker replicas.** The present
   cap is per queue per process. If later infrastructure needs one global build cap,
   agree on coordination or River's global concurrency offering before scaling.
5. **Before real environment jobs, define duplicate-event handling and per-kind
   timeouts.** Cross-kind uniqueness prevents simultaneous jobs for a PR, but does
   not itself preserve a newer SHA or guarantee a teardown follows a running build.
   That behavior belongs with environment state/reconciliation. The ten-second ping
   timeout and one-minute rescue threshold must also be revisited for long builds.

River owns its schema using its pinned migrations, following the
[official migration workflow](https://riverqueue.com/docs/migrations). No conflicting
architecture or additional production job kinds were introduced.
