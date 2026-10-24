# 02 · Development setup

Configure, build, run and check Previa locally. Run commands from the repository
root in Bash or Zsh. On Windows, use WSL 2 with Docker Desktop's WSL integration
enabled (or Docker Engine installed inside the distribution).

Current schema: **Goose 42**, **River 007**. After pulling changes, run
`make migrate-up` and restart the server and worker. When the egress proxy or the
router changed (this version changed both), also run `make egress-image` (builds
are metered and may be limited to package registries) and `make router-up` (the
router now serves request counts for "Last opened" and idle previews).

This update adds member names/GitHub profile metadata (0032) and stable random
public UUIDs (0033). It needs no new environment variables or Docker image rebuild.
PR descriptions and execution health are added by 0034, and uploaded profile
photos (with GitHub-name prefill for never-edited blank names) by 0035, deactivated
members, in-app invitations, activity and workspace deletion by 0036, cached
head-commit messages for the PR page by 0037, the repository Setup tab's
checks, preview opens and hourly check allowance by 0038
([29](29-guided-onboarding.md)), pull request review pages, deployment
records and activity by 0039 ([30](30-reviews.md)), setup durations and the
review page's sealed test access by 0040 ([31](31-test-data.md)), and persistent
test data (the Test data tab, repository baselines and kept data) by 0041
([32](32-persistent-test-data.md)). Migration 0042 adds finite workspace
entitlements and bounded execution accounting ([33](33-usage-and-free-plan.md)).
**Stop the old execution worker before this cutover**, then restart it
after the migration installs the free plan as described below. Internal
database keys and stored jobs remain unchanged. Apply migrations before
starting the new binaries; old numeric HTML bookmarks redirect after access checks.
Use UUIDs for forms, streams and downloads.

```sh
make migrate-up          # loads DATABASE_URL from exported env, otherwise .env
make run-server          # automatically loads .env; exported values win
make run-worker          # restart separately after migration
```

**Expected:** Goose reaches **42**, River stays **007**, and the new server accepts
the schema (a server built for 42 refuses to start on an older schema with "run
migrations first"). Restart the worker too: setup checks run on any worker with
the GitHub App, including one without a preview domain; deployment records are
written by the worker that activates routes, and every worker prunes activity.
0039 records live previews as *Present at migration* deployments; it adds no
configuration. 0040 adds none either: test access passwords use the existing
`SECRETS_ENCRYPTION_KEYS` (without them only the account can be stored), and the
worker records setup durations from the next build on. 0041 adds four optional
worker settings (`PREVIEW_BASELINE_DIR`, `PREVIEW_BASELINE_MAX_SIZE`,
`PREVIEW_DATA_COPY_TIMEOUT`, `PREVIEW_BASELINE_MIN_INTERVAL`, all with defaults; see
[technical-docs/config-setup.md](../technical-docs/config-setup.md)); Test data stays off for every repository until an Owner or Admin turns it on.
Repository baselines are encrypted with the existing `SECRETS_ENCRYPTION_KEYS`:
a worker without them stores none and says so at startup (kept data still works). Sign in again (or use Your profile → GitHub account → Refresh)
to populate GitHub metadata. Login requests the read-only `user:email` OAuth scope
for the verified primary email; tokens are still discarded. No repository OAuth
scope is added. The account's first/last name is editable independently; refreshed
GitHub data never overwrites it. If email lookup fails, sign-in still works and the
profile explains that the email could not refresh. Existing saved email is retained.

### Deployment setup update (2026-10-18)

The final production setup ([25](25-production-deployment.md#the-final-deployment-at-a-glance))
adds one optional setting, `SENTRY_DSN`. Keep it empty in `.env` (the
`.env.example` line is already empty): nothing is reported and nothing leaves your
machine. With a DSN, the server and worker also send their redacted warnings and
errors to Sentry, `check-config` refuses a malformed value, and
`go run ./cmd/server sentry-test` (or `previa-server sentry-test`) sends one test
error. No migration and no image rebuild is needed for local work. CI now skips a
push or pull request that changes documentation only, and the Release workflow
runs only when started by hand from a tag: production builds its images on the
server from the tag.

### F3: finite free-plan cutover

The owner delegated the allowance choice and confirmed there are no hosted users
on 18 October 2026. Migration 0042 grants every existing active workspace a fresh
**300 build minutes per UTC calendar month, one concurrent build and two active
preview slots**. New workspaces receive the same finite entitlement. No historical
account/workspace elapsed time is imported, and no paid state or charge is created.
The cutover timestamp is recorded by the migration. There are no new configuration
variables or initialization commands.

Stop the old execution worker, run `make migrate-up`, then restart the server and
worker. `make migrate-up`, `make run-server` and `make run-worker` load `.env`
automatically and preserve exported overrides. Direct Go commands and the browser
test targets do not load `.env`; export the settings they need first
(`set -a; . ./.env; set +a`), including a disposable `TEST_DATABASE_URL` for tests.
**Expected:** Goose 42, normal startup, and **Usage** in the workspace sidebar.
Owners/Admins see workspace totals; Developers see granted repository technical
usage; Reviewers cannot open it. Existing previews, names and data remain intact.

`BUILDS_PER_ACCOUNT` and `BUILD_MINUTES_PER_ACCOUNT_DAY` keep their environment-key
spellings, but are additional **workspace** safety ceilings. Zero disables only
that extra ceiling; it never makes the free entitlement unlimited. Restart all
execution workers together when changing them. Usage reports the actual last
worker policy, including preview lifetime, idle sleep and log retention. Missing
worker policy is shown as unknown, not invented.

`make test-usage-browser` adds the F3 Chromium suite, including no-JavaScript
checks. It needs `TEST_DATABASE_URL` and uses isolated schemas. Set
`UI_BROWSER_SCREENSHOTS=1` to save 1440/390 px images in both themes under
`web/tests/browser/artifacts`; `UI_BROWSER_EXECUTABLE` selects a local Chromium.
Usage tests include migration 0042 up/down/up, concurrency, replay, UTC boundaries,
provisional crash recovery, and deadline cancellation during a database stall.
The broader `make test-browser` suite allows ten minutes for its 55 scenarios
(twelve minutes for the Go harness and cleanup) on a busy Docker development
host. Individual browser-action deadlines are unchanged.
See [33](33-usage-and-free-plan.md) for accounting and recovery details.

## 1. Prerequisites

- Go **1.26.10 or newer** (required by `go.mod` and `tools/go.mod`; 1.26.10 fixes standard-library vulnerabilities `make vuln` reports in earlier releases, most recently GO-2026-6809 in `crypto/x509`). With the default `GOTOOLCHAIN=auto`, an older local Go downloads 1.26.10 by itself.
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
and macOS x64/arm64; about 115 MB on Linux x64). The download retries up to five
times on a failed or dropped response from GitHub, and a verified copy already at
the destination is reused. Image builds keep it in a BuildKit cache mount, so a
rebuild, including a Previa preview of this repository, need not download it
again. Node.js 20+ is needed only for the optional browser tests.

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
production host is in [25 · Production deployment](25-production-deployment.md).

## 2. Code and configuration

```sh
git clone <repository-clone-url> Previa
cd Previa
cp .env.example .env        # skip if .env already exists
go mod download
go mod download -modfile=tools/go.mod
```

A checkout under `/mnt/c` (WSL) has `core.fileMode=false`, so Git records no
executable bit for a new script. Mark scripts the server runs directly when you add
them, for example `git update-index --chmod=+x deploy/production/new-script.sh`;
`TestDeployScriptsAreExecutableInGit` (in `internal/app`) fails until you do.

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

CI runs for pull requests into `master` (the default and release branch), for
merges into it, and when dispatched manually (**Actions → CI → Run workflow**,
choosing your branch); a push to `main` alone runs nothing, and neither does a
change to documentation only (`*.md`, `docs/`, `technical-docs/`, `test-docs/`). To verify a CI fix,
push it, open or update a pull request into `master` (or dispatch CI on your
branch), then open the new Actions run and compare its commit SHA with
`git rev-parse HEAD`.
**Re-run jobs** keeps the original run's commit and cannot test local changes or
a later fix. See [CI in action](../technical-docs/ci-in-action.md#92-manual-run-and-logs)
for the steps that identify the corrected scanner and rootless-builder setup.

Tidy the tools module from `tools/` as shown. Using
`go mod tidy -modfile=tools/go.mod` from the repository root changes the module
manifest without changing the source root; Go then interprets the application's
packages as part of the tools module and tries to fetch their imports remotely.
The existing `go tool -modfile=tools/go.mod <tool>` commands still select the
pinned tool dependencies correctly.

Every setting, which process reads it, its default and how to get its value is in
[technical-docs/config-setup.md](../technical-docs/config-setup.md), grouped as in
`.env.example`: shared, dashboard (server), GitHub App, repository secrets, builds,
previews, preview domain and router, housekeeping, tests and tooling, and
production only. The rules that most often stop a first run:

- Sign-in needs real OAuth values; placeholders start the server but cannot log in.
- The five server App settings are all-or-nothing. Omit them all to run with
  repository linking not offered (the Repositories page says it is not configured). A partial set fails validation.
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
  [22](22-mvp-operations.md#capability-and-configuration-matrix).
- The `BUILD*`, `PREVIEW_*` and housekeeping settings (`ENV_TTL_HOURS`,
  `HOUSEKEEPING_*`, `BUILD_CACHE_BUDGET`) have working defaults; the worker validates
  them at start and stops with the variable's name if one is invalid.
- **Repository baselines** (persistent test data, [32](32-persistent-test-data.md))
  are encrypted with the two `SECRETS_*` values on the worker and written to
  `PREVIEW_BASELINE_DIR` (an absolute, clean path; default `previa-baselines` in the
  system temporary directory, created with mode 0700). Without the keys the worker
  logs `capability unavailable … test-data baselines are not stored (kept data
  still works)` and keeps refresh jobs waiting.
- Routing is off until `PREVIEW_BASE_DOMAIN` is set. Then the worker requires
  `APP_BASE_URL` and an existing `ROUTER_DYNAMIC_DIR`, and both processes refuse a
  dashboard host equal to or below the preview domain (the server also warns when they
  share a registrable domain). The router must be running (`make router-up`) for
  previews to be published.
- `SENTRY_DSN` stays empty locally. A malformed value makes `check-config` fail;
  at startup the processes log `error reporting is off` and run without it.
- Over HTTPS the dashboard's session and OAuth state cookies are named
  `__Host-previa_session` and `__Host-previa_oauth_state`; switching `APP_BASE_URL`
  between HTTP and HTTPS signs everyone out once.

### How configuration is loaded

| Command | Loads `.env`? |
| --- | --- |
| `make run-server`, `make run-worker`, `make dev` | Yes; already-exported variables take precedence. Restart Air after editing `.env`. |
| `make migrate-up`, `migrate-down`, `river-migrate-up`, `river-status`, `river-ui`, `test-webhook`, `test-github-app`, `test-source`, `test-reporting`, `secrets-reencrypt`, `egress-image`, `test-builder`, `test-runtime`, `test-routing`, `test-housekeeping`, `test-e2e`, `test-reference`, `test-test-data`, `housekeeping-dry-run`, `router-up`, `router-down`, `route-sync` | Yes; falls back to `.env.example` for migrations and `river-ui` if `.env` is absent. `test-runtime`, `test-routing`, `test-housekeeping`, `test-e2e`, `test-reference`, `test-test-data` and `test-reporting` use `DATABASE_URL` as `TEST_DATABASE_URL` unless that is exported (they create and drop their own schemas). |
| Direct `go run`, manual Goose commands | No; run `set -a; . ./.env; set +a` once per terminal. |
| `make build`, `make lint`, `make generate`, `make vuln`, `make observability-check`, `make release-images` | No configuration needed (the last two need Docker). |
| `make prod-smoke` | No; it writes its own settings into a temporary directory ([25](25-production-deployment.md#local-staging-smoke-test)). |
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
| 0018 | The execution kill switch: build requests may wait with reason `paused`, and workers report `paused` ([23](23-observability-and-hardening.md#execution-kill-switch)). |
| 0019 | `pull_requests.mergeability_unresolved_at`: when the mergeability check gave up after eight attempts, so the dashboard shows **Unknown** instead of **Checking** forever; any new observation clears it. |
| 0020 | Backend previews ([11](11-backend-previews.md)): `preview_components` (what a deployed candidate ran next to its app: services, setup, processes, docs path, outbound count), `repository_destinations` (owner-approved outbound `host:port`) and `worker_capabilities.services` (the managed services a worker offers). |
| 0021 | `build_failure_dismissals`: a failed build request an owner dismissed (user, pull request, request revision), so it stops needing attention on the Overview until the pull request fails again. |
| 0022 | Fair sharing between accounts ([17](17-build-scheduling.md)): `build_intents.waiting_since`/`waiting_checked_at` (turns between accounts), waiting reasons `account` and `quota`, and `account_build_usage` (build seconds per account and UTC day). |
| 0023 | `build_log_streams.deleted_at`: an owner deleted a finished attempt's logs ([08](08-build-logs.md)). |
| 0024 | Live preview activity ([10](10-preview-runtime.md)): `preview_crashes` (one row per container stop of a live preview, so its output is copied to the build log once), `environments.visits`/`last_visited_at` and `preview_visit_counters` (the router's request counts). |
| 0025 | Live app output ([08](08-build-logs.md#live-app-output)): `app_output_watches` and `app_output_lines` (kept only while someone watches) and the `app_output_watches_realtime` trigger. |
| 0026 | Idle previews ([10](10-preview-runtime.md#idle-previews-sleep)): `environments.asleep_at`, `woke_at`, `wake_requested_at` and `wake_error`. |
| 0027 | Private previews ([12](12-routing-and-https.md#private-previews)): `repos.private_previews`. |
| 0028 | Opt-in build secrets ([07](07-repository-secrets.md)): `env_secrets.build`, false for all existing secrets; `worker_capabilities.build_timeout_ns` lets Starter kits validate against live worker ceilings. Server and worker require this schema. |
| 0029 | `worker_capabilities.private_previews`: all live routing workers must advertise a configured auth URL before an owner can enable private previews. Legacy workers default false; server and worker require this schema. |
| 0030 | Shared workspaces, roles/grants, invitation consent and handover, installation workspace ownership, manual-intent/publication authorization fences, and workspace consumption. Backfills existing connections without changing resource IDs. Coordinated server/worker cutover required. |
| 0031 | Workspace description, independent profile revision and authenticated small-logo storage. Existing memberships, connections and authorization revisions remain intact. |
| 0032 | Editable personal first/last names and cached GitHub metadata, separate from workspace permissions. |
| 0033 | Persisted random public UUID aliases for existing resources; internal relational keys and jobs stay stable. |
| 0034 | Persisted PR descriptions and revision-bound execution-job health; failed/exhausted workers no longer leave active build requests spinning. |
| 0035 | Uploaded profile photos (`user_avatars`, `users.avatar_version`, `users.custom_avatar`); never-edited blank names are filled from the cached GitHub name. No new configuration; the CSP now also allows images from `https://avatars.githubusercontent.com`. |
| 0036 | Member lifecycle: `workspace_memberships.removed_by`/`removed_repository_ids` (Deactivated list and re-invitation with previous access), an index for invitations addressed to a GitHub account (in-app Invitations), an index for per-member build activity, and `workspaces.deleted_at` (soft delete). |
| 0037 | `pull_requests.head_commit_*`: the PR page caches the head commit's message, author and time, fetched once per head SHA (GitHub's PR payloads omit them). Needs the GitHub App; without it the section is simply absent. |
| 0038 | Guided onboarding ([29](29-guided-onboarding.md)): `repository_setup_checks` (the latest setup check per pull request, its last completed result and why an attempt failed), `preview_opens` (who opened which deployment from Previa) and `setup_check_budgets` (60 checks per workspace and hour). Changes to the first two wake live pages. No configuration; existing data is untouched. |
| 0039 | Native review and feedback ([30](30-reviews.md)): `preview_publications` (one per route activation, numbered per pull request; live previews backfilled as *Present at migration*), `preview_components.config_hash`, `activity_events` and `activity_pruned` (90 days, 5,000 per pull request), `pr_review_instructions`, `preview_feedback_threads`, `preview_feedback_messages`, `pr_review_counters` and `review_write_budgets` (300 review changes per member and hour). Review changes wake live pages. No configuration. Rolling back drops reviews written since; back up first. |
| 0040 | Ready-to-test sample data ([31](31-test-data.md)): `preview_components.setup_ms` (how long each candidate's setup ran, shown on the review page) and `review_test_access` (one per repository: account text and an optional password sealed with the secrets keyring, revision, who changed and last revealed it). Saves wake live pages; reveals do not. No configuration. Rolling back drops stored test access; back up first. |
| 0041 | Persistent test data ([32](32-persistent-test-data.md)): `repository_sampledata_settings` (Test data settings, off by default), `baseline_refreshes` (requests, coalesced and fenced by sequence, with their log counters), `baseline_logs`, `repository_baselines` (metadata of encrypted dump files: never rows), starting-data columns on `preview_components` (`seed`, `seed_ms`, `data_source`, `data_detail`), `build_intents.start_source` (Reset's chosen source) and `worker_capabilities.baselines`. Settings, refresh and baseline changes wake live pages. Optional worker settings with defaults (§2). Rolling back drops Test data settings and baseline records; the files in `PREVIEW_BASELINE_DIR` are then unreferenced and can be deleted. |


### Shared workspace cutover

Stop Air/the server and drain/stop the old worker before applying migration 0030.
Keep your pre-upgrade backup. Do not leave an owner-based old server accepting
requests alongside workspace-aware code; this migration changes authorization.
Existing preview containers can remain and their old labels resolve through PR IDs.

```sh
make migrate-up
make build
make run-server      # terminal 1
make run-worker      # terminal 2
```

**Expected:** Goose reports schema 31; server/worker pass the schema check. Existing
installers see their original repositories in a backfilled workspace. Sign in,
open **Members** in the sidebar, invite a Developer/Reviewer, and select explicit
repositories. No new environment variable, notification integration or frontend
runtime is required. Make's run/migration targets load `.env`; direct Go commands
and test targets require manual exports as described below. Air rebuilds code and
assets but does not apply migrations automatically.

Rolling 0030 back is safe only before team data exists; it copies workspace
consumption back to the legacy ledger. Invitations, extra memberships, new
workspaces or transferred ownership make that rollback refuse data loss. Migration
0031 Down removes descriptions/logos and profile revisions; reapplying Up does not
restore the removed profile data. Use a forward fix or restore a rehearsed pre-upgrade backup with its matching previous
binaries. See [workspaces](28-shared-workspaces.md#upgrade-and-verification).

### Workspace profile and navigation update (0031)

If already on schema 30, stop the server and worker, then run `make migrate-up`,
`make build`, and restart both with the commands above. New binaries require 31;
Air does not migrate the database. No new environment variables or services are
needed. The existing 256 KiB HTTP form cap allows a 192 KiB logo plus multipart
fields; PNG/JPEG uploads are also limited to 512 × 512 pixels by the server.
Logos live in PostgreSQL and are served to active members only. Include them in
the normal database backup. Avoid rolling 0031 Down unless losing logo/description
changes is intentional.

**Expected:** Members and Workspace settings appear directly in the sidebar.
Members contains joined people and a separate Invitations view; Settings contains
editable name/description, logo and ownership. Changing identity does not revoke
preview cookies or cancel builds. Preview listings remain at `/previews`, including
a read-only view for Reviewers. A workspace is one team with many members/repos;
people can belong to multiple workspaces. Old workspace preview bookmarks redirect.

Optional check that live updates are wired:

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa \
  -c "SELECT tgname FROM pg_trigger WHERE tgname LIKE '%_realtime'"
```

**Expected:** eight rows (six repository triggers, `build_log_streams_realtime` and
`app_output_watches_realtime`).
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

`make lint` uses [.golangci.yml](../.golangci.yml): golangci-lint's default
linters plus `gocognit` (a function above cognitive complexity 30 must be split
into named steps), `nestif`, `gocritic` and a set of `revive` style rules
(package comments, error strings, unused parameters, context first after
`*testing.T`). Tests are exempt from the two complexity checks. A finding fails
the command and CI; fix the code rather than raising the limits.

Run the PostgreSQL-backed tests after migrating:

```sh
set -a; . ./.env; set +a
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 ./...
```

**Expected:** all packages pass, including pool, migration-constraint, webhook,
discovery, scheduling, publication, secrets and realtime integration tests. Most
tests create isolated schemas and drop them afterwards. Queue tests migrate River
in their own temporary schemas, so a running development worker cannot claim their
jobs. The migration-constraint and auth repository tests run in rolled-back
transactions on your migrated schema, so they fail if it is behind the code (for example, a
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
comment …`. Only comment and check run IDs are logged. The PR must be open. Each
run uses a unique live-proof comment marker, so an existing preview comment is
preserved. Afterwards only the test's comments are removed; its head commit keeps
three completed check runs named
"Previa preview (live proof)" because GitHub cannot delete check runs. If the test
is interrupted before its cleanup, remove only comments whose HTML marker starts
with `<!-- previa:live-proof `, and complete any unfinished live-proof check runs
through GitHub's API. A rerun creates a new proof scope and does not clean up a
previous interrupted run. The database part uses a throwaway schema and leaves nothing
behind. Without the three variables the target stops with a message; with
`TEST_REPORTING` unset the test skips.

### Repository compatibility settings

After pulling these changes, run `make migrate-up` before restarting the server
and worker: Goose must reach **28** (River migrations remain separate). Existing
secrets remain runtime-only. On the repository's **Secrets** tab, Allow opens a
confirmation modal; explicitly opt in a scoped test token for private dependency
builds. Use `RUN --mount=type=secret,id=NAME,required=true` in the Dockerfile.
For private base images, allow `REGISTRY_AUTH` with one `host username token`
line per registry. It never reaches app containers or Dockerfile steps. The same
encryption keyring is required on server and worker; forks receive no credentials.

`.previewrc.yaml` may now set `context: apps/web` (Dockerfile and COPY paths are
relative to it) and `build_timeout: 10m` (image stage). Keep the config at the root.
To permit a 20-minute image build, set `BUILD_TIMEOUT=30m` in `.env` and restart
the worker with `make run-worker`; that command loads `.env` automatically.
Direct `go run`/`go test` commands do **not** load `.env`: export the needed values
manually. Requests above the worker ceiling are refused. The overall deadline
still includes source and startup, and archive limits still cover the whole repo.

An empty `PREVIEW_SERVICE_CATALOG` now also offers pinned **MySQL 8.4**. A custom
catalog is unchanged: add `mysql:8.4=image@sha256:digest` to offer it, or use `none`
to turn managed services off. MySQL consumes 1 CPU and 1 GiB per candidate (including
384 MiB data tmpfs); allow room for the app, setup and any old preview too. The
service image is pulled by the worker on first use, and readiness requires an
authenticated query. See [backend previews](11-backend-previews.md) for variables.

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
memory, disk, image-size, timeout and cancellation limits, opt-in secret mounts,
missing/fork-secret refusal, fork cache separation, image reuse bound to both
repository IDs, and cleanup. Afterwards only the long-lived `previa-egress` container and network
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

For the new managed-service check alone (Docker and image download access needed):

```sh
TEST_RUNTIME=1 go test -count=1 -v -timeout 10m -run '^TestRealRuntimeManagedMySQL$' ./internal/runtime
```

Expected: MySQL starts under the real service restrictions, accepts authenticated
queries, refuses a wrong password, starts with empty data on its second candidate,
creates no persistent volume, and all test containers and the service network are
removed. The pinned service image remains available for reuse.

**Expected:** every `TestRealRuntime…` test passes: a hardened healthy candidate
(UID 65532, read-only root, no capabilities, no IPv6, redacted secret), fork secret
withholding, network probes logged as `probe: … blocked` except
`proxied-public REACHED`, unhealthy/crashing/cancelled starts cleaned up, operator
opt-outs, capacity refusals, two candidates racing for room for one (exactly one
starts) and refusal to remove objects Previa (or another Previa instance) did not
create. Preview networks (`previa-<instance>-pr-<id>`) and images are removed by
the tests; only `previa-egress` stays running.

### Real end-to-end check (optional)

Needs Docker, internet access the first time (Alpine, BusyBox and Traefik images), the egress
image and PostgreSQL; no GitHub account, domain or DNS token. It runs the worker's own
pipeline against a fake GitHub over TLS and a Traefik router with its default
certificate, on a throwaway schema. Each test worker gets a separate
`previa-egress-test-…` container and outward network, so its forwarding policy
cannot overwrite a running development worker's approvals. Production continues
to use the shared `previa-egress` gateway.

```sh
make test-e2e             # about 20 minutes; the first run also pulls PostgreSQL and Redis
make test-reference       # only the ready-to-test reference journeys, about 2 minutes
make test-test-data       # only the persistent test data journeys, about 4 minutes
```

`make test-e2e` runs `TestRealEndToEnd`, `TestRealBackendEndToEnd`,
`TestRealReferenceJourneys`, `TestRealTestDataJourneys` and `TestRealRoleJourney`
(the core flow through the server's own routes, person by person: a signed webhook
opens the PR, refused roles cannot build, the granted Developer builds, the
Reviewer opens the preview and records and resolves a finding across two
deployments, the Owner tears it down); `make test-reference`
runs only the reference journeys ([31](31-test-data.md#reference-journeys)) and
`make test-test-data` only the persistent test data journeys
([32](32-persistent-test-data.md#how-it-is-verified)): a baseline built from the
default branch and refreshed by a merge, data kept across a commit and a Rebuild,
Reset to the baseline or to empty and seed, a fallback, a broken migration and a
fork. It logs one line per step, ending with `teardown: no container, image,
network or volume remains`. The backend test compiles
a small API fixture (`internal/app/testdata/backend`) for the Docker host's
architecture, uses `github.com:22` as the approved outbound destination and the
public names `localtest.me` and `169.254.169.254.nip.io` (which resolve to loopback
and metadata addresses) as refused ones, so it needs internet access.

**Expected:** `TestRealEndToEnd` passes and logs `published pr-7-…preview.test from
1111111; reported Ready; log has N lines`, `torn down: route removed, containers
removed, reported` (after `GitHub outage: N report calls rejected; the preview
published anyway and the report caught up`), `Automatic: the burst built only the newest commit, back at
pr-7-…; one living comment throughout` and `closed: route, containers, image and
network removed`. `TestRealBackendEndToEnd` passes and logs `backend preview at …:
database …, 2 items, job processed by the worker`, `isolation: services reach
neither the internet, the egress proxy, the host nor other previews, and no other
preview reaches them`, `outbound: approved destination reached; refused the rest`,
`rebuild: fresh database … replaced …`, `failed migration: setup_failed, the
earlier preview and its data keep serving` and `teardown: no container, image,
network or volume of either preview remains`. `TestRealReferenceJourneys` passes and
logs `shop at …: 2 seeded orders, setup completed in …`, `reset: the tester's order
is gone; deployment 2 of the same commit serves the seed again`, `failed seed:
setup_failed; deployment 2 keeps serving its seeded data`, `notes at …: the revealed
test access signs in to 3 seeded notes`, `fork: no secrets, no test login, and the
preview says so` and `teardown: no container, image, network or volume of any
preview remains`. `TestRealRoleJourney` passes in under a minute and logs its ten
steps, from `1. A signed-out visitor is sent to GitHub sign-in; signed in, the Owner
sees the connected repository` to `10. GitHub closes the PR: its review stays
readable and nobody can build it`, then `usage: 2 physical attempts`. To run only it:
`TEST_E2E=1 go test -count=1 -v -run '^TestRealRoleJourney$' ./internal/app` with
`.env` loaded and a disposable `TEST_DATABASE_URL` exported. The basic preview also serves 50
consecutive HTTPS requests before the failed-rebuild check. Set `TEST_E2E_LOG=1`
for worker logs and Traefik debug diagnostics; a failing test captures the router's
last 80 log lines before removing it. Anything the run could not remove fails the
test. The test's gateway and network are also removed. Existing development
previews, their networks, and the shared `previa-egress` gateway remain intact.

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
`smoke test passed: 45 checks` (48 with `SMOKE_OLD_DUMP`, 33 without `age`). Ports used: 127.0.0.1:18080,
18443 and 13000. What it covers and its options are in
[25](25-production-deployment.md#local-staging-smoke-test).
The script reads the required schema version and alert count from the checkout;
build matching images before running it. Internal-listener exposure is checked
on the smoke containers' port bindings, so development listeners on 9090/9091
can stay running while this isolated stack is tested.

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
curl -v https://anything.$PREVIEW_BASE_DOMAIN/   # staging issuer: certificate verification fails
make router-down
```

**Expected:** `Routes: … holds route table version N`, then within a minute a Let's
Encrypt **staging** wildcard certificate. Browsers and ordinary `curl` reject that
issuer; Chrome reports `NET::ERR_CERT_AUTHORITY_INVALID`. A diagnostic staging
request using `curl -vk` shows a 302 to `APP_BASE_URL/previews/unavailable`.
`make router-up` first runs `make route-sync`,
which writes the accepted route table from PostgreSQL (the worker's route controller
does the same at start and on every route change); Traefik requests the certificate only once a
router uses it, and the table's fallback router does. After staging issuance works,
set `ACME_CA_SERVER=https://acme-v02.api.letsencrypt.org/directory` and
`ACME_STORAGE_FILE=/acme/acme-production.json` in `.env`, then run
`ENV_FILE=.env make router-up` again. This preserves the staging file and restores
existing preview network attachments after the router is recreated. Keep the
production storage path on later starts. Once issuance completes, ordinary `curl`
must verify the preview certificate without `-k`; Chrome should open the app or its
private sign-in flow. The app needs no rebuild. These settings belong to Compose,
not the Go config loader; Make loads `.env` automatically, and exported values
take precedence. See [12](12-routing-and-https.md) for storage and TLS details.

The router serves Prometheus metrics on its loopback API address; the worker reads
each preview's request count there every minute (`ROUTER_METRICS_ADDRESS`, empty =
`ROUTER_API_ADDRESS`). Check it with:

```sh
docker exec previa-router wget -qO- http://127.0.0.1:8080/metrics | grep -c traefik_service_requests_total
```

**Expected:** a number above 0 once a preview has been opened. A worker that cannot
read it logs `preview visits not read; retrying every minute`, and no preview is put
to sleep while that lasts.

**Private previews on one machine.** The router container must reach the server for
its sign-in check. Run the server on all interfaces and point the worker at it:
`HTTP_ADDR=0.0.0.0:8080` and
`PREVIEW_ACCESS_URL=http://host.docker.internal:8080/previews/auth` in `.env`, then
`make migrate-up`, `make router-up`, and restart both server and worker. Set the URL
on all routing workers; production sets it in the Compose file. The **Private
previews** switch is not offered, and a setup explanation shows instead, until all live routing
workers advertise it (workers expire after 90 seconds). The server checks the same
capability on POST, so submitting an old form cannot enable an unconfigured feature.

**Expected:** the switch becomes available after configured workers start. A
private preview sends anonymous visitors to GitHub sign-in and lets authorized
visitors through. A configured URL must still be reachable from the router; the
capability advertises configuration, not an HTTP health check. If private previews
were enabled before this guard, they remain private and deny access without an auth
URL. New publications fail with `private_preview_unconfigured`, rather than
repeated `route_not_converged` failures. Startup/recovery refreshes an existing deny
route when settings change, even if the route-table version is unchanged. Retry a
failed build after configuring and restarting; historical River errors remain in
the job history. Enabling public access still requires the owner's confirmation.

On WSL with Docker Desktop, an explicit `HTTP_ADDR=0.0.0.0:8080` creates an IPv4
listener. WSL can forward a dual-stack IPv6 listener only on Windows `::1`, which
makes Docker Desktop's IPv4 `host.docker.internal` connection fail even though the
server works at WSL `localhost`. Restart the server after upgrading this binding
fix (Air reloads changed Go code). Verify from inside the router:

```sh
docker exec previa-router wget -S -T 5 -O - http://host.docker.internal:8080/previews/auth
```

**Expected:** HTTP 403 `Not a preview.` without forwarded preview headers. That
proves the auth server is reachable; an anonymous request through the private
preview URL should receive a 302 to the dashboard's sign-in flow. A refused or
timed-out connection is a networking issue, and Traefik may return HTTP 500.
Hostname or unspecified listener addresses retain Go's usual binding behavior;
explicit IPv6 addresses remain supported.

An operation retry arriving before the durable retry deadline or while a previous
attempt holds a live lease now snoozes until it can run. Waiting does not consume a
River attempt or append an `already leased` error.

If a build stops at **Publish** with `The preview is healthy. It is not routed
yet.`, the app has passed readiness, but the router has not confirmed publication.
For an existing private repository, check the worker's startup warnings and
`localhost:9091/readyz` for private preview support. An unset `PREVIEW_ACCESS_URL`
is now named in a `capability unavailable` warning. Stop and rerun
`make run-worker` after changing worker code or `.env`; restart Air/server too
after changing `HTTP_ADDR`. `make dev` rebuilds only the server. On restart,
startup sync refreshes the routes and pending operations resume; retry the build
if its publication already failed. Do not change the repository to public to
work around an authentication configuration error.

Private Preview changes save the setting, route version and River sync job in
one transaction. A failed enqueue leaves the setting unchanged. Jobs deduplicate
per route version, allowing a newer change while an older sync is running.
App output follows the running app's stdout/stderr via the worker's `app_output`
queue; Build logs cover building and startup instead. After code updates, restart
both server and worker so they use the corrected queue options. These queue fixes
alone need no migration; the private-preview configuration guard requires 0029.

**Router recreation and merge recovery.** After upgrading these fixes, restart
`make run-worker` (Air reloads only the server). No new migration is needed beyond
0029. Worker startup and its one-minute recovery pass now restore the router's
preview-network attachments from the accepted routes, even if the route file is
unchanged. This recovers HTTP 502 after recreating the router. `make router-up`
also preserves the existing attachments during recreation.

An accepted owner build clears an earlier local teardown request atomically.
Rebuilt live previews, including existing rows with an old marker, remain eligible
for the normal 15-minute GitHub reconciliation. A received merge webhook records
`merged` and queues teardown immediately; reconciliation provides recovery when
the webhook is missed. Verify that the PR page shows **Merged**, its URL redirects
to `/previews/unavailable`, and its preview containers are absent. An updated GitHub
comment alone does not prove that a merge delivery reached this local server.

```sh
go test ./internal/environments ./internal/housekeeping ./internal/routing ./internal/pullrequests ./internal/reporting
make test-routing
```

Export `TEST_DATABASE_URL` for the direct Go command as described above; Make
loads `.env` and supplies its database fallback for the real routing suite.

To verify these flows against local PostgreSQL, export `.env` explicitly for
direct Go commands (Make's run targets load it automatically):

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="${TEST_DATABASE_URL:-$DATABASE_URL}"
go test ./internal/appoutput ./internal/previewaccess ./internal/auth ./internal/pullrequests ./internal/environments ./internal/routing ./internal/platform/workers
```

**Expected:** tests pass, including real River enqueue/deduplication, stale
follower replacement, privacy rollback/configuration guards, River snoozes without
spent attempts, same-version route refresh, and OAuth return to the requested PR
logs. Tests use disposable schemas. Automatic builds offer **Build now** only
during the quiet period; queued/running requests show their progress as a status.

### Focused build stress and fuzz checks

Use an exported `TEST_DATABASE_URL` pointing at a disposable PostgreSQL database
at Goose 47 / River 007. Direct `go test` commands do **not** load `.env`;
export test settings first. Existing Make integration targets load `.env` as
explained above, but an exported test database overrides the fallback.

```sh
go test -race -shuffle=on -count=5 -run '^TestBurstAdmissions' ./internal/environments
go test -race -run '^$' -fuzz '^FuzzBuildConfiguration$' -fuzztime=60s -parallel=2 ./internal/repoconfig
go test -race -run '^$' -fuzz '^FuzzArchivePathContainment$' -fuzztime=60s -parallel=2 ./internal/source
```

**Expected:** simultaneous builds stay within global, workspace and free-plan
limits; capacity-blocked work does not reserve allowance or reach the runtime. Arbitrary
configuration and archive-path inputs produce no panic, unsafe accepted path,
nondeterministic configuration or operator-limit escape. The archive fuzzer tests
validation in memory and writes no extracted files. Seed corpora also run during
ordinary package tests; mutation campaigns require `-fuzz`. Go stores interesting
inputs in its fuzz cache and failing inputs under the package's `testdata/fuzz`.

For real image building, service readiness, swaps, failure cleanup and complete
worker journeys, run `make test-builder`, `make test-runtime`, `make test-routing`,
`make test-housekeeping` and `make test-e2e` **sequentially**. The E2E journeys
assert Docker resource deltas; overlapping Docker suites would invalidate those
checks. These commands use disposable schemas and test-owned resources. Keep the
development server/worker and their database separate from the test fixtures.

If the machine or Docker restarts while a real suite runs, its fixtures stay
behind (containers labelled `io.previa.instance=<test instance>`, `previa-rt-*`
routing fixtures, their networks and `previa/<instance>-pr-*` images). Remove only
those: detach `previa-egress` from a fixture network
(`docker network disconnect -f <network> previa-egress`) before removing it.

### Role and access matrix

`TestEveryRouteRespectsEveryRole` (`internal/app`) assembles the server exactly as
`RunServer` does, with a fake GitHub as its App, and sends **every route** the
router serves as eight people: signed out, Owner, Admin, a granted Developer, a
granted Developer whom GitHub no longer lets read the code, a granted Reviewer, a
removed member and another workspace's owner. Repository routes run against a
granted and an ungranted repository, with valid forms. It needs only an exported
`TEST_DATABASE_URL` and runs in the ordinary `go test ./...`:

```sh
go test -race -count=1 -run '^(TestEveryRouteRespectsEveryRole|TestPagesOfferOnlyWhatPeopleCanUse)$' ./internal/app
```

`TestPagesOfferOnlyWhatPeopleCanUse` crawls every page each of those people can
open and fails on a disabled control, a form the server would refuse them or a
link that is refused or bounces them (see [21](21-ui-architecture.md)). To list
what it finds without failing, run `USABILITY_DUMP=/path/usability.tsv go test
-count=1 -run '^TestUsabilityDump$' ./internal/app`.

**Expected:** `ok` in about a minute. Every answer matches `accessMatrix`, the
database afterwards matches each person's fingerprint (refused requests change
nothing), each person's sidebar matches `accessNavigation`, and the crawl finds
nothing. A new route fails the test with `… is not in accessMatrix: classify who
may use it`; to see what the server now answers, record the observed matrix with
`ACCESS_MATRIX_DUMP=/path/matrix.tsv go test -count=1 -run '^TestAccessMatrixDump$'
./internal/app`, review it against the roles in [28](28-shared-workspaces.md#roles)
and [30](30-reviews.md#access), then update the table.

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
`web/tests/browser/artifacts/`. `UI_TEST_PATTERN='<regex>'` reruns only the
scenarios whose titles match (for example `UI_TEST_PATTERN='slow|offline'`); the
whole suite takes about two minutes. Details: [web/tests/browser/README.md](../web/tests/browser/README.md).

To verify shared workspace flows against real PostgreSQL:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
go test -race ./internal/workspaces ./internal/previewaccess ./internal/buildlogs ./internal/appoutput ./internal/environments ./internal/runtime
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-workspace-browser
```

**Expected:** fixtures create/drop isolated schemas; roles, grants, callback binding,
shared quota enforcement, token/cookie/stream revocation, safe rollback and handover
pass. Browser scenarios cover access editing and modal confirmation,
light/dark/mobile layouts, direct navigation, search, creation/switching,
invitation acceptance without JavaScript, in-place saves with toasts (and their
no-JS fallback), profile photo upload/visibility, logo persistence, stale edits
and ownership acceptance. Only GitHub responses are fixtures; sessions,
CSRF, workspace handlers and PostgreSQL are real. `RUN_WORKSPACE_BROWSER_TESTS=1`
enables that suite for direct Go runs; browser tooling remains optional.
`UI_BROWSER_SCREENSHOTS=1` also writes `workspace-*.png` captures, including the
open in-place editor and toast. The auth persistence test runs in a rolled-back
transaction on `TEST_DATABASE_URL`'s own schema, so migrate that database first
(`DATABASE_URL="$TEST_DATABASE_URL" make migrate-up`) or it fails at sign-in.

To verify the repository Setup tab against real PostgreSQL:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
go test -count=1 ./internal/onboarding ./internal/pullrequests ./internal/workspaces
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-setup-browser
```

**Expected:** the Go tests create and drop isolated schemas and cover who may see
setup, check requests (once per commit, the cooldown and hourly allowance), the
worker's fences (a newer request, a pushed commit, lost access, a closed pull
request), GitHub failures that keep the last result, preview opens and the
Overview's journey. The browser suite (`RUN_SETUP_BROWSER_TESTS=1` for direct Go
runs) takes an owner from a check to an opened preview with live updates through
real `LISTEN`/`NOTIFY`, checks the pull request picker, the Developer and Reviewer
views and the no-JavaScript path, with no CSP errors. Test-only routes stand in for
the worker's GitHub reads, a push, a build and a deployment. `UI_BROWSER_SCREENSHOTS=1`
writes `setup-*.png` captures (desktop light and dark, mobile).

To verify review pages, deployment records, activity, test access and Reset test data against real PostgreSQL:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
go test -count=1 ./internal/reviews ./internal/activity ./internal/environments ./internal/onboarding ./internal/pullrequests ./internal/runtime
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-review-browser
```

**Expected:** the Go tests create and drop isolated schemas. They cover:
- who may read and change a review;
- a whole conversation with edits, removal and moderation;
- version conflicts, instruction conflicts and closed pull requests;
- removed members, limits and the hourly allowance;
- filters, search and pages;
- cascade deletion with the pull request, and the realtime signal;
- every form without JavaScript, CSRF and forged IDs;
- publications recorded once per activation (a same-commit Rebuild included, a failed update excluded);
- activity deduplication and pruning;
- test access: sealed saves by Owners and Admins, reveals by members with preview
  access (JSON and no-JavaScript, `no-store`), repository and purpose binding, key
  rotation, saves waking live pages while reveals do not;
- setup facts of the serving deployment, when Reset test data is offered, Reset
  accepted only as a Rebuild and recorded with it, and the Build action returning
  to the review page.

The browser suite (`RUN_REVIEW_BROWSER_TESTS=1` for direct Go runs) covers:
- a Developer writing instructions in place;
- a Reviewer recording, replying, editing, removing and resolving a finding;
- a live reply and a new deployment arriving while a draft is unsent (the draft stays; the version is chosen on purpose);
- moderation with a reason from the pull request page;
- setup facts, the Reset test data confirmation opening in place, and test access
  added in place, revealed, and kept across a live refresh;
- the whole flow without JavaScript (a reveal included), with no CSP errors.

`UI_BROWSER_SCREENSHOTS=1` writes `review-*.png` captures (light, dark, mobile).

To verify persistent test data (the Test data tab, refreshes, kept data and
baselines) against real PostgreSQL:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
go test -count=1 ./internal/sampledata ./internal/buildlogs ./internal/runtime ./internal/pullrequests ./internal/reviews ./internal/reporting ./internal/starterkits
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-test-data-browser
```

**Expected:** the Go tests create and drop isolated schemas. They cover the first
baseline, coalesced and fenced refreshes, failures keeping the previous baseline,
capacity and quota waits, retries, spacing, takeover after a crash, turning Test
data off mid-build, settings permissions and conflicts, merges into the baseline
branch, unusable baselines, the daily check, cleanup, the tab's HTTP behaviour,
baseline logs, the PR page's *Test data* row, the Reset source and the review
page's line. The browser suite (`RUN_TEST_DATA_BROWSER_TESTS=1` for direct Go runs)
turns Test data on in a modal, follows a refresh live to a ready baseline, saves a
switch and the baseline branch in place (a bad branch is explained beside the
field), confirms Refresh baseline, opens a refresh's build log, checks the
Developer and Reviewer views and turns Test data off in a modal, with no CSP
errors. `UI_BROWSER_SCREENSHOTS=1` writes `test-data-*.png` captures at 1440 and
390 px in light and dark.

To verify the build-log viewer specifically:

```sh
UI_BROWSER_EXECUTABLE=/snap/bin/chromium UI_TEST_PATTERN='build logs|PR logs|recent log' make test-browser
set -a; . ./.env; set +a
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 -race ./internal/buildlogs ./internal/appoutput ./web/components/logviewer
```

**Expected:** the PR log link opens the last 300 events beneath its main card;
source/search filters, copyable inline dates, full-page navigation, live tail,
stream cleanup, pagination and the JavaScript-free fallback pass. Database
search tests use temporary schemas. No new configuration or migration is needed;
`make build` regenerates templates, SQL bindings and CSS. Restart/rebuild the
server (or let Air rebuild it) and refresh the browser to load the updated assets.

### Full regression

[test-docs/](../test-docs/README.md) lists every regression test case by module,
with the command that runs each level (static checks, `go test -race` against
PostgreSQL, the browser suite, the real Docker suites, end-to-end, release images
and the production smoke test) and the results of the latest cycle.

## 6. Run the application

Terminal 1, server (or `make dev` for live reload):

```sh
make run-server
```

**Expected:** `internal listener serving /livez, /readyz and /metrics`,
`database health check passed` (with `version`, `schema=31` and `environment`), then
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
checks; with builds configured ([22](22-mvp-operations.md#capability-and-configuration-matrix))
it also builds, publishes, reports and keeps house, follows live app output while
someone watches it, watches for crashed previews, reads preview visit counts from
the router every minute, and (with `PREVIEW_IDLE_SLEEP`) puts idle previews to
sleep. It always runs `build_log_prune` and `activity_prune` (activity older than 90
days or beyond 5,000 per pull request) at start and hourly; with the App it also runs
repository setup checks (`setup_checks` queue, two at a time), so the Setup tab's
**Check setup** needs only a worker with GitHub credentials. `Ctrl+C` removes the worker's capability row,
lets running jobs finish for 30 seconds, then cancels them; an interrupted build
runs again on the next start.

`make dev` runs Air: it compiles CSS, regenerates templates, rebuilds and restarts
the server when Go, templ, CSS, JS or SQL files change (polling works on
WSL-mounted folders). It does not restart the worker; restart that yourself after
worker changes. Refresh the browser after a rebuild.

Screen-by-screen behavior is in the [user guide](../technical-docs/user-guide.md). Signed
webhook check, once the secret is configured and the server is running:

```sh
make test-webhook   # expect: HTTP 204: signed fixture accepted
```

If development webhooks arrive through a Cloudflare quick tunnel, keep that
process running alongside the server. For example:

```sh
cloudflared tunnel --no-autoupdate --protocol http2 --url http://127.0.0.1:8080 --metrics 127.0.0.1:20241
curl --fail http://127.0.0.1:20241/ready
```

**Expected:** cloudflared registers a connection and `/ready` returns HTTP 200.
An expired quick tunnel can keep retrying with `Unauthorized: Tunnel not found`
while `/ready` returns 503. Stop that expired process and start a replacement;
the old hostname cannot be recovered by its connection retries.
After restarting the tunnel, set the existing GitHub App's **Webhook URL** to the
new printed HTTPS address followed by `/webhooks/github`, keeping its webhook
secret unchanged. Verify the external path as well:

```sh
WEBHOOK_TEST_URL=https://YOUR-TUNNEL.trycloudflare.com/webhooks/github make test-webhook
```

**Expected:** HTTP 204. Make loads the secret from `.env`; direct invocation of
`deploy/test-webhook.py` requires exporting it first. An old tunnel URL, HTTP 502,
or a failed `/ready` check means GitHub events cannot reach this server; changing
the dashboard's `APP_BASE_URL` is unnecessary for this webhook tunnel. Missed
merge events recover through the reconciliation described above.

## 7. Inspect jobs and operations

### In the browser: River UI

```sh
make river-ui
```

**Expected:** the first run compiles River UI (pinned in `tools/go.mod`, about a
minute), then prints `River UI: http://localhost:8090 (Ctrl+C stops it)` and
`Starting server addr=127.0.0.1:8090`. Open http://localhost:8090 to see every job
by state (available, scheduled, running, retryable, completed, discarded,
cancelled), filter by kind or queue, and open a job for its arguments, attempts and
errors. Ctrl+C stops it.

- It reads `DATABASE_URL` like the other targets: the exported variable first, then
  `.env`, then `.env.example`. It needs only PostgreSQL; the server and worker may be
  stopped.
- `RIVER_UI_PORT` (default `8090`, in `.env` or on the command line:
  `RIVER_UI_PORT=8091 make river-ui`) sets the port. It listens on `127.0.0.1` only,
  so it is reachable from this machine (including the Windows browser under WSL),
  not the network.
- River UI can also cancel, retry and delete jobs. Use it to look: cancelling or
  deleting a `build` or `environment_operation` job does not update Previa's own
  build and preview state; use the dashboard to build or tear down.
- `bind: address already in use` means something already serves that port (for
  example an earlier River UI container: `docker stop previa-riverui`); stop it or
  pick another `RIVER_UI_PORT`.

### In SQL

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
   followed by `Secrets: every value uses the active key; older key versions can be
   removed`, then the same two lines for `Test access:` (the review page's sealed
   test-login passwords, [31](31-test-data.md#test-access)). It prints counts only
   and is safe to repeat while the server runs.
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
- **Port 8080 in use:** stop the old `make dev`/Air or server terminal with Ctrl+C.
  To identify and gracefully stop the remaining listener on Linux/WSL:

  ```sh
  sudo lsof -nP -iTCP:8080 -sTCP:LISTEN
  sudo lsof -tiTCP:8080 -sTCP:LISTEN | xargs -r sudo kill -TERM
  ```

  **Expected:** the first command identifies the server; after it exits, it lists
  no listener. If a listener still remains, repeat the second command with
  `kill -KILL` as a last resort. Stop the supervisor first so it cannot restart it.
- **Password authentication failed:** Compose credentials live in the YAML, not
  `.env`; changing them does not change an already initialized volume.
- **Missing tables or skipped integration tests:** run `make migrate-up`, then export
  `TEST_DATABASE_URL`.
- **Stale configuration:** exported variables override `.env`; use a fresh terminal
  and restart the process after edits.
- **GitHub sign-in fails:** the OAuth callback must equal `APP_BASE_URL` +
  `/auth/github/callback`; use `localhost` consistently (not `127.0.0.1`). See [04](04-github-sign-in-and-app.md).
- **Tailwind download or checksum failure:** check curl, SHA-256 tool and GitHub
  release access; `curl: (22) … 502` after the retries means GitHub's release
  downloads were failing, so build again later. Never bypass a checksum mismatch. Edit `web/styles`, not the
  generated `web/static/css/app.css`.
- **Stylesheet missing after a manual build:** run `make assets` before compiling;
  the server embeds assets at build time.
- **Live indicator never turns green:** apply migrations 0011 and 0013, and make sure
  any proxy in front of the server does not buffer `text/event-stream` ([20](20-realtime-updates.md)).
- **Builder errors:** `egress proxy image … is not available` means run
  `make egress-image`. `builder did not become ready` usually means user
  namespaces are disabled or Docker is too old for rootless BuildKit. `disk_low`
  means less than `BUILD_MIN_FREE_DISK` is free on Docker's storage (plus
  `BUILD_DISK_BUDGET` for each build already running). `make test-builder` and
  `make test-e2e` lower the floor to 1 GiB for their tiny images, so they run on a
  machine or CI runner with about 10 GiB free; the worker keeps your values. Repeated pull
  failures for public images can be Docker Hub's anonymous rate limit ([09](09-image-builds.md)).
- **Removing a member shows "temporarily unavailable" and the server log says
  `function workspace_request_published(bigint) does not exist` (or River build jobs
  fail with `workspace_publication_authorized`):** the database applied an early
  draft of migration 0030 that lacked those two functions; Goose never re-runs an
  applied migration. Compare with a fresh schema, then recreate only the missing
  functions from `db/migrations/0030_workspaces.sql`:

  ```sh
  psql "$DATABASE_URL" -tAc "SELECT proname FROM pg_proc WHERE proname IN ('workspace_request_published','workspace_publication_authorized')"
  ```

  **Expected:** both names are listed. If either is missing, run the two
  `CREATE FUNCTION workspace_request_published…` and
  `CREATE FUNCTION workspace_publication_authorized…` statements from 0030 (as
  `CREATE OR REPLACE FUNCTION`) against that database, then retry the removal.
- **Editor says templ components are undefined:** run `make generate && go build ./...`;
  generated `*_templ.go` files are ignored and must exist locally. If the build
  passes, restart the Go/templ language servers. `make watch-templates` regenerates
  templates on save without starting the server.

### Fresh local setup after deleting Docker data

A new database can run all migrations from scratch, including workspace migrations
0030–0041. Deleting the PostgreSQL volume erases users/sessions, workspace memberships,
invitations, repository links, stored secrets, preview history, reviews, test access, Test data settings and usage.
Repository baselines are files in `PREVIEW_BASELINE_DIR` (by default under the
system temporary directory); with their records gone, the worker's cleanup removes
them within hours, or delete the directory. Remove
data only if you intend to discard it, or take a database backup first. Restrict
cleanup to Previa resources; global Docker cleanup can erase other projects.

Keep the configured `.env`, GitHub App private-key file, encryption keys and DNS
credential file on the host. Keep the router's ACME certificate volume if you
want to retain existing certificates; deleting it requires certificate issuance
again. Images and build caches can be recreated.

Stop Air/server and worker terminals with Ctrl+C, then stop the router with
`make router-down` (keeps its certificate volume). If resetting the database
through Compose, this explicitly deletes its local data volume:

```sh
docker compose -f deploy/docker-compose.yml down -v
```

Also remove the old Previa preview/build/service containers and their associated
resources when discarding the database. Compose's database shutdown does not
remove containers created dynamically by the worker. Docker Desktop can remove
those Previa resources without selecting other projects or the ACME volume.

If you removed the database, clear its old generated route table while the router
and workers are stopped. `ROUTER_DYNAMIC_DIR` is a host bind mount, so Docker
volume deletion does not clear it. Its old monotonic table version can prevent a
fresh database's lower versions from being published:

```sh
(
    . ./.env
    rm -f -- "${ROUTER_DYNAMIC_DIR:?Set ROUTER_DYNAMIC_DIR}/previa-routes.yml"
)
```

This subshell manually reads the trusted `.env` without printing its contents and
removes only Previa's generated routes file. Then rebuild and start prerequisites:

```sh
docker compose -f deploy/docker-compose.yml up -d --wait postgres
make migrate-up
make build
make egress-image
make router-up
```

**Expected:** PostgreSQL is healthy; Goose reaches 46 and River reaches 007;
the application builds, the egress image exists and Traefik starts with the fresh
route table. These Make targets automatically read `.env`. Start `make dev`
(or `make run-server`) and `make run-worker` in separate terminals. Sign in,
create a workspace, reconnect the existing GitHub App installation and choose
repositories. Re-enter any discarded repository secrets/settings and rebuild
previews. Resetting Docker/PostgreSQL does not remove the App installation or
existing comments/check runs on GitHub.

## F4 account exits, support and deletion

Read [34](34-accounts-support-and-deletion.md) for lifecycle/restore semantics.
Stop old Air/server and worker processes before upgrading; do not let old
workers keep executing against the new account fences:

```sh
make migrate-up
make build
make run-server
# Separate terminal:
make run-worker
```

**Expected:** Goose **0046**, River **007**, build succeeds. New sidebar links
reach Account & connections and Support & status; Your profile offers End other
sessions. This task does not migrate your development database automatically.
Make migration/run/dev commands load trusted `.env` assignments (exported
overrides win). Direct binaries/Go commands and browser targets require exported
settings. Quote env values containing spaces, e.g.
`OPERATOR_NAME="Previa operator"`.

Add these settings from `.env.example` as needed. No Go/template contact edit is
needed when the support address changes:

| Env key | Default / meaning |
| --- | --- |
| `SUPPORT_EMAIL` | Empty; displayed support contact. The owner's local `.env` is configured. |
| `OPERATOR_NAME` | `Previa operator`; public policy operator name. |
| `SUPPORT_OPERATOR_GITHUB_IDS` | Empty denies everyone; comma-separated numeric **user** IDs. Required in production while `EARLY_ACCESS_ENABLED=true`. |
| `SUPPORT_MESSAGE_MAX_BYTES` | 16384 UTF-8 bytes; 1024–65536 allowed. |
| `SUPPORT_REQUESTS_PER_DAY` | 5 per user per UTC day; 1–100 allowed. |
| `SUPPORT_RETENTION_DAYS` | 90 after resolution; open requests stay until resolved/deleted. |
| `BACKUP_RETENTION_DAYS` | 30; also used by policy text and host backup retention. |
| `DELETION_JOURNAL_RETENTION_DAYS` | 90; must exceed backup retention, never prunes unfinished entries. |
| `DELETION_JOURNAL_MAX_BYTES` | 1048576 plaintext bytes; overflow leaves deletion pending. |

Find an operator's numeric user ID on Your profile → GitHub account → GitHub ID,
or `https://api.github.com/users/YOUR_LOGIN` (`id`). Do not enter the login, App ID
or installation ID. Restart the server after changing operator IDs/contact; keep
server/worker retention settings consistent. The owner's operator setting stays
empty until explicitly configured; workspace ownership does not grant support
console access. Support submission persists a native request, without sending
an email.

For production, rerun `deploy/production/setup.sh` to install the minute
`previa-deletion-journal.timer`, configure the existing
`BACKUP_AGE_RECIPIENT`/`BACKUP_REMOTE` channel, then:

```sh
systemctl start previa-deletion-journal.service
systemctl status previa-deletion-journal.service --no-pager
systemctl list-timers previa-deletion-journal.timer
```

**Expected:** encrypted `deletion-journal/latest.json.age` stored independently
of database dumps; accepted lifecycle requests receive journal acknowledgement.
An unconfigured/stopped publisher deliberately leaves requests pending. A local
developer can test this workflow using a disposable production Compose smoke
stack and an absolute remote directory; do not point it at the development DB.
Production needs an off-host destination. Restore now requires the independent
latest journal; old pre-F4 dumps without its instance identity fail closed.

Verification against an explicitly exported disposable PostgreSQL URL:

```sh
make build
make lint
go test ./...
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-accounts-browser
TEST_E2E=1 go test ./internal/app -run '^TestRealTestDataJourneys$' -count=1 -v -timeout=60m
```

The browser test uses simulated GitHub authentication and isolated schemas, with
JavaScript on/off, CSP, dialogs, persistence and session revocation. The Docker
journey builds real previews/baselines and finishes with repository disconnection,
journal acknowledgement and physical erasure. Optional
`UI_BROWSER_SCREENSHOTS=1` writes ignored captures to
`web/tests/browser/artifacts`. Public status is unknown until an execution worker
reports actual Docker/router probes. A database-only worker can erase personal
data and empty workspaces; repository cleanup waits for an execution worker.


## F5/F6: installation recovery and workspace approval

Stop old server/worker processes, then run `make migrate-up` before restarting.
Expected schema: **45**, with River migrations current. Migration 0044 adds fair
installation reconciliation scheduling; 0045 preserves existing workspaces as
Approved and makes new workspaces Pending. This task does not migrate your
development database automatically.

| Setting | Default | Meaning |
| --- | --- | --- |
| `EARLY_ACCESS_ENABLED` | `true` | Both server and worker enforce approval of new workspaces. |
| `INSTALLATION_RECONCILE_MAX_CHANGES` | `5` | Max installation/repository revocations in one automatic pass, 1–100. The all-known circuit remains enabled. |

Both env examples include these settings. Make migration/run commands and Air
load trusted `.env` settings as documented above; direct `go`/binary commands
require manual exports. Keep server/worker settings consistent. Existing
`GITHUB_APP_ID`/private-key credentials enable reconciliation automatically, every
five minutes and at startup, even with builds paused. No installation token is
stored in PostgreSQL. `SUPPORT_OPERATOR_GITHUB_IDS` also authorizes approvals at
`/operator/early-access`; empty denies everyone. Use numeric GitHub user IDs from
the GitHub user API response's numeric `id` (`https://api.github.com/users/YOUR_LOGIN`), not logins, App IDs or installation IDs.

With early access on and no operator, nobody could approve a new workspace. A
development server still starts and logs this warning once:

```json
{"level":"WARN","msg":"early access is on but SUPPORT_OPERATOR_GITHUB_IDS is empty, so nobody can approve new workspaces; set your numeric GitHub user ID or EARLY_ACCESS_ENABLED=false"}
```

(abridged: the real line also carries the time and process fields).

With `APP_ENV=production` the server refuses to start, and `check-config` exits 1
with `SUPPORT_OPERATOR_GITHUB_IDS is required when APP_ENV=production and
EARLY_ACCESS_ENABLED=true`. Set the operator's ID or `EARLY_ACCESS_ENABLED=false`.
The worker approves nothing and does not check this.

After creating a workspace, expect `/early-access` with Awaiting approval. A
configured operator approves it in the console. Refresh status: Approved.
Connections/builds then work under their normal roles and free limits. Settings,
support, account exits and cleanup work while awaiting approval. Operator approval
does not start a build. Preexisting workspaces continue without a new approval.

Verification (explicitly export `TEST_DATABASE_URL` for a disposable database):

```sh
make build
make lint
go test -p=2 -timeout=30m ./...
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-accounts-browser
TEST_E2E=1 go test ./internal/app -run '^TestRealEndToEnd$' -count=1 -v -timeout=30m
make observability-check
```

The accounts browser suite now also verifies early-access approval with scripts
on/off and denial of the operator console to a customer Admin. Screenshot mode
uses `UI_BROWSER_SCREENSHOTS=1`. The ordinary real E2E journey now additionally
drops the uninstall webhook, detects the exact installation 404, verifies real
route/runtime cleanup, and relinks without rebuilding. See [35](35-installation-recovery-and-early-access.md)
for circuit-breaker recovery; an all-known revocation deliberately needs review.

## Google sign-in and email reviewer invitations (F7)

Stop old server/worker, run `make migrate-up`, then restart both. Expected Goose
**46**, River**007**. Migration0046 preserves existing identities/memberships and
adds optional Google identities/email Reviewer invitations. Do not roll it back
after those records exist: rollback deliberately refuses; forward-fix instead.

In Google Cloud configure an OAuth consent screen/audience, then create a **Web
application** OAuth client. Add the exact authorized redirect URI for this
environment: `${APP_BASE_URL}/auth/google/callback` (for example
`http://localhost:8080/auth/google/callback` locally). External apps in Google's
testing mode must include the intended Google test accounts. Use a separate
production client or register the exact production URI as appropriate.

Set both server settings in ignored `.env` or the production env file:

```dotenv
GOOGLE_OAUTH_CLIENT_ID=your-client-id
GOOGLE_OAUTH_CLIENT_SECRET=your-client-secret
```

Never commit real credentials. Both blank disables Google while preserving
GitHub. A partial pair fails configuration. Restart the server after changes.
Make/Air run commands load trusted `.env` automatically as described above; direct
Go/binary invocations need manual exports. Existing GitHub OAuth/App credentials
are still required for their existing features; workers need no Google settings.

Expected: the home/sign-in page offers Google; a new Google-only account reaches
Workspaces with a welcome explanation, without an administrative workspace.
Managers may invite a reviewer email; copy/share its link manually (no mail is
sent). Matching verified email, acceptance and explicit repository grants allow
review. Connect the second method from **Your profile** on the existing account;
matching emails do not merge accounts and already-owned identities cannot move.

Journal exports are now **v2**, including Google deletion fences. Restore accepts
priorv1 journals but must use an updated v2-aware binary for current exports.
Use the current application/migrations for restore, not an old release image.

Verification on a disposable database (manual exports for direct Go commands):

```sh
export TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB'
make build
make lint
go test -p=2 -timeout=30m ./...
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-google-browser
```

Optional `UI_BROWSER_SCREENSHOTS=1` captures login, welcome, profile, email
invitation/consent and reviewer pages at1440/390 in both themes.
[Feature36](36-google-sign-in-and-reviewer-invitations.md) documents security and
[manual rows M1–M9](../technical-docs/user-guide.md#manual-test-checklist) cover live
consent. Automated tests use a signed fake Google over TLS; they do not configure
or claim to exercise your real Google Cloud client.

## Preview Previa on Previa

The repository's own [`.previewrc.yaml`](../.previewrc.yaml) lets a Previa instance
preview pull requests to Previa. A preview runs the dashboard only: the
Dockerfile's last stage, `server`, on a managed PostgreSQL 17, with
`previa-migrate up` (Goose and River, safe to repeat) as its setup. The worker
needs the Docker socket, which previews never get, so a preview of Previa starts no
builds; pages, sign-in and settings can all be reviewed.

1. Create a GitHub OAuth App for previews (GitHub → Settings → Developer settings →
   OAuth Apps) with the callback `https://<PREVIEW_BASE_DOMAIN>/auth/github/callback`.
   GitHub accepts the preview hostnames below that domain. Keep it separate from
   the production sign-in app.
2. On the Previa repository's **Secrets** tab in Previa, put these lines in its
   `.env` file: `SESSION_SECRET=` 32+ random characters (for example
   `openssl rand -hex 32`), `GITHUB_OAUTH_CLIENT_ID=` and
   `GITHUB_OAUTH_CLIENT_SECRET=` from the previews OAuth App. A missing one stops
   the server at start, and the build log names it. Do not paste your local Previa
   `.env`: its `HTTP_ADDR`, `APP_BASE_URL` and `EARLY_ACCESS_ENABLED` would override
   the preview's own settings, and previews do not need its other credentials.
3. Build a pull request. Expected: setup prints `Goose: schema at migration 47` and
   `River: 7 of 7 migrations applied`, and the preview is published.
4. Opening the link now answers `This server does not serve that host.` (HTTP 421):
   Previa serves only `APP_BASE_URL`. In that pull request, uncomment
   `APP_BASE_URL=https://<your-preview-link>` in `.previewrc.yaml` with the link
   (it stays the same for the pull request), push, and build again. Expected: the
   home page loads and **Get started** signs in through the previews OAuth App.

The preview runs with `APP_ENV=development` (no webhook secret or operator needed)
and `EARLY_ACCESS_ENABLED=false`. There is no `health_check_path`, because the
readiness probe reaches the container by address and Previa answers other hosts
with 421; readiness is the port accepting connections.
`TestPreviasOwnPreviewConfigBuildsTheDashboard` (in `internal/app`) checks the file
against Previa's own rules and that the Dockerfile's last stage is the server
holding the setup command. Google sign-in on a preview would need each preview's
exact callback registered with Google, so it is left off.

## Secrets as one .env file (migration 47)

Each repository's secrets are now one `.env` file on its **Secrets** tab
([07](07-repository-secrets.md#the-env-file)). Stop the server and worker, then:

```sh
make migrate-up        # expected: Goose 47, River 007
make run-server        # and make run-worker in another terminal
```

Existing secrets need no conversion: a repository opens with one `NAME=value` line
per secret until its first save. Re-encryption (`make secrets-reencrypt`) now
also moves `.env` files onto the active key. Check it:

```sh
export TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB'
go test -count=1 ./internal/secrets
UI_TEST_PATTERN='Secrets tab is one|\.env file works without' make test-browser
```

Expected: `ok`, then `pass 2` and `fail 0` from the browser run.
