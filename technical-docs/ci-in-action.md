# CI in action: how Previa checks a change

Previa's continuous integration runs on **GitHub Actions**, using temporary
GitHub-hosted Ubuntu runners. It checks the source, UI, database behavior,
container images and preview pipeline. It does not deploy Previa or use the
production OVHcloud VPS, database or R2 backup bucket.

This guide explains the checked-in [ci.yml](../.github/workflows/ci.yml) block by
block, including the Makefile targets and test harnesses it calls. It describes
configuration, not evidence that a particular GitHub run has passed. See
[CD in action](cd-in-action.md) for release publishing and manual deployment, and
[production deployment](../docs/25-production-deployment.md) for host operations.
Source behavior was reviewed on **2026-10-06**.

## Contents

- [Where the work runs](#1-where-the-work-runs)
- [How to read the YAML](#2-how-to-read-the-yaml)
- [Workflow name, triggers, permissions and shared settings](#3-the-workflow-level-blocks)
- [Checks: generation, build, lint, vulnerabilities and database tests](#4-the-checks-job)
- [Browser regressions](#5-the-browser-job)
- [Docker image builds and scans](#6-the-images-job)
- [Real Docker and production-stack tests](#7-the-real-docker-job)
- [Supporting files](#8-the-files-behind-the-workflow)
- [Running CI and reading failures](#9-how-to-use-the-pipeline)
- [What a passing run establishes](#10-what-a-passing-run-establishes)

## 1. Where the work runs

| Place | What runs there | Lifetime |
| --- | --- | --- |
| Your workstation | Git edits, commits, pushes and optional local checks | Your development environment |
| GitHub-hosted runner | One CI job's commands, temporary test databases and Docker containers | Temporary build/test environment |
| OVHcloud VPS | Deployed server, worker, production PostgreSQL, Traefik and live previews | Persistent production host |
| GHCR | Published release images from the separate Release workflow | Registry storage, not an app server |

Every CI job has `runs-on: ubuntu-24.04`. These are GitHub-hosted machines;
there is no `self-hosted` runner or OVHcloud API/SSH deployment step in this
workflow. GitHub provisions runners for the jobs. You do not create those
machines in your OVHcloud account. [GitHub-hosted runners](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).

The `checks` and `real-docker` jobs each get their **own** PostgreSQL service.
Their Docker daemons, workspaces and databases are separate. Passing `checks`
does not transfer its migrated database or built executables to `real-docker`;
that job checks out and prepares its own environment.

`checks`, `browser` and `images` can start independently, subject to runner
availability. `real-docker` waits for `checks` and its fork condition:

```mermaid
flowchart TD
    Event["Push to main, PR event or manual CI run"] --> Checks["checks: generate, build, lint and tests"]
    Event --> Browser["browser: UI regressions"]
    Event --> Images["images: build and scan"]
    Checks --> Gate{"Checks passed and event allowed?"}
    Gate -->|Yes| Docker["real-docker: runtime and stack suites"]
    Gate -->|No| Skip["Job skipped"]
```

These temporary runner machines do not change the live preview architecture:
Previa's worker creates live PR containers on the persistent OVHcloud VPS.
A connected repository's webhook/build request is a different pipeline from
the GitHub Actions workflow that tests Previa itself.

## 2. How to read the YAML

| Syntax | Meaning in these workflows |
| --- | --- |
| `name:` | Human-readable workflow, job or step label in GitHub |
| `on:` | Events that request a workflow run |
| `jobs:` | Named units of work; each hosted job uses its own runner |
| `steps:` | Ordered operations within that job |
| `uses: owner/action@version` | Execute an existing GitHub Action |
| `with:` | Inputs passed to that Action |
| `run:` | Execute shell commands on the runner |
| `run: \|` | A multiline shell script; newlines are preserved |
| `options: >-` | Fold multiple YAML lines into the Docker options string |
| `env:` | Environment variables at workflow, job or step scope |
| `services:` | Companion service containers for that job |
| `needs:` | Dependencies on other jobs in this workflow |
| `if:` | Condition deciding whether a job/step runs |
| `timeout-minutes:` | Maximum running time for that job |
| `${{ github.sha }}` | GitHub expression evaluated from the workflow context |
| `"$target"` | Shell expansion while a `run` script executes |

Indentation defines which block owns a setting. A step's `env` applies to that
step; a workflow's `env` supplies defaults throughout the workflow. Separate
`run` steps do not share one persistent interactive shell, although files
written in the same job's workspace remain available to later steps.

Steps normally run in order and a failure prevents later ordinary steps in that
job. Independent jobs can still finish. The YAML has no `continue-on-error`
override making these checks optional. For GitHub's general field definitions,
see [workflow syntax](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax).

## 3. The workflow-level blocks

### 3.1 Comments and workflow name

```yaml
name: CI
```

The opening comments describe the intended separation between ordinary checks
and the dedicated integration suites. Comments do not enforce behavior: the
actual `permissions`, job conditions and commands below do.

`CI` is the entry shown in the repository's **Actions** sidebar. The file lives
at `.github/workflows/ci.yml` so GitHub can discover it.

### 3.2 Events: when CI starts

```yaml
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
```

| Event | Behavior |
| --- | --- |
| Push directly to `main` | Runs CI on the pushed commit |
| Merge a PR into `main` | The resulting push runs CI again |
| Open, reopen or push new commits to a PR | Runs through the default `pull_request` activity types |
| Push to a feature branch without an open PR | Does not match this workflow's `push` filter |
| Push a version tag | Does not match this CI push filter; may trigger Release instead |
| Change only documentation on a qualifying push/PR | Runs CI; there is no path filter |
| Manually dispatch CI | Runs on the selected branch/ref through the Actions interface |

There is no PR base-branch filter, so the `pull_request` entry is not restricted
to PRs targeting `main`. For mergeable PRs, the default checkout tests GitHub's
temporary merge result with the base branch. The PR event's `github.sha` can
therefore be the merge SHA rather than the feature branch's head SHA.
[Event and checkout behavior](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows).

There is no scheduled nightly trigger, deployment trigger or
`pull_request_target` entry here.

### 3.3 Token permissions

```yaml
permissions:
  contents: read
```

The workflow's GitHub token can read repository contents. The CI YAML does not
grant package/release write access and does not pass production credentials into
its steps. This is a GitHub API permission boundary; it does not disable Docker,
network access or file writes on the runner.

The automatically supplied `GITHUB_TOKEN` is distinct from Previa's GitHub App
private key and OAuth secret. See [GitHub token behavior](https://docs.github.com/en/actions/concepts/security/github_token).

### 3.4 Cancel superseded runs

```yaml
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
```

Runs sharing a ref share a concurrency group. For example, repeated pushes to
`main` use a group based on `refs/heads/main`. A newer run can cancel an older
in-progress run for that group, avoiding checks on obsolete changes.

PR merge refs have their own groups. A PR run and a later `main` run are not
necessarily the same group. This setting does not serialize releases or
deployments: it belongs to this CI workflow.

### 3.5 Environment shared by jobs

```yaml
env:
  GOTOOLCHAIN: local
  DATABASE_URL: postgres://previa:previa@localhost:5432/previa?sslmode=disable
  TEST_DATABASE_URL: postgres://previa:previa@localhost:5432/previa?sslmode=disable
```

| Variable | Role |
| --- | --- |
| `GOTOOLCHAIN=local` | Use the Go toolchain installed by the job rather than automatically switching/downloading a newer one |
| `DATABASE_URL` | Database connection used by migration and application tooling |
| `TEST_DATABASE_URL` | Enables tests that need PostgreSQL and River integration |

`actions/setup-go` selects Go from `go.mod`, currently requiring Go **1.26.9**.
The URL uses deliberately disposable test credentials. `localhost:5432` works
because the jobs with PostgreSQL publish the service port onto their runner.

The browser/image jobs inherit these strings but do not declare a PostgreSQL
service. Inheritance does not create a database: the browser suite uses fixtures
without PostgreSQL, and image building does not need a live application DB.
These are not values to copy into production, whose private Compose hostname
is `postgres` and whose password is generated separately.

## 4. The checks job

### 4.1 Job identity, runner and deadline

```yaml
checks:
  name: Generate, build, lint, vulnerabilities, tests
  runs-on: ubuntu-24.04
  timeout-minutes: 30
```

`checks` is the identifier used by `needs: checks` later. Its longer `name` is
the UI label. All its steps share one Ubuntu runner and must finish within
30 minutes; this is a deadline for the job, not 30 minutes for every step.

### 4.2 Temporary PostgreSQL service

```yaml
services:
  postgres:
    image: postgres:17.11-alpine
    env:
      POSTGRES_USER: previa
      POSTGRES_PASSWORD: previa
      POSTGRES_DB: previa
    ports: ["5432:5432"]
    options: >-
      --health-cmd "pg_isready -U previa -d previa"
      --health-interval 5s --health-timeout 5s --health-retries 20
```

1. Start the specified PostgreSQL 17 container for this job.
2. Initialize the role/database named `previa` with the test password.
3. Publish container port 5432 as runner port 5432.
4. Probe database readiness using `pg_isready`, with the specified interval,
   probe timeout and retry count.
5. Run migrations and tests against this disposable database.

The connection settings above match this block. This is not the production
volume `previa_postgres` and is not a managed PostgreSQL service.
GitHub manages the service container lifecycle for the job.
[Service containers](https://docs.github.com/en/actions/tutorials/use-containerized-services/use-docker-service-containers).

### 4.3 Check out the source

```yaml
- uses: actions/checkout@v4
  with:
    persist-credentials: false
```

Checkout puts the event's source in the runner workspace so the Makefile and
Go tools can read it. The default checkout is shallow; CI does not request the
full Git history.

`persist-credentials: false` avoids retaining the checkout authentication token
in Git configuration for later shell commands. It does not mean every Action
has lost its ability to use the job's token. [Checkout inputs](https://github.com/actions/checkout/tree/v4).

### 4.4 Install the matching Go toolchain

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
```

The Action reads the Go version declaration in `go.mod` and prepares that
toolchain on the runner. The version is selected from the repository rather than
duplicated in this step. The tool dependencies are separately declared in
[tools/go.mod](../tools/go.mod).

### 4.5 Generate templates, then verify dependency manifests are tidy

```yaml
- name: Generate templates before module checks
  run: make templates
```

This runs immediately after Go setup. Template-generated Go files are ignored by
Git, so a fresh runner initially has some directories containing only `.templ`
sources. `make templates` uses the pinned templ tool to turn them into local Go
packages before `go mod tidy` resolves imports.

Without this step, tidy can try to download Previa's own packages as external
modules and report `git ls-remote` / GitHub authentication errors. Generate the
local packages first; these errors do not require a GitHub PAT or a module rename.

```sh
make templates
go mod tidy
(cd tools && go mod tidy)
git diff --exit-code -- go.mod go.sum tools/go.mod tools/go.sum
```

The two tidy commands check the application module and the tools module
respectively. The final command fails if those tracked manifests/checksum files change.
A green result means the checked-in dependency metadata did not need those
updates. CI does not commit the changes for you.

The subshell changes into the tools module before tidying and returns to the
application root for the diff. `-modfile=tools/go.mod` by itself does not change
the source root: using it for tidy from the application root makes Go interpret
application packages under the tools module's path and fetch Previa imports from
GitHub. This is separate from the missing-template problem. Tool execution with
`go tool -modfile=tools/go.mod` remains the existing supported Makefile command.

If this fails, inspect the diff, run the same tidy commands locally, review the
dependency changes and commit the appropriate manifests/checksums.

### 4.6 Generate code and check tracked SQL output

```sh
make generate
git diff --exit-code -- ':(glob)internal/**/dbgen/*.go'
```

`make generate` first generates Go from the templates under `web` using `templ`,
then generates SQL access code with `sqlc`. Its empty-query guard can skip sqlc
only when the SQL configuration has no queries/schema; this project has feature
queries.

The Git diff check covers **tracked Go files under `internal/**/dbgen/`**.
Its step name is broader than its actual diff filter: it does not compare all
generated templates or CSS, and Git diff alone does not detect newly generated
untracked files. Generation makes templates available for compilation; the
explicit drift check checks existing tracked database access output.

### 4.7 Build

```yaml
- name: Build
  run: make build
```

`make build` depends on `assets` and `generate`:

1. Install/use the pinned Tailwind executable and generate/minify CSS.
2. Generate templates and SQL code.
3. Run `go build ./...`.

This checks compilation of all Go packages. It is not deployment and does not
start a persistent production server. The Tailwind download is checked by
[deploy/install-tailwind.sh](../deploy/install-tailwind.sh).

### 4.8 Vet and lint

```yaml
- name: Vet and lint
  run: make lint
```

This also ensures assets/generated code exist, then runs `go vet ./...` and the
tools-module `golangci-lint run`. Vet checks suspicious Go constructs; the
linters configured in [.golangci.yml](../.golangci.yml) check correctness, a
cognitive-complexity ceiling of 30 per function (tests exempt), nesting depth
and selected style rules. A reported issue fails this step rather than being
automatically repaired.

### 4.9 Known Go vulnerabilities

```yaml
- name: Known vulnerabilities
  run: make vuln
```

`make vuln` invokes the tools-module `govulncheck ./...` against the code and Go
dependencies. This is different from scanning the built Docker image later.
It checks known vulnerability data and reachability; a pass is not proof that
the app contains no security defects.

### 4.10 Migrate the test database

```yaml
- name: Migrate PostgreSQL
  run: make migrate-up
```

The Makefile uses the exported `DATABASE_URL`, so it does not need a committed
`.env`. It runs Goose migrations from `db/migrations`, then invokes River's
migration adapter via `make river-migrate-up`.

Goose owns Previa's application schema; River owns its queue schema/history.
Both are prepared before integration tests. A failure here points to migration,
connection, PostgreSQL readiness or schema setup, not a migration on your production server.

### 4.11 Run Go tests, including database tests

```sh
go test -race -count=1 ./...
```

- `./...` selects all packages.
- `-race` instruments the executed tests to detect concurrent data access races.
- `-count=1` runs tests rather than reusing a cached successful test result.
- `TEST_DATABASE_URL` enables the tests that opt into database integration.

This does not enable every optional external/runtime suite. The `TEST_BUILDER`,
`TEST_RUNTIME` and similar switches are enabled in the dedicated Docker job.
The real GitHub proof commands are not run by this workflow.

### 4.12 Validate monitoring configuration

```yaml
- name: Alert rules, Prometheus configuration and dashboard queries
  run: make observability-check
```

The Makefile runs the pinned Prometheus image's `promtool` to check alert rules,
run their tests and check Prometheus configuration. A Python helper extracts
Grafana expressions into temporary rules so their PromQL syntax can be checked.

This validates configuration and rule tests. It does not send a production alert,
prove an Alertmanager receiver works, or establish that live scrape targets are
healthy.

## 5. The browser job

### 5.1 Independent Ubuntu job

```yaml
browser:
  name: Browser regressions
  runs-on: ubuntu-24.04
  timeout-minutes: 20
```

There is no `needs: checks`, so this job can run alongside Checks. It gets a
separate workspace and no PostgreSQL service.

### 5.2 Prepare source, Go and Node

Checkout and Go setup repeat the same blocks explained above. The additional
Action is:

```yaml
- uses: actions/setup-node@v4
  with:
    node-version: 20
```

Node 20 is used by Playwright test tooling. This does not make Node a runtime
dependency of the deployed Go dashboard.

### 5.3 Install browser tooling and OS dependencies

```sh
make browser-install
npm exec --prefix web/tests/browser -- playwright install-deps chromium
```

The Makefile runs `npm ci --prefix web/tests/browser` using its lockfile, then
installs Playwright's Chromium. The second workflow command installs the Linux
system libraries Chromium needs.

Tooling is isolated in `web/tests/browser`. A missing browser executable and a
missing OS library are different setup failures; inspect the installation step.

### 5.4 Exercise the UI

```yaml
- name: Browser tests
  run: make test-browser
```

The target prepares CSS/generated code and sets `RUN_BROWSER_TESTS=1` before
running `go test -count=1 -v ./web/tests/browser`. Without that opt-in, the suite
normally skips.

[browser_test.go](../web/tests/browser/browser_test.go) serves the UI through
an in-process test HTTP server with fixtures. It exercises real UI, session,
CSRF and Content Security Policy behavior without a live PostgreSQL database or
external GitHub calls. It does not open your OVHcloud Control Panel or sign in
to your real GitHub account.

## 6. The images job

### 6.1 Runner and Buildx

```yaml
images:
  name: Release images build and scan
  runs-on: ubuntu-24.04
  timeout-minutes: 30
```

After source checkout, `docker/setup-buildx-action@v3` prepares Docker Buildx.
Buildx builds the named targets from the root [Dockerfile](../Dockerfile).
This independent job has no registry login, `push: true` or production access.

### 6.2 Build all three platform images

```sh
for target in server worker egress; do
  docker buildx build --load --target "$target" \
    --build-arg VERSION=ci-${{ github.sha }} \
    -t "previa-$target:ci" .
done
```

| Part | Effect |
| --- | --- |
| `for target ...` | Repeat the build for server, worker and egress |
| `--target` | Select the final Dockerfile stage for that executable |
| `--load` | Load the image into this runner's local Docker image store |
| `VERSION=ci-...` | Embed the workflow commit identity into the Go binaries |
| `-t previa-...:ci` | Give the image a local tag for the scan steps |
| `.` | Use the checked-out repository as build context |

This command does not request the release workflow's AMD64/ARM64 matrix.
It builds locally for CI validation. The images are not uploaded to GHCR or
copied to the production server. The later Docker integration job builds its own images.

### 6.3 Scan the worker image

The image job pins the official scanner image:

```yaml
env:
  TRIVY_IMAGE: aquasec/trivy:0.65.0@sha256:a22415a38938a56c379387a8163fcb0ce38b10ace73e593475d3658d578b2436
```

```yaml
- name: Scan for fixable HIGH and CRITICAL vulnerabilities
  run: |
    mkdir -p "$RUNNER_TEMP/trivy-cache"
    docker run --rm \
      --mount "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock,readonly" \
      --mount "type=bind,source=$RUNNER_TEMP/trivy-cache,target=/root/.cache/trivy" \
      "$TRIVY_IMAGE" image --no-progress --image-src docker \
      --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 previa-worker:ci
```

Docker runs Trivy directly. This avoids the old Action's binary installer, whose
`trivy_0.65.0_Linux-64bit.tar.gz` release download returned HTTP 404 and failed
before any image scan. The digest fixes the scanner image content. No extra
GitHub token or production secret is needed.

The socket mount lets the scanner inspect the runner's local image store.
`--image-src docker` prevents fallback to a registry for the local tag. The cache
mount holds the downloaded vulnerability database under the runner's temporary
directory so the second scan can reuse it. `--rm` removes the scanner container
after each invocation, and `--no-progress` keeps download progress out of the log.

Trivy inspects the local worker image. The configured failure scope is known
HIGH/CRITICAL vulnerabilities with available fixes. `--ignore-unfixed`
excludes findings without fixes from this gate; it does not mean they do not
exist. `--exit-code 1` makes findings in scope fail the step. The scan uses the
current vulnerability database even though the scanner executable is pinned.

### 6.4 Scan the server image

The next block repeats the Docker invocation and scan settings for
`previa-server:ci`. It uses the same pinned image and temporary database cache.

The egress image is built but **has no separate Trivy scan step here**.
Do not interpret the job label as scanning all three images or all severities.

## 7. The real-docker job

### 7.1 Event condition, dependency and deadline

```yaml
real-docker:
  name: Real Docker, BuildKit, Traefik and end-to-end suites
  if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository
  needs: checks
  runs-on: ubuntu-24.04
  timeout-minutes: 45
```

The condition permits non-PR events, or a PR whose head repository is this
repository. It excludes fork PRs from **this job**. `needs: checks` also requires
Checks to succeed.

| Situation | Dedicated Docker integration job |
| --- | --- |
| Push to `main`, Checks pass | Runs |
| Manual CI run, Checks pass | Runs |
| Same-repository PR, Checks pass | Runs |
| Fork PR | Skipped |
| Checks fail or are cancelled | Does not proceed normally |

The source comment saying fork code never gets the runner's Docker daemon is
broader than the implemented condition. The `images` job still builds Docker
images for eligible fork PR runs, and Checks uses Docker for service/monitoring
validation. This condition skips the named integration job; it is not a global
Docker prohibition. Fork runs may also require approval under repository settings.

### 7.2 Its own PostgreSQL, checkout and Go setup

The PostgreSQL block repeats the same image, test credentials, port mapping and
health options as Checks, on a new runner. Checkout and Go setup repeat too.
No DB volume or built image is passed from the earlier job.

```yaml
- name: Prepare host for rootless BuildKit
  run: |
    if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
      sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
    fi
```

On Ubuntu 24.04, the host's AppArmor restriction can prevent RootlessKit from
creating its user namespace even with the builder container's configured
security options. This step applies BuildKit's documented prerequisite on the
disposable runner before the suites start. It skips hosts without that sysctl.
It is host configuration, not an application env variable; see
[BuildKit's rootless host requirements](https://github.com/moby/buildkit/blob/master/docs/rootless.md#ubuntu-2404-or-later).

Builder readiness now detects a stopped daemon immediately and includes its
exit status and up to 4 KiB of the last 20 daemon log lines before cleanup. A
readiness timeout also preserves the last probe error and startup logs.

```yaml
- name: Free runner disk
  run: |
    df -h /
    sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /usr/local/.ghcup /opt/hostedtoolcache/CodeQL
    df -h /
```

A fresh `ubuntu-24.04` runner leaves only about 10 GiB free on `/` once Go
modules and images are in place. This job builds images, runs previews with
PostgreSQL/Redis services, builds the three release images and starts the
production stack, so it deletes preinstalled SDKs it never uses. The two `df`
lines show the space before and after in the step log.

The real build suites also lower the build admission floor for their own tiny
images: `make test-builder` uses a 1 GiB free-disk floor and a 4 GiB per-build
budget, and `make test-e2e` a 1 GiB floor. With the production defaults
(`BUILD_MIN_FREE_DISK=15GiB`, `BUILD_DISK_BUDGET=20GiB`), a runner refuses builds
with `disk_low`, for example "with 1 other builds running, a build needs at
least 21.0 GiB". A worker always uses its configured values.

```yaml
- name: Generate application assets and code
  run: make assets generate
```

The job generates CSS, template Go packages and SQL access code on its own
runner before any integration suite loads application packages. A dependency on
`checks` controls scheduling; it does not copy that job's generated files.
Without this step, routing and end-to-end tests can fail to compile imports
whose directories contain only ignored template output on a developer's machine.

### 7.3 Egress image

```yaml
- name: Egress image
  run: make egress-image
```

The Makefile compiles the egress executable for this Docker host, builds its
small image through [deploy/egress/Dockerfile](../deploy/egress/Dockerfile), and
removes the temporary build directory. The default tag is `previa-egress:dev`.
The integration suites use this proxy/prober when testing isolated networks.

### 7.4 Integration steps, in their actual order

| Step | Command | What it exercises |
| --- | --- | --- |
| Image builds | `make test-builder` | Real BuildKit builds, isolation probes, resource limits, caching and cleanup |
| Hardened previews | `make test-runtime` | Container hardening, readiness, crashes, secret withholding, network denial, capacity and removal |
| HTTPS routing and teardown | `make test-routing` | Real Traefik, local Pebble ACME, previews, route swaps, revocation and teardown |
| Housekeeping | `make test-housekeeping` | Grace periods, dry run, orphan cleanup, ownership boundaries and controlled pruning |
| Whole pipeline | `make test-e2e` | Fake GitHub over TLS, real builds/previews/routing, PR reports, logs and teardown |

Each target sets its corresponding `TEST_*` opt-in variable and selects its
integration tests. The targets also depend on `egress-image`, so invoking them
can rebuild that prerequisite. They use runner Docker resources and throwaway
database state.

The local Pebble ACME server is a certificate-test service, not Let's Encrypt.
Fake GitHub is a test fixture, not a write to your production PRs. The e2e suite
checks the worker's real orchestration using those controlled external fixtures.

### 7.5 Production-stack smoke test

```sh
sudo apt-get install -y age
make release-images PREVIA_VERSION=ci
make prod-smoke PREVIA_VERSION=ci
```

1. Install `age`, used by the encrypted backup/restore drill.
2. Build the root Dockerfile's server, worker and egress images with tag `ci`.
3. Run [smoke.sh](../deploy/production/smoke.sh) against production Compose plus
   [compose.smoke.yml](../deploy/production/compose.smoke.yml).

The production scripts are tracked with executable mode `100755`; Make and the
drill invoke them directly, so a clean runner needs those permissions. On WSL,
local permissions can hide a missing Git executable bit. A `Permission denied`
error before a script starts calls for checking its tracked file mode.

The harness uses project `previa-smoke`, temporary configuration/storage and
loopback ports `18080`/`18443` for HTTP/HTTPS and `13000` for Grafana. The GitHub
App is disabled, and ACME is deliberately unreachable. It checks startup,
migrations, readiness, security headers, metrics, hardening, graceful stops,
tag-based replacement/rollback and an encrypted backup/restore drill.

Its backup destination is a temporary local directory, not Cloudflare R2.
The default cleanup removes the test stack/volumes and temporary files.
`SMOKE_KEEP=1` can retain them for a local investigation; that variable is not
set in CI.

The upgrade rehearsal retags the **same built images** with a next tag. It tests
the replacement procedure, not compatibility between two different releases.
The optional older-schema restore needs `SMOKE_OLD_DUMP`, which CI does not supply.

The initial migration, upgrade and optional older-backup restore checks assert
Goose schema **20**, matching
[schema.go](../internal/platform/database/schema.go). Update these three
expectations when a release adds a migration, and rerun the smoke test against
that release's images. The earlier release report's smoke result is historical;
it does not establish that today's image passed.

## 8. The files behind the workflow

| File | Role in CI |
| --- | --- |
| [ci.yml](../.github/workflows/ci.yml) | Events, runners, job graph, service containers and steps |
| [Makefile](../Makefile) | Commands actually executed by the named `make` targets |
| [go.mod](../go.mod) and [tools/go.mod](../tools/go.mod) | Application/tool dependencies and Go version |
| [sqlc.yaml](../sqlc.yaml) | SQL access generation configuration |
| [Dockerfile](../Dockerfile) | Shared build stage and platform runtime image targets |
| [.dockerignore](../.dockerignore) | Excludes local state, credentials and generated templates from image context |
| [browser tests](../web/tests/browser/browser_test.go) | Fixture-backed Go HTTP server driving UI regressions |
| [browser package manifest](../web/tests/browser/package.json) | Isolated Playwright tooling |
| [alerts.yml](../deploy/observability/alerts.yml) and [rule tests](../deploy/observability/alerts_test.yml) | Application alert expressions and expected evaluation behavior |
| [dashboard-rules.py](../deploy/observability/dashboard-rules.py) | Extracts Grafana expressions for syntax validation |
| [smoke.sh](../deploy/production/smoke.sh) | Disposable production-stack rehearsal |

An Action version such as `@v4` is the Action's version reference, not your
application version. The workflow uses these checked-in versions; this guide
does not upgrade Actions or change configuration.

## 9. How to use the pipeline

### 9.1 First GitHub setup

1. Ensure the workflows and their supporting files are committed and pushed.
2. Check **Repository → Settings → Actions → General** permits Actions and the
   referenced Actions under your organization/repository policy.
3. Open a PR or push `main`, then open **Actions → CI**.
4. Confirm the event, branch/ref and commit match the change you wanted tested.
5. Inspect all jobs, including skipped ones and fork restrictions.
6. Configure branch protection/rulesets with the checks you require for merging.
   Workflow YAML alone does not make green CI mandatory for a merge.

No OVHcloud API credential, production SSH key, production GitHub App PEM,
Cloudflare DNS token or R2 token is required by this CI workflow. Its temporary
database values are already declared in YAML.

### 9.2 Manual run and logs

1. Open **Actions → CI → Run workflow**.
2. Select the intended branch/ref and start the run. The workflow must be
   available on the default branch for manual dispatch discovery.
3. Open the run, select a job, then expand its steps.
4. Start with the first failed step and read its command/output.
5. After a fix, push a new commit. Re-run a failed job when retrying the same
   commit is appropriate, such as a transient download failure.

Check the run's commit SHA against `git rev-parse HEAD` and review `git status`
before retrying. Uncommitted workflow or application changes are local only.
GitHub's **Re-run jobs** uses the original commit SHA and ref, so it does not
pick up fixes from a later commit; see
[re-running workflows and jobs](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).
After committing and pushing a fix, open the new run and confirm its SHA matches
the pushed commit. For the fixes described here, the Docker job should include
**Prepare host for rootless BuildKit**, and the image scans should invoke
`docker run` with `TRIVY_IMAGE` instead of installing a Trivy binary.

The run graph shows queued, running, passed, failed, skipped or cancelled jobs.
A cancelled older run can result from the concurrency setting. A fork-skipped
Docker suite is not evidence that suite passed.

There is no explicit `upload-artifact` step in CI for browser screenshots,
test reports or the local images. Logs remain available through Actions; do not
assume every temporary file is retained as a downloadable artifact.

### 9.3 Local commands and their boundaries

On a prepared **development/staging machine**, following
[development setup](../docs/02-development-setup.md), the corresponding checks are:

```sh
make build
make lint
make vuln
make migrate-up
go test -race -count=1 ./...
make observability-check
```

Configure `DATABASE_URL` and `TEST_DATABASE_URL` for a disposable local DB first.
`make migrate-up` writes to the database named by that URL. Use development/test
credentials; do not point these commands at production to reproduce CI.

Browser reproduction requires `make browser-install` and `make test-browser`;
Linux may also need the workflow's Playwright OS-dependency command. Real Docker
suites require Docker, available image downloads and disposable local state.
Local env loading varies by Makefile target; see the setup guide rather than
assuming every Go command automatically loads `.env`.

### 9.4 Diagnose by the failed step

| Failure | First place to investigate |
| --- | --- |
| Generate templates before module checks | Pinned templ tool installation and the first template-generation error |
| Modules are tidy | Template generation must have succeeded; then inspect manifest/checksum changes |
| Generated code is current | Tracked SQL output changes and the matching SQL queries/schema |
| Build | The first compiler error and whether required templates were generated |
| Vet/lint | Reported file/rule; warnings promoted to failures by the configured tool |
| Known vulnerabilities | Affected dependency/reachable symbol and available update |
| Migrate PostgreSQL | Service health, connection settings and first failing migration |
| Race/tests | Test output, detected race and integration prerequisites |
| Browser | Browser installation, OS dependencies, then failing UI scenario |
| Image build | Dockerfile stage, dependency download and compile output |
| Image scan | Scanner-image pull or vulnerability-DB download errors; then image name, CVE, severity and fixed version |
| Real Docker | Builder exit status/startup logs, rootless host prerequisite, then suite-specific Docker/network/resource errors |
| Production smoke | First `FAIL` line, captured stack logs and schema expectations for the tested release |

## 10. What a passing run establishes

A passing run means the checks configured for that event completed successfully
against their runner environments and fixtures. It does not establish:

- A release was published, or that Release is automatically gated on this CI.
- The OVHcloud VPS was updated, backed up or rolled back.
- Real GitHub OAuth/App registrations, DNS and production certificates work.
- An R2 backup can be decrypted/restored on a replacement server.
- Skipped fork-only or opt-in suites passed.
- Every image/severity was scanned, or every UI/production path has a test.

The documented live proofs remain separate in the
[release report](../docs/26-release-report.md) and
[deployment runbook](../docs/25-production-deployment.md). Continue with
[CD in action](cd-in-action.md) to follow the exact boundary between checking a
commit, publishing its artifacts and installing them on the production host.
