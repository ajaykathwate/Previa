# 02 · Development setup

Configure, build, run and check Previa locally. Run commands from the repository
root in Bash or Zsh. On Windows, use WSL 2 with Docker Desktop's WSL integration
enabled (or Docker Engine installed inside the distribution).

Current schema: **Goose 19**, **River 007**. After pulling changes, run
`make migrate-up` and restart the server and worker.

## 1. Prerequisites

- Go **1.26.8 or newer** (required by `go.mod` and `tools/go.mod`; 1.26.8 fixes standard-library vulnerabilities `make vuln` reports in earlier releases).
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

For rootless builds on an Ubuntu 24.04 Docker host, apply BuildKit's
[user-namespace prerequisite](https://github.com/moby/buildkit/blob/master/docs/rootless.md#ubuntu-2404-or-later):

```sh
if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
    sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
fi
```

**Expected:** when the setting exists, sysctl prints
`kernel.apparmor_restrict_unprivileged_userns = 0`. This permits RootlessKit to
create its namespace; the setting is on the machine running Docker Engine.
Docker Desktop uses its own Linux VM. CI applies it on its temporary runner.
This command lasts until reboot; persistent configuration for a dedicated
production host is in [24 · Production deployment](24-production-deployment.md).

## 2. Code and configuration

```sh
git clone <repository-clone-url> Previa
cd Previa
cp .env.example .env        # skip if .env already exists
go mod download
go mod download -modfile=tools/go.mod
```

`.env` is Git-ignored and grouped by process. Never commit it, print it or share it.
Every line of `.env.example` is uncommented and has a one-line label above it:
`[Required]` (the process does not start without it), `[Required for <step>]`
(repositories, live updates, repository secrets, previews: that step does not
work without it) or `[Optional, default <x>]` (already set to its default;
change it only if needed). Replace every placeholder (values containing
`replace-with`, `/home/you`, `you@`, `your-` or `123456`). To skip a step for
now, delete that step's `[Required for …]` lines from your `.env` instead of
emptying them, and keep the Optional lines as they are: an empty number or
switch fails to load. Step-by-step instructions for every value, including
accounts to create and commands to run, are in
[technical-docs/config-setup.md](../technical-docs/config-setup.md);
`go test ./internal/platform/config` checks that every line is labelled and that
the Optional values equal the defaults.
Pinned tools (Goose, templ, sqlc, golangci-lint, Air) run through
`go tool -modfile=tools/go.mod <tool>`.

On a fresh checkout, generate the ignored template Go files before running
`go mod tidy` or direct Go build/test commands:

```sh
make templates
go mod tidy
(cd tools && go mod tidy)
git diff --exit-code -- go.mod go.sum tools/go.mod tools/go.sum
```

**Expected:** template generation succeeds and both tidy commands leave the four
module files unchanged. Without generation, directories containing only
`.templ` files are not Go packages; tidy can try to fetch Previa's own imports
from GitHub and fail with `git ls-remote` or authentication errors. CI runs
`make templates` before its module checks. `make build`, `make test` and
`make lint` already generate templates as prerequisites.

CI's separate Docker integration runner runs `make assets generate` before its
opt-in suites. When running those suites manually on a fresh checkout, run the
same command first; generated files are not passed between GitHub Actions jobs.

To verify a CI fix, commit and push the workflow and application changes, then
open the new Actions run and compare its commit SHA with `git rev-parse HEAD`.
**Re-run jobs** keeps the original run's commit and cannot test local changes or
a later fix. See [CI in action](../technical-docs/ci-in-action.md#92-manual-run-and-logs)
for the steps that identify the corrected scanner and rootless-builder setup.

Tidy the tools module from `tools/` as shown. Using
`go mod tidy -modfile=tools/go.mod` from the repository root changes the module
manifest without changing the source root; Go then interprets the application's
packages as part of the tools module and tries to fetch their imports remotely.
The existing `go tool -modfile=tools/go.mod <tool>` commands still select the
pinned tool dependencies correctly.

| Variable | Process | How to set it | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | server, worker, migrations | Keep the example for the Compose database. | Application data, sessions and River jobs. |
| `HTTP_ADDR` | server | `127.0.0.1:8080` | Listen address. |
| `APP_ENV` | server, worker | Optional; `development` (default) or `production`. | Production requires an HTTPS `APP_BASE_URL` and `GITHUB_WEBHOOK_SECRET`, refuses a GitHub App key readable by group or others, and turns the queue-ping tools off ([22](22-observability-and-hardening.md)). |
| `LOG_LEVEL` | server, worker, egress | Optional; `debug`, `info` (default), `warn`, `error`. | JSON logs with correlation fields; secrets redacted. |
| `INTERNAL_ADDR` | server | Optional; default `127.0.0.1:9090`. | `/livez`, `/readyz`, `/metrics`; never publish it. |
| `TRUSTED_PROXIES` | server | Optional; addresses or CIDR ranges; empty trusts nobody. | Reverse proxies allowed to set `X-Forwarded-For`/`-Host`. |
| `BACKUP_STATUS_FILE` | server | Optional; set in production Compose. | Its modification time becomes the last-backup metric ([24](24-production-deployment.md#backups-and-restore)). |
| `APP_BASE_URL` | server | `http://localhost:8080` | OAuth redirects, CSRF origin checks, cookie security. |
| `GITHUB_OAUTH_CLIENT_ID` / `_SECRET` | server | **Required** (the server does not start without them). From a GitHub **OAuth App** ([04](04-github-sign-in-and-app.md)). | Dashboard sign-in. |
| `SESSION_SECRET` | server | `openssl rand -hex 32` | Signs OAuth state; at least 32 characters. |
| `MAX_CONCURRENT_BUILDS` | worker | `2` | Builds running at once across all workers (and build workers per process); also the ping queue's concurrency ([14](14-housekeeping-and-capacity.md)). |
| `BUILD_TIMEOUT` | worker | `12m` (max `24h`) | Build execution budget plus a one-minute grace. |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_PATH` | server, worker | **Required for repositories** (linking, sync, builds). From a GitHub **App** ([04](04-github-sign-in-and-app.md)). | App JWTs; the worker needs only these two for discovery. |
| `GITHUB_APP_SLUG`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` | server | **Required for repositories.** Same GitHub App. | Installation page and verified linking. |
| `GITHUB_WEBHOOK_SECRET` | server | **Required for live updates and Automatic builds** (and in production). `openssl rand -hex 32`, same value in the App settings. | Verifies webhook signatures ([05](05-webhooks-and-pr-discovery.md)). |
| `SECRETS_ENCRYPTION_KEYS` | server, worker | **Required for repository secrets.** `1:` + output of `openssl rand -base64 32`; comma-separate versions. | Encrypts repository secrets ([07](07-repository-secrets.md)). |
| `SECRETS_ACTIVE_KEY_VERSION` | server, worker | **Required for repository secrets.** The version new values use, e.g. `1`. | Selects the key for new values. |
| `BUILD_CPUS`, `BUILD_MEMORY`, `BUILD_PIDS` | worker | Optional; defaults `2`, `4GiB`, `2048`. | Per-build limits for the isolated builder ([09](09-image-builds.md)). |
| `BUILD_DISK_BUDGET`, `BUILD_MAX_IMAGE_SIZE`, `BUILD_MIN_FREE_DISK` | worker | Optional; defaults `20GiB`, `4GiB`, `15GiB`. | Disk and image bounds; sizes take `KiB`, `MiB`, `GiB` or `TiB`. |
| `BUILDKIT_IMAGE`, `EGRESS_IMAGE` | worker | Optional; defaults to the pinned rootless BuildKit image and `previa-egress:dev`. | Builder image; egress proxy and prober image for builds and previews. |
| `PREVIEW_ALLOW_ROOT`, `PREVIEW_WRITABLE_ROOTFS` | worker | Optional; default `false`. | Server-wide opt-outs from running previews as UID 65532 with a read-only root ([10](10-preview-runtime.md)). |
| `PREVIEW_TMPFS_SIZE`, `PREVIEW_PIDS`, `PREVIEW_STARTUP_TIMEOUT` | worker | Optional; defaults `256MiB`, `512`, `120s`. | Preview `/tmp` size, process limit and readiness deadline. |
| `PREVIEW_MEMORY_BUDGET`, `PREVIEW_CPU_BUDGET`, `PREVIEW_MAX_PER_REPOSITORY` | worker | Optional; empty budgets use 60% of Docker memory and 75% of its CPUs; `10`. | What all previews may use together, and per repository. |
| `PREVIEW_OCI_RUNTIME` | worker | Optional, e.g. `runsc` where gVisor is installed. | Docker runtime for previews. |
| `WORKER_INTERNAL_ADDR` | worker | Optional; default `127.0.0.1:9091`. | `/livez`, `/readyz` (with capabilities), `/metrics`. |
| `BUILDS_PAUSED` | worker | Optional; default `false`. | The execution kill switch: builds wait; teardown, cleanup, routing, reports and housekeeping continue ([23](23-incident-runbook.md#execution-kill-switch)). |
| `SOURCE_WORKSPACE_ROOT` | worker | Optional; default `previa-sources` in the system temporary directory. An absolute path on a Linux filesystem. | Where each build's extracted source lives while it builds. |
| `ENV_TTL_HOURS` | worker | Optional; default `48`, `0` never expires. | Preview lifetime after its latest publication ([14](14-housekeeping-and-capacity.md)). |
| `HOUSEKEEPING_DRY_RUN`, `HOUSEKEEPING_GRACE` | worker | Optional; defaults `false`, `15m`. | Log housekeeping deletions without making them; the minimum age of an orphan. |
| `BUILD_CACHE_BUDGET` | worker | Optional; default `40GiB`. | All build caches together; the oldest unused ones are pruned first. |
| `PREVIEW_BASE_DOMAIN` | server, worker | **Required for builds and previews.** `preview.localtest.me` on one machine, or a domain you control (not the dashboard's host or a parent of it). | Preview hostnames below it ([11](11-routing-and-https.md)); turns routing on in the worker. |
| `APP_BASE_URL` | worker (when routing) | The dashboard origin, as on the server. | Unknown preview hosts are redirected to its `/previews/unavailable`. |
| `ROUTER_DYNAMIC_DIR` | worker (when routing), router | **Required for builds and previews.** A full path on a Linux filesystem, without `~` (`.env` does not expand it) or a trailing slash; `make router-up` creates it. | Where the route controller writes Traefik's route table. |
| `ROUTER_CONTAINER`, `ROUTER_ENTRYPOINT`, `ROUTER_API_ADDRESS` | worker | Optional; defaults `previa-router`, `websecure`, `127.0.0.1:8080` (must be loopback). | The router container, its HTTPS entry point and its in-container API. |
| `ACME_EMAIL`, `ACME_DNS_PROVIDER`, `ACME_DNS_CREDENTIAL_VARIABLE`, `ACME_DNS_CREDENTIAL_FILE` | router (`make router-up`) | **Required for builds and previews** (`make router-up` refuses without them, even on one machine; the file may hold any text there). Your ACME contact, lego provider (e.g. `cloudflare`), its credential variable (e.g. `CF_DNS_API_TOKEN`) and a `chmod 600` file holding the token. | Wildcard certificate over DNS-01; read by `deploy/traefik/compose.yml`, not by Go. |
| `ACME_CA_SERVER`, `ACME_DNS_RESOLVERS` | router | `ACME_CA_SERVER` is **required on one machine**: `https://127.0.0.1:1/directory` requests no certificate. With a real domain, Let's Encrypt **staging** (the default), then production. Resolvers are optional. | Switch to production only after staging issued a certificate. |

- Sign-in needs real OAuth values; placeholders start the server but cannot log in.
- The five server App settings are all-or-nothing. Omit them all to run with
  repository linking visibly disabled. A partial set fails validation.
- Without the two worker App settings, the worker processes only `ping`.
- An unset webhook secret makes `/webhooks/github` return 503.
- The two `SECRETS_*` values go together, on the server and the worker alike (the
  same keyring). Without them, the Secrets tab explains that secrets are not
  configured, and the worker starts previews only for repositories without secrets
  (a repository with secrets fails closed). A malformed keyring stops the process
  with a message that never shows key material. Restart after changing them.
- **Builds run only when the worker has everything they need:** the App settings,
  `PREVIEW_BASE_DOMAIN` with the routing settings, a reachable Docker Engine and the
  egress image. Otherwise the worker registers no build workers, logs
  `capability unavailable` with the reason, and the dashboard says no worker can
  build; requests wait durably. The full matrix is in
  [21](21-mvp-operations.md#capability-and-configuration-matrix).
- The `BUILD*`, `PREVIEW_*` and housekeeping settings (`ENV_TTL_HOURS`,
  `HOUSEKEEPING_*`, `BUILD_CACHE_BUDGET`) have working defaults; the worker validates
  them at start and stops with the variable's name if one is invalid.
- Routing is off until `PREVIEW_BASE_DOMAIN` is set. Then the worker requires
  `APP_BASE_URL` and an existing `ROUTER_DYNAMIC_DIR`, and both processes refuse a
  dashboard host equal to or below the preview domain (the server also warns when they
  share a registrable domain). The router must be running (`make router-up`) for
  previews to be published.
- Over HTTPS the dashboard's session and OAuth state cookies are named
  `__Host-previa_session` and `__Host-previa_oauth_state`; switching `APP_BASE_URL`
  between HTTP and HTTPS signs everyone out once.

### How configuration is loaded

| Command | Loads `.env`? |
| --- | --- |
| `make run-server`, `make run-worker`, `make dev` | Yes; already-exported variables take precedence. Restart Air after editing `.env`. |
| `make migrate-up`, `migrate-down`, `river-migrate-up`, `river-status`, `test-webhook`, `test-github-app`, `test-source`, `test-reporting`, `secrets-reencrypt`, `egress-image`, `test-builder`, `test-runtime`, `test-routing`, `test-housekeeping`, `test-e2e`, `housekeeping-dry-run`, `router-up`, `router-down`, `route-sync` | Yes; falls back to `.env.example` for migrations if `.env` is absent. `test-runtime`, `test-routing`, `test-housekeeping`, `test-e2e` and `test-reporting` use `DATABASE_URL` as `TEST_DATABASE_URL` unless that is exported (they create and drop their own schemas). |
| Direct `go run`, manual Goose commands | No; run `set -a; . ./.env; set +a` once per terminal. |
| `make build`, `make lint`, `make generate`, `make vuln`, `make observability-check`, `make release-images` | No configuration needed (the last two need Docker). |
| `make prod-smoke` | No; it writes its own settings into a temporary directory ([24](24-production-deployment.md#local-staging-smoke-test)). |
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
| 0014 | Route controller state (`environment_routes`, `route_table`), local teardown requests on PRs, `environments.teardown_generation`, a DNS-label check on hostnames and nullable `expires_at`. Fails if an existing hostname is not a lowercase DNS label. |
| 0015 | PR feedback state (`pull_request_reports`: wanted/reported report versions, digests, current check run, reporter lease, last error category) and `environments.last_error_code`. |
| 0016 | This database's instance identity (`platform_instance`), waiting notes on build requests (`waiting_revision`, `waiting_reason`) and `pr_reconciliations`. |
| 0017 | `worker_capabilities`: what each running worker can do, refreshed every 30 seconds, read by the server. |
| 0018 | The execution kill switch: build requests may wait with reason `paused`, and workers report `paused` ([22](22-observability-and-hardening.md#execution-kill-switch)). |

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
make vuln     # govulncheck; expect "Your code is affected by 0 vulnerabilities"
make test     # go test ./...; database tests skip without TEST_DATABASE_URL
make observability-check   # alert rules and their tests, Prometheus config, dashboard queries (Docker)
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

### Live PR feedback proof (optional)

Writes to GitHub, so run it only on a **disposable PR you are authorized to write
to**, in a repository where the App is installed with the permissions from
[04](04-github-sign-in-and-app.md). Put the PR's numeric identity in `.env` (or export
it), next to the App ID and key path:

```dotenv
TEST_GITHUB_INSTALLATION_ID=<installation id>
TEST_GITHUB_REPO_ID=<numeric repository id>
TEST_GITHUB_PR_NUMBER=<open PR number>
```

```sh
make test-reporting
```

**Expected:** `PASS` for `TestRealReportingOnADisposablePR` in about half a minute,
with log lines such as `created living comment …`, `created check run … (revision
1)`, `recreated living comment … after deletion of …` and finally `cleanup: deleted
comment …`. Only comment and check run IDs are logged. The PR must be open and must
not already have a Previa comment (the test stops if it does). Afterwards the PR has
no Previa comment; its head commit keeps three completed check runs named
"Previa preview (live proof)" because GitHub cannot delete check runs. If the test
is interrupted before its cleanup, delete the Previa comment on the PR by hand; an
unfinished run can be completed by running the proof again on the same commit after
deleting the comment. The database part uses a throwaway schema and leaves nothing
behind. Without the three variables the target stops with a message; with
`TEST_REPORTING` unset the test skips.

### Real image builder check (optional)

Needs Docker and internet access (Docker pulls the pinned BuildKit image; builds
pull `alpine` through the egress proxy). No database or GitHub access is used.

On Ubuntu 24.04, apply the host prerequisite above before starting the suite.
An exited builder now reports its status, exit code and bounded daemon logs;
`permission denied` plus `apparmor_restrict_unprivileged_userns` identifies a
host namespace restriction. A startup timeout retains the last probe error.

```sh
make egress-image         # expect an image ID; creates previa-egress:dev
make test-builder         # about 4 minutes
```

**Expected:** every `TestRealBuilder…` test passes: real and cached builds,
isolation probes (logged as `probe: … blocked`, with only `proxied-public REACHED`),
memory, disk, image-size, timeout and cancellation limits, fork cache separation and
cleanup. Afterwards only the long-lived `previa-egress` container and network
remain. To remove them and any build caches:

```sh
docker rm -f previa-egress && docker network rm previa-egress
docker volume ls -q --filter label=io.previa.kind=build-cache | xargs -r docker volume rm
```

### Real preview runtime check (optional)

Same prerequisites as the builder check (it builds tiny `alpine` apps first), plus
PostgreSQL for the admission-lock test, which uses a throwaway schema. No GitHub
access is used; secrets come from an in-memory store sealed with a throwaway key.

```sh
make test-runtime         # about 3 minutes
```

**Expected:** every `TestRealRuntime…` test passes: a hardened healthy candidate
(UID 65532, read-only root, no capabilities, no IPv6, redacted secret), fork secret
withholding, network probes logged as `probe: … blocked` except
`proxied-public REACHED`, unhealthy/crashing/cancelled starts cleaned up, operator
opt-outs, capacity refusals, two candidates racing for room for one (exactly one
starts) and refusal to remove objects Previa (or another Previa instance) did not
create. Preview networks (`previa-<instance>-pr-<id>`) and images are removed by
the tests; only `previa-egress` stays running.

### Real end-to-end check (optional)

Needs Docker, internet access the first time (alpine and Traefik images), the egress
image and PostgreSQL; no GitHub account, domain or DNS token. It runs the worker's own
pipeline against a fake GitHub over TLS and a Traefik router with its default
certificate, on a throwaway schema.

```sh
make test-e2e             # about a minute
```

**Expected:** `TestRealEndToEnd` passes and logs `published pr-7-…preview.test from
1111111; reported Ready; log has N lines`, `torn down: route removed, containers
removed, reported` (after `GitHub outage: N report calls rejected; the preview
published anyway and the report caught up`), `Automatic: the burst built only the newest commit, back at
pr-7-…; one living comment throughout` and `closed: route, containers, image and
network removed`. Set `TEST_E2E_LOG=1` for the worker's logs. Anything the run could
not remove fails the test. Afterwards only the `previa-egress` container and network
remain (`docker network ls --filter label=io.previa.managed`).

### Release images and production smoke test (optional)

The production shell scripts are tracked with executable mode `100755`:
`setup.sh`, `backup.sh`, `restore.sh` and `smoke.sh`. A Git checkout preserves
this mode; it is required because Make and the backup/restore drill invoke the
scripts directly. If copying the project loses those permissions, restore them:

```sh
chmod +x deploy/production/{setup,backup,restore,smoke}.sh
```

Needs Docker and, for the backup drill, `age` (`sudo apt-get install age`). It
builds the three release images, then runs the production Compose stack on this
machine under the project `previa-smoke` and removes it afterwards:

```sh
make release-images PREVIA_VERSION=v0.1.0-rc.1   # about 6 minutes the first time
make prod-smoke PREVIA_VERSION=v0.1.0-rc.1        # about 3 minutes
```

**Expected:** a line per check (`ok   dashboard home over HTTPS`, …) ending in
`smoke test passed: 40 checks` (43 with `SMOKE_OLD_DUMP`, 33 without `age`). Ports used: 127.0.0.1:18080,
18443 and 13000. What it covers and its options are in
[24](24-production-deployment.md#local-staging-smoke-test).

### Real housekeeping check (optional)

Needs Docker, the egress image and PostgreSQL; uses a throwaway schema and never
touches objects of your development database (another instance).

```sh
make test-housekeeping    # about 30 seconds
```

**Expected:** `TestRealHousekeepingRemovesOnlyThisInstancesOrphans` (young objects
kept, a dry run removing nothing, then the orphan and an unknown PR's container
removed while the serving preview and another instance's container stay) and
`TestRealHousekeepingPrunesUnderDiskPressure` (nothing pruned until a build waits
for disk, then an unavailable repository's cache and an untagged image go and a
cache in use stays) pass. Afterwards only `previa-egress` remains.

### Housekeeping dry run (optional)

```sh
make housekeeping-dry-run                    # or WORKSPACES=/abs/dir make housekeeping-dry-run
```

Runs one inventory, expiry and prune pass against your configured database and
Docker as a dry run and changes nothing. Needs migration 16. **Expected:** log lines
for each object it would remove, then `Dry run for instance <id> (nothing was
changed):` and counts of orphans, unknown PRs' objects, expired previews, untagged
images, workspaces and build caches.

### Real routing, publication and teardown check (optional)

Needs Docker, internet access the first time (Traefik, Pebble and alpine images), the
egress image and PostgreSQL. It creates and drops its own schema; no domain, DNS
token or GitHub access is used: a local Pebble ACME server issues the wildcard
certificate over DNS-01.

```sh
make test-routing         # about 5 minutes
```

**Expected:** every `TestRealRouting…` test passes and the first logs
`HTTPS: https://pr-…preview.test served by a Pebble-issued wildcard certificate
[*.preview.test]`. Afterwards only `previa-egress` remains; the test router, Pebble,
networks, volume, schema and images are removed. Set `TEST_ROUTING_LOG=1` for the
worker-side logs.

### The router (required for builds and previews)

On one machine, keep `PREVIEW_BASE_DOMAIN=preview.localtest.me`, set
`ROUTER_DYNAMIC_DIR` to a full path, create the credential file with any text
(`printf x > ~/.config/previa/dns-credential; chmod 600` it) and keep
`ACME_CA_SERVER=https://127.0.0.1:1/directory`; no certificate is requested and
browsers warn about Traefik's default one. With a real domain, its wildcard record
must point at this host and the credential file must hold a real DNS provider
token. Then:

```sh
make router-up            # checks settings, writes the route table, starts Traefik
docker logs -f previa-router
curl -v https://anything.$PREVIEW_BASE_DOMAIN/   # staging issuer; redirect to the unavailable page
make router-down
```

**Expected:** `Routes: … holds route table version N`, then within a minute a Let's
Encrypt **staging** wildcard certificate and a 302 to
`APP_BASE_URL/previews/unavailable`. `make router-up` first runs `make route-sync`,
which writes the accepted route table from PostgreSQL (the worker's route controller
does the same at start and on every route change); Traefik requests the certificate only once a
router uses it, and the table's fallback router does. Switch `ACME_CA_SERVER` to
production and remove the `previa-router_acme` volume only after staging works
([11](11-routing-and-https.md)).

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

**Expected:** `internal listener serving /livez, /readyz and /metrics`,
`database health check passed` (with `version`, `schema=19` and `environment`), then
`HTTP server listening`. Open <http://localhost:8080/> and sign in with GitHub.
`curl -s localhost:9090/readyz` answers `{"checks":{"database":"ok","schema":"ok"},"status":"ready",...}`.
The server refuses to start if migrations are behind (`run migrations first`).

Terminal 2, worker:

```sh
make run-worker
```

**Expected:** `database health check passed`, then `River worker started
execution=… paused=… reporting=… housekeeping=… discovery=…`, preceded by a `capability
unavailable` warning naming each missing dependency. `curl -s localhost:9091/readyz`
lists its checks and capabilities. The server records work; the
worker executes it. With App credentials it runs PR discovery and mergeability
checks; with builds configured ([21](21-mvp-operations.md#capability-and-configuration-matrix))
it also builds, publishes, reports and keeps house. It always runs
`build_log_prune` at start and hourly. `Ctrl+C` removes the worker's capability row,
lets running jobs finish for 30 seconds, then cancels them; an interrupted build
runs again on the next start.

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
  any proxy in front of the server does not buffer `text/event-stream` ([19](19-realtime-updates.md)).
- **Builder errors:** `egress proxy image … is not available` means run
  `make egress-image`. `builder did not become ready` usually means user
  namespaces are disabled or Docker is too old for rootless BuildKit. `disk_low`
  means less than `BUILD_MIN_FREE_DISK` is free on Docker's storage. Repeated pull
  failures for public images can be Docker Hub's anonymous rate limit ([09](09-image-builds.md)).
- **Editor says templ components are undefined:** run `make generate && go build ./...`;
  generated `*_templ.go` files are ignored and must exist locally. If the build
  passes, restart the Go/templ language servers. `make watch-templates` regenerates
  templates on save without starting the server.
