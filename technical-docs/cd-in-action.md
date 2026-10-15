# CD in action: how Previa becomes a production release

Previa currently has **automated release publishing and manual production
deployment**. GitHub Actions builds versioned binaries and Docker images.
An operator then installs the selected version on the production host, one
OVHcloud VPS.
There is no checked-in workflow that SSHes into that host, deploys on every
commit, or automatically rolls back a failed deployment.

This guide explains [release.yml](../.github/workflows/release.yml),
[.goreleaser.yaml](../.goreleaser.yaml), the [Dockerfile](../Dockerfile), and the
connection to the existing deployment scripts. It describes the checked-in
behavior reviewed on **2026-10-06**, not proof that a particular GitHub Release
or live server has been verified.

Read [CI in action](ci-in-action.md) for the checks before publishing.
Use [25 · Production deployment](../docs/25-production-deployment.md) for the full
host, DNS, secrets, monitoring, backup and recovery runbook, including the choice
of provider and low-cost settings. The commands below explain that existing process; they do not provision
anything by being documented.

## Contents

- [CI, delivery, deployment and live previews](#1-the-four-different-activities)
- [Machines, services and artifacts](#2-where-everything-lives)
- [Release workflow settings](#3-the-release-workflow-level-blocks)
- [The binaries job](#4-the-binaries-job)
- [GoReleaser configuration](#5-goreleaseryaml-block-by-block)
- [The images job](#6-the-images-job)
- [Dockerfile and build context](#7-the-dockerfile-and-dockerignore)
- [Publish a release, step by step](#8-how-to-publish-a-release)
- [How a release reaches the host](#9-how-a-release-reaches-the-host)
- [Backups and rollback](#10-backups-failures-and-rollback)
- [Secrets and configuration locations](#11-which-credentials-go-where)
- [Automatic deployment and troubleshooting](#12-automation-boundaries-and-troubleshooting)

## 1. The four different activities

| Activity | Meaning | Current implementation |
| --- | --- | --- |
| Continuous integration | Validate a proposed change | `ci.yml` runs generation, build, lint, vulnerability and test jobs |
| Release delivery | Produce and publish a named installable version | `release.yml` publishes binaries and container images from a version tag |
| Production deployment | Change the version running for users | Operator performs the production runbook and verification on the server |
| PR preview deployment | Build/run an app from a connected repository | The deployed Previa worker manages Docker previews on that server |

CD can mean continuous delivery or continuous deployment. This project's current
automation covers artifact delivery. It does not implement automatic production
deployment. A successful release job says the publication steps succeeded;
production remains at its previously selected version until the operator deploys.

```mermaid
flowchart TD
    Commit["Commit / PR"] --> CI["GitHub Actions CI"]
    CI --> Review["Operator confirms exact commit passed"]
    Review --> Tag["Push version tag"]
    Tag --> Binary["Release: binaries"]
    Binary --> Draft["Draft GitHub Release"]
    Binary --> Images["Release: server / worker / egress image jobs"]
    Images --> Registry["GHCR versioned images"]
    Draft --> Publish["Maintainer publishes release page"]
    Registry --> Deploy["Operator backs up and deploys"]
    Publish --> Deploy
    Deploy --> Host["OVHcloud VPS"]
```

The review-to-tag arrow is an **operator responsibility**, not an implemented
dependency between the CI and Release workflows. The separate `Release`
workflow has no `needs` relationship to CI, and does not rerun the full CI suite.

## 2. Where everything lives

| Resource | Location | Responsibility |
| --- | --- | --- |
| Release build jobs | GitHub-hosted `ubuntu-24.04` runners | Compile/package/publish, then finish |
| Release archives and checksums | GitHub Releases | Downloadable executables and selected deployment documentation/files |
| Platform Docker images | GitHub Container Registry, `ghcr.io` | Stores images for the host to pull |
| Production platform | One OVHcloud VPS (VPS-2), Ubuntu + Docker Compose | Runs server, worker, Traefik, PostgreSQL and optional monitoring |
| Production database | Docker `postgres` service, volume `previa_postgres` on the host | Application state, sessions, repository secrets and River jobs |
| Live app previews/builders | Docker containers created by the worker on the same host | PR runtime and temporary app builds |
| Encrypted backup archives | A private Cloudflare R2 bucket | Off-host DB/config/certificate recovery |
| Public DNS | Cloudflare authoritative zones | Points dashboard/wildcard names to the chosen host address |

GitHub's runner VMs are temporary build machines. You do not configure them
through OVHcloud. [Runner infrastructure](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).
The workflow has no provider container registry, hosting-platform integration or
OVHcloud API provisioning call. GHCR is image storage; it does not run the app.

The host pulls **Previa platform images** from GHCR. The worker builds **users'
PR images** locally on the host. Those PR images are not published by this
release workflow, and each PR does not create another server.

## 3. The Release workflow-level blocks

### 3.1 Header comments and name

```yaml
name: Release
```

This gives the workflow its Actions sidebar label. The file's comments describe
release publishing and an intended protected environment. Comments do not
configure GitHub settings or approval rules; the actual environment setup is
explained below.

### 3.2 Tag event

```yaml
on:
  push:
    tags: ["v*.*.*"]
```

A pushed tag matching this pattern requests the Release workflow. A local
`git tag` alone does not send an event to GitHub; it must be pushed.

| Example event | Starts this workflow? |
| --- | --- |
| Push `v0.2.0` | Yes |
| Push `v0.2.0-rc.1` | Matches the pattern; GoReleaser handles prerelease status |
| Push a commit to `master`, `main` or another branch | No |
| Open/update a PR | No |
| Publish the draft release page | No new run; there is no `release: published` trigger |
| Select a manual workflow dispatch | Not available in this YAML; no `workflow_dispatch` |

The pattern is a glob, not complete semantic-version validation. Use a new,
valid version and let packaging validate it. The `binaries` job refuses a tag
whose commit is not on `master` ([4.2](#42-checkout-with-history)); nothing checks
that the commit passed CI, so tag only a commit whose `master` run is green.

### 3.3 Default permissions and Go selection

```yaml
permissions:
  contents: read

env:
  GOTOOLCHAIN: local
```

Read-only repository contents access is the workflow default. Publication jobs
explicitly request their write permissions below.

`GOTOOLCHAIN=local` uses the job's installed Go toolchain. `setup-go` reads the
required version from `go.mod`, currently **1.26.9**. It is unrelated to the
application's version tag such as `v0.2.0`.

There is no workflow `concurrency` block. Multiple tags can request separate
release runs; nothing here serializes production deployment.

## 4. The binaries job

### 4.1 Runner, environment and publication permissions

```yaml
binaries:
  runs-on: ubuntu-24.04
  environment: release
  permissions:
    contents: write
```

`binaries` is the job ID used by the image job's `needs: binaries` dependency.
It uses a GitHub-hosted Ubuntu runner. No explicit `timeout-minutes` is declared
in this release job; GitHub's platform limits still apply.

`contents: write` authorizes the job token to create/update the GitHub Release
and its assets. It does not give the job access to the production database or
the production server.

`environment: release` associates the job with the GitHub environment named
`release`. Configure **Settings → Environments → release** with allowed tag rules
and required reviewers when your GitHub plan/repository supports them.
The YAML alone creates no reviewer approval requirement.
[Environment behavior and configuration](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).

A GitHub environment is deployment metadata/protection/secrets configuration;
it is not a running VPS or a replacement for `/etc/previa/previa.env`.

### 4.2 Checkout with history

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
    persist-credentials: false
```

Checkout fetches the tagged source. `fetch-depth: 0` also fetches history/tags
needed by release versioning/changelog tooling. The CI checkout is shallow by
default, whereas this binary release checkout explicitly requests history.
`persist-credentials: false` avoids leaving checkout credentials in Git config.
[Checkout inputs](https://github.com/actions/checkout/tree/v4).

```yaml
- name: Release only commits on master
  run: |
    git merge-base --is-ancestor "$GITHUB_SHA" origin/master || {
      echo "::error::$GITHUB_REF_NAME points to a commit that is not on master; tag a commit on master."
      exit 1
    }
```

The full history also lets this step check that the tagged commit is on `master`,
the release branch. A tag on any other commit fails here, before GoReleaser runs,
and the images job (which needs this job) never starts.

### 4.3 Install Go and the SBOM tool

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- uses: anchore/sbom-action/download-syft@v0
```

Go setup prepares the toolchain specified by the repository. The Syft download
Action supplies the component-inventory tool used by GoReleaser's SBOM phase.
An SBOM is a software bill of materials: a list of components/dependencies
associated with the published artifact.

### 4.4 Execute GoReleaser

```yaml
- uses: goreleaser/goreleaser-action@v6
  with:
    distribution: goreleaser
    version: "~> v2"
    args: release --clean
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

| Field | Effect |
| --- | --- |
| `distribution: goreleaser` | Selects the GoReleaser distribution |
| `version: "~> v2"` | Selects the configured v2 version range, not Previa's release number |
| `args: release --clean` | Runs the release pipeline and cleans its output directory first |
| Step `GITHUB_TOKEN` | Supplies the automatically generated job token for GitHub publication |

The command reads the root `.goreleaser.yaml`. Its `--clean` applies to generated
release output on the runner; it does not clean the server, database or R2 bucket.
The Action version `@v6`, GoReleaser v2, Go 1.26.9 and Previa's `v0.2.0` tag
identify four different things.

This workflow requires no manually created production GitHub App token or
OVHcloud or Cloudflare secret for publication. The token's permissions come from the job.
[GoReleaser Action](https://github.com/goreleaser/goreleaser-action),
[GitHub token](https://docs.github.com/en/actions/concepts/security/github_token).

## 5. .goreleaser.yaml block by block

### 5.1 Configuration format and project name

```yaml
version: 2
project_name: previa
```

`version: 2` is the GoReleaser configuration format. `project_name` supplies the
artifact naming prefix. Neither sets the application's runtime version; that
comes from the tag.

### 5.2 Before-build hooks

```yaml
before:
  hooks:
    - sh deploy/install-tailwind.sh bin/tailwindcss-v4.1.18
    - bin/tailwindcss-v4.1.18 -i web/styles/app.css -o web/static/css/app.css --minify
    - go tool -modfile=tools/go.mod templ generate -path web
```

The hooks install the pinned/checksummed Tailwind executable, generate/minify CSS,
and generate template Go code. The application embeds those presentation assets
in its binaries. There is no separately published frontend server.

These hooks do **not** run the full `make generate` SQL-generation/drift check
from CI. The release builds use committed SQL access code; verify CI before
tagging rather than assuming Release repeats every validation.

### 5.3 Shared Go build settings

```yaml
builds:
  - &build
    id: server
    main: ./cmd/server
    binary: previa-server
    env: [CGO_ENABLED=0]
    goos: [linux]
    goarch: [amd64, arm64]
    flags: [-trimpath]
    ldflags:
      - -s -w -X github.com/ajaykathwatee/previa/internal/platform/version.Version={{ .Tag }}
    mod_timestamp: "{{ .CommitTimestamp }}"
```

| Setting | Meaning |
| --- | --- |
| `&build` | YAML anchor naming these shared build settings |
| `id: server` | Identifies this build entry to GoReleaser |
| `main` | Go command package to compile |
| `binary` | Output executable name |
| `CGO_ENABLED=0` | Disables CGo for these builds |
| `goos: [linux]` | Produces Linux executables |
| `goarch` | Produces AMD64 and ARM64 variants |
| `-trimpath` | Removes source/build-machine path information from compiled output |
| `-s -w` | Removes symbol/debug information, reducing executable size |
| `-X ...Version={{ .Tag }}` | Sets Previa's reported version to the release tag |
| `mod_timestamp` | Uses the source commit timestamp for the binary file timestamp |

`{{ .Tag }}` and `{{ .CommitTimestamp }}` are **GoReleaser templates**.
They are different from GitHub's `${{ ... }}` expression syntax and a shell's
`$VARIABLE`. The historical Go module owner in the linker path contains
`ajaykathwatee`; do not casually replace it with the GitHub account spelling.
[GoReleaser Go builds](https://goreleaser.com/customization/builds/builders/go/).

### 5.4 Commands inheriting the shared settings

Each following entry starts with `<<: *build`. That YAML merge reuses the common
settings, then overrides its `id`, `main` and `binary`:

| ID | Go package | Executable / role |
| --- | --- | --- |
| `server` | `./cmd/server` | `previa-server`: dashboard, HTTP API, webhooks and live updates |
| `worker` | `./cmd/worker` | `previa-worker`: River jobs and preview orchestration |
| `migrate` | `./cmd/migrate` | `previa-migrate`: embedded Goose/River migrations |
| `route-sync` | `./cmd/route-sync` | `previa-route-sync`: synchronize accepted routing state |
| `housekeeping` | `./cmd/housekeeping` | `previa-housekeeping`: maintenance/cleanup command |
| `secrets-reencrypt` | `./cmd/secrets-reencrypt` | `previa-secrets-reencrypt`: repository-secret rotation |
| `egress` | `./cmd/egress` | `previa-egress`: outbound proxy and readiness probing |

Publishing all seven executables does not mean seven long-running host services.
The Compose deployment runs persistent server/worker processes, uses migrate
as a one-shot command, and invokes maintenance tools when appropriate.

### 5.5 Archives and included files

```yaml
archives:
  - formats: [tar.gz]
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    files:
      - README.md
      - docs/**/*
      - deploy/production/**/*
      - deploy/observability/**/*
```

This packages the Linux architecture-specific executables in compressed archives,
named from the project/version/OS/architecture. For example, a version can produce
`previa_0.2.0_linux_amd64.tar.gz` and its ARM64 counterpart.

The listed documentation, production scripts/Compose files and monitoring files
accompany the executables. A release archive is not the full Git source checkout
or root Dockerfile context. `technical-docs/**/*` is not among these extra-file
patterns, so these two guides are available in the repository rather than
automatically included as extra archive files.

### 5.6 Checksums and component inventories

```yaml
checksum:
  name_template: checksums.txt
  algorithm: sha256

sboms:
  - artifacts: archive
```

`checksums.txt` records SHA-256 checksums for release artifacts so downloads can
be compared with the expected files. The SBOM phase uses Syft to inventory the
archives. Checksums/inventories support verification; they are not deployment
health checks or an automatic guarantee of safe application behavior.

### 5.7 Changelog

```yaml
changelog:
  use: github
  sort: asc
```

GoReleaser uses GitHub-backed changelog generation with ascending order.
Review the generated notes and add the release's actual migration/configuration,
compatibility and recovery information. These fields do not run tests or decide
whether the release is production-ready.

### 5.8 Publication destination, draft and prerelease handling

```yaml
release:
  github:
    owner: '{{ envOrDefault "GITHUB_REPOSITORY_OWNER" "ajaykathwatee" }}'
    name: previa
  draft: true
  prerelease: auto
```

The owner is taken from the runner's GitHub repository-owner environment when
available, with the historical module owner as fallback. The repository name
is configured as `previa`; if the project is moved/renamed, review this setting.

`draft: true` leaves the GitHub Release awaiting a maintainer's publication.
`prerelease: auto` derives prerelease handling from the version.
[GoReleaser release settings](https://goreleaser.com/customization/publish/scm/).

A draft release does not stop the following image jobs. Once this job succeeds,
they can publish GHCR images even before you publish the release page.

## 6. The images job

### 6.1 Runner, environment, dependency and permissions

```yaml
images:
  runs-on: ubuntu-24.04
  environment: release
  needs: binaries
  permissions:
    contents: read
    packages: write
    id-token: write
    attestations: write
```

`needs: binaries` waits for binary publication to succeed. It does not transfer
the earlier workspace or wait for CI. Each image job checks out source again.

| Permission | Purpose |
| --- | --- |
| `contents: read` | Read repository source |
| `packages: write` | Publish images to GHCR |
| `id-token: write` | Obtain the OIDC identity used for build attestations |
| `attestations: write` | Publish GitHub artifact attestations |

These permissions concern GitHub publication. The OIDC permission is not an
OVHcloud credential or an SSH key. The same `release` environment applies here,
with whatever protection rules you configured; approval of the binaries phase
does not encode a bypass of all later environment checks.

### 6.2 Matrix: three image jobs

```yaml
strategy:
  matrix:
    target: [server, worker, egress]
```

GitHub expands this into jobs with `matrix.target` set to each listed target.
They can build independently after `binaries` succeeds. There is no declared
`fail-fast: false` or special partial-publication recovery.

A failure can leave some images published while others are missing or cancelled.
An archive plus one image is not a complete release. Verify all three targets
and both supported architectures before deployment.

### 6.3 Checkout, QEMU and Buildx

After checkout with `persist-credentials: false`:

```yaml
- uses: docker/setup-qemu-action@v3
- uses: docker/setup-buildx-action@v3
```

QEMU makes CPU emulation available for multi-platform builds; Buildx prepares
Docker's builder. The current Dockerfile runs its build stage on `BUILDPLATFORM`
and cross-compiles Go for `TARGETARCH`, so its Go compilation does not depend on
running target executables under QEMU. QEMU is nevertheless configured.
[QEMU Action](https://github.com/docker/setup-qemu-action).

### 6.4 Authenticate to GHCR

```yaml
- uses: docker/login-action@v3
  with:
    registry: ghcr.io
    username: ${{ github.actor }}
    password: ${{ secrets.GITHUB_TOKEN }}
```

The runner's Docker client logs into GitHub Container Registry using the run's
actor and the job token. The image namespace is the repository owner, not
necessarily that actor's username.

This does not log Docker into GHCR on your server. Private package pulls require
separate host authentication.

### 6.5 Build, push and attach image metadata

```yaml
- name: Build and push previa-${{ matrix.target }}
  id: push
  uses: docker/build-push-action@v6
  with:
    context: .
    target: ${{ matrix.target }}
    platforms: linux/amd64,linux/arm64
    build-args: VERSION=${{ github.ref_name }}
    tags: ghcr.io/${{ github.repository_owner }}/previa-${{ matrix.target }}:${{ github.ref_name }}
    provenance: mode=max
    sbom: true
    push: true
```

| Input | Effect |
| --- | --- |
| `name` | Shows the concrete matrix target in Actions |
| `id: push` | Names this step so later steps can reference its outputs |
| `context: .` | Uses the checked-out workspace as build context |
| `target` | Selects the server, worker or egress final Dockerfile stage |
| `platforms` | Publishes AMD64 and ARM64 variants |
| `build-args` | Supplies the tag as the binary's version |
| `tags` | Sets image owner, name and explicit release tag |
| `provenance: mode=max` | Requests detailed BuildKit provenance metadata |
| `sbom: true` | Requests image component inventories |
| `push: true` | Uploads the outputs to GHCR |

For tag `v0.2.0` under `ajaykathwate`:

```text
ghcr.io/ajaykathwate/previa-server:v0.2.0
ghcr.io/ajaykathwate/previa-worker:v0.2.0
ghcr.io/ajaykathwate/previa-egress:v0.2.0
```

There is no `latest` tag in this workflow. Use the actual owner and review
registry naming/access if the repository moves. Docker can select the matching
architecture from the published image index.
[Build/push Action inputs](https://github.com/docker/build-push-action).

CI uses `--load` with local `:ci` tags; Release uses `push: true` with versioned
GHCR tags. They are different build/publish paths.

### 6.6 GitHub build-provenance attestation

```yaml
- uses: actions/attest-build-provenance@v2
  with:
    subject-name: ghcr.io/${{ github.repository_owner }}/previa-${{ matrix.target }}
    subject-digest: ${{ steps.push.outputs.digest }}
    push-to-registry: true
```

The subject name identifies the image repository. The digest comes from the
preceding push step and identifies the exact published output. The final option
publishes the attestation through the registry workflow.

This records build origin/identity separately from BuildKit's provenance/SBOM
options. The binaries job has no equivalent GitHub attestation step for archives.
Consumers must verify/evaluate provenance for it to inform trust. It does not
prove vulnerability-free code, live health or database compatibility. The current
production commands do not enforce automatic attestation verification.
[Artifact attestations](https://docs.github.com/en/actions/concepts/security/artifact-attestations).

## 7. The Dockerfile and .dockerignore

### 7.1 Pinned image arguments

The Dockerfile declares `GO_IMAGE` and `RUNTIME_IMAGE` with digest-pinned
defaults: Go 1.26.9 on Debian Bookworm for compilation, and nonroot distroless
static Debian 12 for server/worker runtime. A digest identifies image content;
the descriptive version tag alone is not the same pin.

`ARG` supplies build configuration, not production secrets. The Tailwind
checksum and Go module checksums supply additional build-input checks.

### 7.2 Build stage and dependency layer

```dockerfile
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
COPY go.mod go.sum ./
COPY tools/go.mod tools/go.sum tools/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
```

1. Compile on the build machine's own platform.
2. Accept the target OS/architecture for cross-compilation.
3. Work in `/src` and disable CGo/toolchain switching.
4. Copy dependency manifests before source to allow reuse of dependency layers.
5. Download modules using a BuildKit cache mount.
6. Copy source after applying `.dockerignore`.
7. Read `VERSION`, overridden by CI/release.

These caches belong to the builder. They do not transfer its Docker data to the
production server.

### 7.3 Compilation heredoc

The next `RUN ... <<BUILD` block uses module/Go-build cache mounts, enables
`set -eu`, installs Tailwind, generates CSS/templates, creates output directories,
and loops over the seven Go command packages.

It sets `GOOS`/`GOARCH`, embeds `VERSION` through linker flags and writes
`/out/bin/previa-<command>`. `-buildvcs=false` avoids requiring VCS metadata inside
the build context, where `.git` is excluded.

`/out/state/sources` and `/out/state/router` are empty directories prepared for
the nonroot worker. They do not contain production state.

### 7.4 Server target

The server stage copies `previa-server`, `previa-migrate` and
`previa-secrets-reencrypt` into `/usr/local/bin` and runs as UID/GID 65532.

| Instruction | Role |
| --- | --- |
| `EXPOSE 8080 9090` | Documents listener ports; does not publish host ports |
| `ENV HTTP_ADDR ... INTERNAL_ADDR ... APP_ENV=production` | Sets default listener/environment values |
| `HEALTHCHECK ... previa-server healthcheck` | Runs application probes at the configured interval, timeout and start period |
| `ENTRYPOINT ... previa-server` | Starts the server unless a caller overrides the entrypoint |

Compose uses this image with an overridden command for the one-shot migration
service. Production secrets arrive through runtime configuration/mounts.

### 7.5 Worker target

The worker stage copies `previa-worker`, `previa-route-sync` and
`previa-housekeeping`. It copies the state directories with
`--chown=65532:65532` and runs as that nonroot user.

It sets the internal listener to `0.0.0.0:9091`, production environment, source
workspace and router directory. Its healthcheck calls `previa-worker healthcheck`;
its entrypoint starts the worker. `EXPOSE 9091` does not publish that port.

Compose separately supplies the Docker socket, networks, volumes and limits.
The image alone does not create those runtime dependencies.

### 7.6 Egress target

The egress stage uses `FROM scratch`, copies only `/previa-egress`, runs as
UID/GID 65532, declares port 3128, and sets the executable as entrypoint with
`serve` as its default command.

The worker creates the live `previa-egress` container through Docker.
Compose's `egress` service under profile `images` stages/pulls/builds the image;
do not enable that profile on normal `up` commands.

### 7.7 Build-context exclusions

[.dockerignore](../.dockerignore) excludes:

- VCS/editor metadata, local binaries, release output, temporary and coverage data.
- Generated templates and CSS, regenerated inside the build.
- Browser `node_modules` and artifacts.
- Local `.env` variants, with an exception allowing `.env.example`.
- PEM/key files and matching local DNS-credential files.

The runtime still needs protected host configuration and credentials.
An image is not a backup of the production database.

## 8. How to publish a release

### 8.1 Prepare GitHub once

1. Enable Actions and allow the referenced Actions under repository policy.
2. Keep workflows/supporting files committed on the default branch.
3. Configure branch rules requiring the intended CI checks.
4. Configure environment `release` with allowed tags and desired reviewers
   where your plan supports them.
5. Ensure jobs can use their declared publication permissions.
6. After publication, review GHCR visibility/repository access. Public source
   alone does not establish that packages are public.

No production SSH key, OAuth secret, App PEM, R2 token or OVHcloud credential
is consumed by these publication jobs. `GITHUB_TOKEN` is supplied automatically.

### 8.2 Select and tag the exact candidate

On your workstation with a reviewed clean checkout:

```sh
git switch master
git pull --ff-only origin master
git status
git rev-parse HEAD
```

Confirm the exact commit passed CI/staging. A PR run tested a temporary merge
SHA; inspect the merge's own `master` run. Review migration/rollback
compatibility and the smoke-test coverage in
[CI in action](ci-in-action.md#75-production-stack-smoke-test).

Then create/push a new unused version. Example only:

```sh
PREVIA_RELEASE=v0.2.0
git tag -a "$PREVIA_RELEASE" -m "Previa $PREVIA_RELEASE"
git push origin "$PREVIA_RELEASE"
```

Creating the tag labels the local commit. Pushing starts Release on GitHub,
without changing the production server.

### 8.3 Inspect publication and publish the draft

1. Open **Actions → Release** and verify tag/commit.
2. Approve environment jobs if configured protection requests it.
3. Inspect the binary job and all three image matrix jobs.
4. Verify archives, checksums, SBOMs and all image tags/platforms.
5. Open **Releases**, review the draft's notes/assets and publish it deliberately.
6. Record source commit, tag and image digests for deployment.

Do not move/reuse a published tag to quietly replace a version. Inspect partial
publication before retrying; select a new version if source/release contents
must change. Nothing here makes partial publication atomic.

## 9. How a release reaches the host

Nothing in this workflow deploys. An operator moves the host to a published
release by hand, following the production guide; this is the shape of it:

| Step | What happens | Production guide |
| --- | --- | --- |
| First deployment | Rent and prepare the host, register GitHub, install credentials, DNS, backups and staged certificates. An image pull alone creates none of this. | [Steps 0–10](../docs/25-production-deployment.md#fresh-host-setup) |
| Record and pause | Record the running tag, schema and image digests; set `BUILDS_PAUSED=true` and let builds drain. | [U1, U2](../docs/25-production-deployment.md#step-u1--prepare-and-record-the-previous-release) |
| Back up | Run the backup service and confirm all three encrypted archives arrived. | [U2](../docs/25-production-deployment.md#step-u2--pause-new-builds-drain-and-make-a-fresh-backup) |
| Select and validate | Check out the same tag, set `PREVIA_VERSION` and the GHCR image names, pull (log in first if the packages are private), run `check-config`. | [U3](../docs/25-production-deployment.md#step-u3--select-validate-and-start-the-candidate) |
| Migrate and start | Run the forward migrations, then `up -d --wait`. The worker replaces the egress proxy itself; rebuild live previews that call external services. Keep Traefik's preview networks if the router is recreated. | [U3, U4](../docs/25-production-deployment.md#step-u4--check-the-egress-proxy-after-the-upgrade) |
| Verify and resume | Check HTTPS, sign-in, webhooks, a disposable preview and PR feedback, then unpause and take a fresh backup. | [U5](../docs/25-production-deployment.md#step-u5--resume-only-after-live-checks-pass) |

The source checkout matters even when pulling images: it selects the matching
Compose file, scripts and monitoring configuration. The host keeps running after
SSH ends; an automatic restart never fetches a newer commit.

## 10. Backups, failures and rollback

A failed **release** changes nothing on the host, because no deploy step ran;
inspect the run and its partial artifacts. A failed **deployment** stays paused
while the operator picks the recovery that fits what failed:

| Failure | Recovery | Production guide |
| --- | --- | --- |
| Configuration or a new executable, with a compatible schema | Return to the recorded checkout, `PREVIA_VERSION` and images; no data changes. | [Application/image rollback](../docs/25-production-deployment.md#applicationimage-rollback-preserving-the-database) |
| Schema or data | Restore the pre-upgrade encrypted dump with the older release; writes after it are lost. | [Schema/data rollback](../docs/25-production-deployment.md#schemadata-rollback-using-the-pre-upgrade-backup) |
| The host itself | Prepare a replacement host, recover configuration, certificates and the database from the off-host backups and offline keys, then move DNS. | [Backups and restore](../docs/25-production-deployment.md#backups-and-restore) |

`previa-migrate` only migrates forward, and a release archive or image never
contains production rows: the nightly encrypted logical backup (02:30 UTC) is the
data recovery path, and provider snapshots are an optional extra.

## 11. Which credentials go where?

The release pipeline needs only what GitHub supplies:

| Credential | Where | Used by |
| --- | --- | --- |
| `GITHUB_TOKEN` | Supplied to each workflow job | The GitHub Release, GHCR login and attestations |
| `release` environment rules | Repository **Settings → Environments** | Approval and tag protection |
| GHCR pull credential (private packages only) | Root's Docker login on the host | Image pulls during a deployment; not the runner's token |

Production settings and secrets never enter GitHub Actions: they live only on the
host, in `/etc/previa/previa.env` and the credential files beside it. Where each
one comes from and its file mode are in the
[production credential checklist](../docs/25-production-deployment.md#credential-checklist-what-is-generated-supplied-and-kept-offline);
[configuration setup](config-setup.md) explains every setting.

## 12. Automation boundaries and troubleshooting

### 12.1 What happens automatically today

| Event/action | Result |
| --- | --- |
| Qualifying commit/PR | CI validates the change on GitHub runners |
| Push version tag | Release packages/publishes artifacts, subject to configured environment rules |
| Publish draft page | Makes the release page/assets available according to GitHub release behavior |
| Operator pulls + migrates + `dc up` | Changes production deliberately |
| Connected repository's preview request | Previa worker builds/runs Docker app previews according to policy |
| Host backup timer | Writes encrypted recovery archives to Cloudflare R2 |

There is no automatic production deployment on `main` or tags and no automatic
production rollback. Repository preview automation still follows Manual versus
Automatic policy; Automatic waits five quiet minutes before an eligible build.
It does not update Previa's own host binary.

### 12.2 What a future deployment workflow would need

This is **not implemented** by either YAML file or this guide. A future design
would need to:

1. Select a verified exact release and serialize deployments to this host.
2. Apply the intended approval gate and verify host identity.
3. Authenticate with a dedicated deployment account, SSH key and registry access.
4. Take/verify a fresh R2 backup and preserve previous artifacts.
5. Apply version/config, validate, migrate, replace and verify.
6. Preserve/reconcile egress and router network lifecycle.
7. Resume only after internal and external checks and report the result.
8. Distinguish compatible image rollback from destructive DB restore.

Those future deployment credentials would belong in a protected deployment
environment/secret store. They are not the current publication token.
An SSH firewall limited to your workstation will not automatically admit hosted
runner traffic; authorized connectivity must be designed. OVHcloud API
credentials are only needed if automation actually manages provider resources.

Do not install a self-hosted runner on the production server merely to follow
these workflows: the checked-in runners already use GitHub-hosted machines.
That would be a separate operational/privilege design.

### 12.3 Diagnose the failed stage

| Symptom | What to inspect |
| --- | --- |
| No Release run after a tag | Was the tag pushed, does it match, and are Actions/workflow permissions enabled? |
| Job waiting for approval | The actual `release` environment protection, reviewer and tag rules |
| GoReleaser fails | Tag/version, generated assets, compilation, repository destination and token permissions |
| One image missing | All matrix results and partial publication; do not deploy an incomplete set |
| GHCR push denied | `packages: write` and package/repository authorization |
| Host pull denied | Package visibility and the host's separate registry read credential |
| New release published but old server still runs | No deploy step has run, or running containers were not reconciled |
| Migration/startup fails on host | First migration/config/health error; pause and choose a tested recovery path |
| Router healthy but preview fails | Dynamic network attachments and accepted route/backend state |
| Egress still reports old image | The worker has not restarted since the upgrade, or the new egress image is missing on the host; check the worker log, pull it and recreate the worker ([U4](../docs/25-production-deployment.md#step-u4--check-the-egress-proxy-after-the-upgrade)) |
| Backup fails | Root's rclone config, bucket scope/endpoint, age recipient, dump/archive and retention logs |

### 12.4 Files to open when behavior changes

| File | What it controls |
| --- | --- |
| [release.yml](../.github/workflows/release.yml) | Triggers, publication jobs, matrix, registry/authentication and attestations |
| [.goreleaser.yaml](../.goreleaser.yaml) | Binaries, hooks, archive contents, checksums, SBOMs and draft release |
| [Dockerfile](../Dockerfile) / [.dockerignore](../.dockerignore) | Image build inputs, executable packaging and image defaults |
| [production Compose](../deploy/production/compose.yml) | Running platform topology, migration dependencies, limits, networks and mounts |
| [production env example](../deploy/production/previa.env.example) | Documented runtime/backup settings |
| [setup.sh](../deploy/production/setup.sh) | Existing host preparation and backup unit installation |
| [backup.sh](../deploy/production/backup.sh) / [restore.sh](../deploy/production/restore.sh) | Encrypted logical backup and destructive DB recovery |
| [smoke.sh](../deploy/production/smoke.sh) | Disposable stack rehearsal and coverage described by the CI guide |
| [production deployment](../docs/25-production-deployment.md) | The full deployment and operations procedure, for any provider |

The code/configuration and those runbooks take precedence when versions change.
Adding these guides does not change either workflow, runtime topology or
deployment authorization.
