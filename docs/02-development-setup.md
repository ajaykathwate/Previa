# 02 · Development setup

Configure, build, run and check Previa locally. Run commands from the repository
root in Bash or Zsh. On Windows, use WSL 2 with Docker Desktop's WSL integration
enabled (or Docker Engine installed inside the distribution).

Current schema: **Goose 13**, **River 007**. After pulling changes, run
`make migrate-up` and restart the server and worker.

## 1. Prerequisites

- Go **1.26.5 or newer** (required by `go.mod` and `tools/go.mod`).
- Git, GNU Make, curl, a SHA-256 tool (`sha256sum` or `shasum`) and OpenSSL.
- Docker Engine with the Compose plugin, or Docker Desktop running Linux containers.
- Internet access for the first dependency, tool and image downloads.

```sh
git --version
go version
make --version
docker compose version
```

**Expected:** each prints a version. No local PostgreSQL, Node.js or globally
installed Go tools are needed. Official Tailwind CSS **v4.1.18** is downloaded
once into ignored `bin/` and verified against pinned SHA-256 digests (Linux/WSL 2
and macOS x64/arm64; about 115 MB on Linux x64). Node.js 20+ is needed only for
the optional browser tests.

On WSL, if `docker version` reports a socket permission error, check that your
user is in the `docker` group (`id -nG`) and open a new terminal or run
`newgrp docker` after adding it.

## 2. Code and configuration

```sh
git clone <repository-clone-url> Previa
cd Previa
cp .env.example .env        # skip if .env already exists
go mod download
go mod download -modfile=tools/go.mod
```

`.env` is Git-ignored and grouped by process. Never commit it, print it or share it.
Pinned tools (Goose, templ, sqlc, golangci-lint, Air) run through
`go tool -modfile=tools/go.mod <tool>`.

| Variable | Process | How to set it | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | server, worker, migrations | Keep the example for the Compose database. | Application data, sessions and River jobs. |
| `HTTP_ADDR` | server | `127.0.0.1:8080` | Listen address. |
| `APP_BASE_URL` | server | `http://localhost:8080` | OAuth redirects, CSRF origin checks, cookie security. |
| `GITHUB_OAUTH_CLIENT_ID` / `_SECRET` | server | From a GitHub **OAuth App** ([04](04-github-sign-in-and-app.md)). | Dashboard sign-in. |
| `SESSION_SECRET` | server | `openssl rand -hex 32` | Signs OAuth state; at least 32 characters. |
| `MAX_CONCURRENT_BUILDS` | worker | `2` | Ping worker concurrency per process. |
| `BUILD_TIMEOUT` | worker | `12m` (max `24h`) | Build execution budget plus a one-minute grace. |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_PATH` | server, worker | From a GitHub **App** ([04](04-github-sign-in-and-app.md)). | App JWTs; the worker needs only these two for discovery. |
| `GITHUB_APP_SLUG`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` | server | Same GitHub App. | Installation page and verified linking. |
| `GITHUB_WEBHOOK_SECRET` | server | `openssl rand -hex 32`, same value in the App settings. | Verifies webhook signatures ([05](05-webhooks-and-pr-discovery.md)). |
| `SECRETS_ENCRYPTION_KEYS` | server (worker from Prompt 5) | `1:` + output of `openssl rand -base64 32`; comma-separate versions. | Encrypts repository secrets ([07](07-repository-secrets.md)). |
| `SECRETS_ACTIVE_KEY_VERSION` | server (worker from Prompt 5) | The version new values use, e.g. `1`. | Selects the key for new values. |
| `BUILD_CPUS`, `BUILD_MEMORY`, `BUILD_PIDS` | worker | Optional; defaults `2`, `4GiB`, `2048`. | Per-build limits for the isolated builder ([09](09-image-builds.md)). |
| `BUILD_DISK_BUDGET`, `BUILD_MAX_IMAGE_SIZE`, `BUILD_MIN_FREE_DISK` | worker | Optional; defaults `20GiB`, `4GiB`, `15GiB`. | Disk and image bounds; sizes take `KiB`, `MiB`, `GiB` or `TiB`. |
| `BUILDKIT_IMAGE`, `BUILD_EGRESS_IMAGE` | worker | Optional; defaults to the pinned rootless BuildKit image and `previa-build-egress:dev`. | Builder and egress proxy images. |

- Sign-in needs real OAuth values; placeholders start the server but cannot log in.
- The five server App settings are all-or-nothing. Omit them all to run with
  repository linking visibly disabled. A partial set fails validation.
- Without the two worker App settings, the worker processes only `ping`.
- An unset webhook secret makes `/webhooks/github` return 503.
- The two `SECRETS_*` values go together. Without them, the Secrets tab explains that
  secrets are not configured; a malformed keyring stops the server with a message
  that never shows key material. Restart the server after changing them.
- The `BUILD*` settings have working defaults. The worker validates them at start
  (an invalid value stops it with the variable's name) but does not build images yet;
  `make test-builder` uses them, and build execution will from Prompt 10.

### How configuration is loaded

| Command | Loads `.env`? |
| --- | --- |
| `make run-server`, `make run-worker`, `make dev` | Yes; already-exported variables take precedence. Restart Air after editing `.env`. |
| `make migrate-up`, `migrate-down`, `river-migrate-up`, `river-status`, `test-webhook`, `test-github-app`, `test-source`, `secrets-reencrypt`, `build-egress-image`, `test-builder` | Yes; falls back to `.env.example` for migrations if `.env` is absent. |
| Direct `go run`, manual Goose commands | No; run `set -a; . ./.env; set +a` once per terminal. |
| `make build`, `make lint`, `make generate` | No configuration needed. |
| `make test` | Database tests need an explicitly exported `TEST_DATABASE_URL`. |

Select another file with `ENV_FILE=.env.local make run-server`. Keep `.env` in
shell syntax and quote values containing spaces or shell metacharacters.

## 3. Start PostgreSQL

```sh
docker compose -f deploy/docker-compose.yml up -d --wait postgres
docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U previa -d previa
```

**Expected:** PostgreSQL 17 is healthy on `127.0.0.1:5432` and reports
`accepting connections`. Data persists in a named Docker volume.

## 4. Apply migrations

```sh
make migrate-up
```

**Expected:** Goose and River apply pending migrations or report none. Re-running
is safe. Neither tool receives credentials as process arguments (Goose reads
`GOOSE_DBSTRING`, the River adapter reads `DATABASE_URL`).

| Migration | Adds |
| --- | --- |
| 0001–0002 | Core tables, auth avatar field and SCS `sessions`. |
| 0003–0004 | PR/environment generations and the durable `environment_operations` outbox. |
| 0005 | Installation ownership/status/revision and repository access. |
| 0006–0007 | Repository `build_mode`, `build_intents` and revision fencing. |
| 0008 | Webhook receipts, lifecycle tombstones, PR metadata and `pr_syncs`. |
| 0009 | One lifetime environment row per PR. |
| 0010 | Nullable PR mergeability and derived `build_status` (existing rows start as Checking). |
| 0011 | Realtime triggers that raise `NOTIFY previa_events` after commits. |
| 0012 | Repository secrets: `key_version`, `updated_at`, name and ciphertext-size checks. |
| 0013 | Current-build log streams (attempt `epoch`, counters), line `epoch`/`kind`, line-size check and a log realtime trigger. |

Optional check that live updates are wired:

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa \
  -c "SELECT tgname FROM pg_trigger WHERE tgname LIKE '%_realtime'"
```

**Expected:** seven rows (six repository triggers and `build_log_streams_realtime`).
Without them the dashboard still works but refreshes only on navigation or your own
actions, and log pages update only on reload.

`make migrate-down` rolls back the latest application migration only (never River's
schema). Rolling back 0001 drops application tables; use it only on disposable data.

## 5. Build and check

```sh
make build    # compile CSS, generate templ/sqlc code, go build ./...
make lint     # go vet + pinned golangci-lint; expect 0 issues
make test     # go test ./...; database tests skip without TEST_DATABASE_URL
```

Run the PostgreSQL-backed tests after migrating:

```sh
set -a; . ./.env; set +a
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 ./...
```

**Expected:** all packages pass, including pool, migration-constraint, webhook,
discovery, scheduling, publication, secrets and realtime integration tests. Most
tests create isolated schemas and drop them afterwards. The migration-constraint,
auth repository and queue tests instead run in rolled-back transactions on your
migrated schema, so they fail if it is behind the code (for example, a
`key_version` or `epoch` NOT NULL error before migrations 12 or 13). No test resets
your data. External systems (GitHub, Docker) are fakes; real Docker builds run only
with `make test-builder`. Useful focused runs:

```sh
go test -count=1 -race ./internal/environments ./internal/pullrequests ./internal/webhooks
go test -count=1 -race ./internal/platform/realtime ./internal/buildlogs
go test -count=1 -race ./internal/source ./internal/repoconfig ./internal/secrets ./internal/builds   # no credentials needed
```

### Real-source check (optional)

With App credentials in `.env`, prepare one real PR's head commit end to end
(read-only: one PR read plus one archive download):

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa -t -A -F' ' \
  -c "SELECT i.github_installation_id, r.github_repo_id, p.number FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id WHERE p.state='open' AND r.accessible AND i.status='active' LIMIT 1"
TEST_GITHUB_INSTALLATION_ID=<1st> TEST_GITHUB_REPO_ID=<2nd> TEST_GITHUB_PR_NUMBER=<3rd> make test-source
```

**Expected:** `PASS`, with log lines stating that the archive declared the head
commit and extracted safely, then either the parsed config hash or a configuration
outcome such as `missing_file` when the repository has no `.previewrc.yaml`.

### Real image builder check (optional)

Needs Docker and internet access (Docker pulls the pinned BuildKit image; builds
pull `alpine` through the egress proxy). No database or GitHub access is used.

```sh
make build-egress-image   # expect an image ID; creates previa-build-egress:dev
make test-builder         # about 4 minutes
```

**Expected:** every `TestRealBuilder…` test passes: real and cached builds,
isolation probes (logged as `probe: … blocked`, with only `proxied-public REACHED`),
memory, disk, image-size, timeout and cancellation limits, fork cache separation and
cleanup. Afterwards only the long-lived `previa-build-egress` container and network
remain. To remove them and any build caches:

```sh
docker rm -f previa-build-egress && docker network rm previa-build-egress
docker volume ls -q --filter label=io.previa.kind=build-cache | xargs -r docker volume rm
```

### Browser regression checks (optional)

```sh
make browser-install   # once: Playwright 1.58.2 + Chromium
make test-browser
```

**Expected:** all scenarios pass against a temporary in-process server using real
session/CSRF middleware and fake GitHub/queue providers; no database or `.env`
needed. To use an existing Chromium, set
`UI_BROWSER_EXECUTABLE=/absolute/path/to/chrome` (for example a snap install at
`/snap/bin/chromium`). `UI_BROWSER_SCREENSHOTS=1` writes ignored screenshots to
`web/tests/browser/artifacts/`. Details: [web/tests/browser/README.md](../web/tests/browser/README.md).

## 6. Run the application

Terminal 1, server (or `make dev` for live reload):

```sh
make run-server
```

**Expected:** `database health check passed`, then `HTTP server listening`. Open
<http://localhost:8080/> and sign in with GitHub.

Terminal 2, worker:

```sh
make run-worker
```

**Expected:** `database health check passed`, then `River worker started`. The
server inserts jobs; the worker executes them. With App credentials it also runs
the `pr_discovery` queue (two workers, 45-second timeout) for sync pages and
mergeability checks. It always runs `build_log_prune` at start and hourly, deleting
build logs 14 days after their attempt finished. `Ctrl+C` drains active work before
exiting.

`make dev` runs Air: it compiles CSS, regenerates templates, rebuilds and restarts
the server when Go, templ, CSS, JS or SQL files change (polling works on
WSL-mounted folders). It does not restart the worker; restart that yourself after
worker changes. Refresh the browser after a rebuild.

Screen-by-screen behavior is in [03-user-guide.md](03-user-guide.md). Signed
webhook check, once the secret is configured and the server is running:

```sh
make test-webhook   # expect: HTTP 204: signed fixture accepted
```

## 7. Inspect jobs and operations

```sh
docker compose -f deploy/docker-compose.yml exec postgres psql -U previa -d previa
```

```sql
SELECT id, kind, queue, state, attempt, max_attempts, created_at, finalized_at
FROM river_job ORDER BY id DESC LIMIT 20;

SELECT queue, state, count(*) FROM river_job GROUP BY queue, state ORDER BY 1, 2;

SELECT id, pull_request_id, generation, revision, kind, state, attempts, available_at
FROM environment_operations ORDER BY id DESC LIMIT 20;

SELECT environment_id, epoch, generation, revision, state, next_sequence AS lines,
       truncated, omitted_lines, lost_lines, pruned_at
FROM build_log_streams ORDER BY updated_at DESC LIMIT 20;
```

- `\watch 2` refreshes a query every two seconds; `\q` exits.
- River states: `available` (waiting), `running`, `completed`, `retryable`,
  `discarded` (attempts exhausted), `cancelled`, `scheduled`.
- River deletes completed jobs after about 24 hours, so `river_job` is an
  operational view. `environment_operations` is the durable record of external work.
- Developer tools (`/developer-tools`) submits a `ping` job. The same key while a
  job is active returns `duplicate=true`; HTTP 202 proves insertion, the worker log
  `ping job processed` proves execution.

## 8. Rotate the secrets key

1. Add a new version to `SECRETS_ENCRYPTION_KEYS` (keep the old one), set
   `SECRETS_ACTIVE_KEY_VERSION` to it and restart the server (and later the worker).
2. Run `make secrets-reencrypt`. **Expected:** `Secrets: re-encrypted N, skipped 0 …`
   followed by `every value uses the active key; older key versions can be removed`.
   It prints counts only and is safe to repeat while the server runs.
3. Only then remove the old version and restart. If it reports values still on an
   older version, run it again first. Details: [07](07-repository-secrets.md#keys-provisioning-and-rotation).

## 9. Daily workflow and shutdown

1. `docker compose -f deploy/docker-compose.yml up -d --wait postgres`
2. After pulling: `make migrate-up` (and `make generate` if needed).
3. `make dev` (or `make run-server`) and `make run-worker` in two terminals.
4. Before finishing a change: `make build lint test`, the database test command,
   and `make test-browser` for UI changes.
5. Stop the database with `docker compose -f deploy/docker-compose.yml down`.
   Avoid `down -v` unless you intend to delete the local database.

## Troubleshooting

- **Docker permission or connection errors:** start Docker, confirm `docker version`
  shows a Server section, and on WSL check Docker Desktop's integration settings.
- **Port 5432 in use:** stop the other database, or change the host port in
  `deploy/docker-compose.yml` and `DATABASE_URL` together.
- **Password authentication failed:** Compose credentials live in the YAML, not
  `.env`; changing them does not change an already initialized volume.
- **Missing tables or skipped integration tests:** run `make migrate-up`, then export
  `TEST_DATABASE_URL`.
- **Stale configuration:** exported variables override `.env`; use a fresh terminal
  and restart the process after edits.
- **GitHub sign-in fails:** the OAuth callback must equal `APP_BASE_URL` +
  `/auth/github/callback`; use `localhost` consistently (not `127.0.0.1`). See [04](04-github-sign-in-and-app.md).
- **Tailwind download or checksum failure:** check curl, SHA-256 tool and GitHub
  release access; never bypass a checksum mismatch. Edit `web/styles`, not the
  generated `web/static/css/app.css`.
- **Stylesheet missing after a manual build:** run `make assets` before compiling;
  the server embeds assets at build time.
- **Live indicator never turns green:** apply migrations 0011 and 0013, and make sure
  any proxy in front of the server does not buffer `text/event-stream` ([14](14-realtime-updates.md)).
- **Builder errors:** `egress proxy image … is not available` means run
  `make build-egress-image`. `builder did not become ready` usually means user
  namespaces are disabled or Docker is too old for rootless BuildKit. `disk_low`
  means less than `BUILD_MIN_FREE_DISK` is free on Docker's storage. Repeated pull
  failures for public images can be Docker Hub's anonymous rate limit ([09](09-image-builds.md)).
- **Editor says templ components are undefined:** run `make generate && go build ./...`;
  generated `*_templ.go` files are ignored and must exist locally. If the build
  passes, restart the Go/templ language servers. `make watch-templates` regenerates
  templates on save without starting the server.
