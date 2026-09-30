# Understand the Previa codebase

This is a guide to the files that exist after Phase 2. It explains their current
contents and separates working code from folders reserved for future features.
Requirements live in [README.md](README.md); the architecture and feature layout
live in [DESIGN.md](DESIGN.md). This guide does not change either document.

## What the project is for

Previa is intended to create temporary, publicly accessible preview environments
for GitHub pull requests. The planned flow is: receive a GitHub event, fetch the
code, build an image, run an isolated container, give reviewers a URL, and remove
the preview when it is no longer needed.

**Today, the foundation works, but that preview workflow is not implemented.**
There is local PostgreSQL infrastructure, an initial schema, validated Go
configuration, a connection pool, tests, and pinned development tools. There is
no running HTTP application, worker, dashboard, GitHub integration, or preview
container management yet.

## How the folders fit together

```text
Previa/
├── cmd/                 Executable entry points (currently empty)
├── internal/            Application packages and shared infrastructure
│   ├── app/             Future dependency construction and wiring
│   ├── <feature>/       Future feature code, grouped by responsibility
│   └── platform/       Shared infrastructure; config and database work today
├── db/migrations/      Versioned SQL changes to the database structure
├── deploy/             Local services and future deployment configuration
├── web/static/         Future browser assets
├── tools/              Separate Go module for development tools
├── docs/               Setup guide and verification notes
└── .github/workflows/  Future automated build/test/release workflows
```

The planned dependency direction is `cmd → app → features → platform`. A feature
keeps its own handlers, business rules, queries, and templates together. Shared
infrastructure must not depend on a feature. Those future files have not been
created merely because their folders exist.

## Files at the repository root

| File | Purpose and current contents |
| --- | --- |
| [README.md](README.md) | Product requirements and technology choices. Describes the intended system, not a list of completed features. |
| [DESIGN.md](DESIGN.md) | Detailed feature designs. Section 1 defines the data foundation; Section 20 defines the feature-based folder layout used here. Its layout takes precedence over the older README layout. |
| [UNDERSTAND_CODEBASE.md](UNDERSTAND_CODEBASE.md) | This navigation and explanation guide. |
| [AGENTS.md](AGENTS.md) | Repository instructions for coding agents: scope, architecture, Go conventions, and required checks. Not runtime application configuration. |
| [go.mod](go.mod) | Application module name (`github.com/ajaykathwatee/previa`), Go version (`1.26.5`), and dependency versions. Direct dependencies are pgx v5.10.0 and envconfig v1.4.0. |
| [go.sum](go.sum) | Checksums used to verify downloaded application dependencies. Go maintains it; it contains no application logic. |
| [Makefile](Makefile) | Short commands for generation, builds, tests, linting, entry points, and migrations. Uses the tools module to run pinned tools. |
| [.env.example](.env.example) | Sample environment values grouped into shared, server-only, and worker-only settings. Its database URL matches local Compose credentials. Secrets are placeholders. |
| [.gitignore](.gitignore) | Keeps local environment files, private keys, build outputs, coverage files, generated templ files, and editor files out of normal Git additions. Keeps `.env.example` visible. |
| [.air.toml](.air.toml) | Air live-reload settings: watch source files, run generation, build `tmp/server`, and restart it. The server currently exits immediately because its entry point is empty. |
| [.golangci.yml](.golangci.yml) | Minimal configuration selecting golangci-lint's version-2 configuration format; no custom lint rules are specified. |
| [sqlc.yaml](sqlc.yaml) | Future SQL-to-Go generation configuration. Currently `sql: []`, because no feature queries exist. Comments explain where each feature's queries and generated package will go. |

`.env` is an optional local file you create from `.env.example`; it is not a
committed source file. The Go config loader does not automatically read it.

## Setup and infrastructure: what runs where

| Component | Its role in this project |
| --- | --- |
| Docker Desktop | Runs the Docker engine used by this WSL development environment. It is installed outside this repository. |
| Docker CLI | The `docker` command in WSL communicates with that engine. |
| Docker Compose | Reads the deployment YAML and creates the local PostgreSQL service, network, and storage volume. |
| PostgreSQL | Stores the project's data. The current Compose service runs only this database. |
| goose | Applies or rolls back versioned SQL migrations. It creates the schema inside PostgreSQL. |
| pgx | The Go driver and connection pool used to connect to PostgreSQL. It does not apply migrations here. |
| sqlc | Will generate Go functions from feature SQL queries. It is installed but has no queries to process yet. |

Starting a database container and creating application tables are separate steps:

```text
docker compose ... up -d --wait → healthy PostgreSQL process
make migrate-up                → seven application tables inside PostgreSQL
database.NewPool(...)          → Go connections to that database
```

The pool is exercised by integration tests; the empty server and worker do not
call it yet. No single command currently starts a complete Previa application.

### Deployment files

| Path | Purpose and current contents |
| --- | --- |
| [deploy/docker-compose.yml](deploy/docker-compose.yml) | Defines the `previa` project and one `postgres:17-alpine` service. Uses local user/database `previa`, password `change-me`, and port `127.0.0.1:5432`. Adds a `pg_isready` health check and persistent `postgres_data` volume. |
| [deploy/traefik/.gitkeep](deploy/traefik/.gitkeep) | Empty placeholder for future routing/TLS configuration. Traefik is not configured or running through this Compose file. |

Compose creates `previa-postgres-1` and the `previa_postgres_data` Docker volume
with the current project settings. Actual database files live in Docker-managed
storage, not in `db/migrations/`. Database credentials in Compose are literal
local defaults; changing `.env` alone does not change them.

There is no application Dockerfile, deployment script, or server/worker service
in Compose yet.

### About the Docker permission commands

The earlier permission error came from an existing terminal session that had
not picked up your already-configured `docker` group membership.

```sh
newgrp docker
docker version
```

`newgrp docker` opens a shell with that group active. `docker version` checks
client and server access; it does not start Previa or change its database. If
Docker already works in a terminal, you do not need to run `newgrp` there.
This was not a blocker to completing Phase 2 verification. An affected terminal
does need its group membership refreshed before it can control Docker.

## Database migrations

| File | Purpose and current contents |
| --- | --- |
| [db/migrations/0001_init.sql](db/migrations/0001_init.sql) | Initial goose migration. `-- +goose Up` creates the seven tables, relationships, indexes, and constraints. `-- +goose Down` drops them in reverse dependency order. |
| [db/migrations/.gitkeep](db/migrations/.gitkeep) | Empty file left from the initial skeleton. It has no effect on migrations. |

The schema contains:

| Table | What it stores | Important rule |
| --- | --- | --- |
| `users` | GitHub user ID, username, creation time. | GitHub ID is unique. |
| `installations` | GitHub App installation ID and its owning local user. | External installation ID is unique; owner must exist. |
| `repos` | Installation, repository full name, optional default-config hash. | Full name is unique; installation must exist. |
| `pull_requests` | Repository, PR number, head SHA, state, optional GitHub comment ID. | `(repo_id, number)` is unique; state is `open`, `closed`, or `merged`. |
| `environments` | PR, container/image identifiers, subdomain, status, creation/activity/expiry times. | Globally unique subdomain and at most one non-`torn_down` environment per PR. |
| `build_logs` | Environment, sequence number, log line, creation time. | A sequence number is unique within an environment. |
| `env_secrets` | Repository, secret key name, encrypted bytes, creation time. | A key name is unique within a repository. Encryption logic is not implemented yet. |

All seven tables have generated numeric primary keys. Foreign keys enforce
relationships between rows. Environment expiry must be later than creation;
an index supports looking up active environments by expiry.

Environment status values are `queued`, `building`, `running`, `failed`,
`tearing_down`, and `torn_down`. The existing migration counts all except
`torn_down` as active, including failed environments that may retain resources.
The database checks allowed values today; guarded state-transition updates are
future application work.

goose also creates its own `goose_db_version` bookkeeping table. Repeating
`make migrate-up` at the latest version makes no schema changes.
**`make migrate-down` drops the seven application tables and their data.**

## Executable entry points: `cmd/`

| File | Purpose and current contents |
| --- | --- |
| [cmd/server/main.go](cmd/server/main.go) | Empty `main()`; future entry point for HTTP, OAuth, webhooks, and the dashboard. No listening server today. |
| [cmd/server/doc.go](cmd/server/doc.go) | Short comment describing that command's intended responsibility. |
| [cmd/worker/main.go](cmd/worker/main.go) | Empty `main()`; future entry point for background jobs. No job processing today. |
| [cmd/worker/doc.go](cmd/worker/doc.go) | Short comment describing the worker's intended responsibility. |

## Working shared packages: `internal/platform/`

`internal/` contains Go packages intended for this project's own code. `platform/`
groups infrastructure that multiple features will use.

| File | Purpose and current contents |
| --- | --- |
| [config/doc.go](internal/platform/config/doc.go) | Package description. |
| [config/config.go](internal/platform/config/config.go) | `SharedConfig`, `ServerConfig`, and `WorkerConfig` with separate loaders. Validates database URL, HTTP origin, OAuth values, session-secret length, and worker concurrency. Unbuilt integrations have no configuration fields. |
| [config/config_test.go](internal/platform/config/config_test.go) | Tests valid loading, missing values, invalid values, and avoiding database-password exposure in validation errors. |
| [database/doc.go](internal/platform/database/doc.go) | Package description. |
| [database/database.go](internal/platform/database/database.go) | `NewPool(ctx, cfg)` parses the database URL, creates a pgx pool, and checks connectivity. Closes the pool if startup fails. `HealthCheck` pings with a maximum five-second wait. Callers must close successful pools. |
| [database/database_test.go](internal/platform/database/database_test.go) | Tests invalid config, cancellation, nil pools, and live connection/health behavior. Live tests require `TEST_DATABASE_URL`. |
| [database/migrations_test.go](internal/platform/database/migrations_test.go) | Live tests of the migrated schema: uniqueness, foreign keys, expiry, statuses, and the active-environment rule. Test fixture rows are rolled back. Requires `TEST_DATABASE_URL`. |

`config.LoadServer()` and `config.LoadWorker()` read exported variables; they do
not read `.env`. Both use `DATABASE_URL`; only the server needs OAuth credentials
and a session secret of at least 32 characters. Only the worker needs queue
concurrency. The variable explanations are in [.env.example](.env.example).

### Shared packages reserved for later

Each folder below contains **only `doc.go`**, a comment and package declaration:

| Folder | Future responsibility |
| --- | --- |
| `internal/platform/dockerx/` | Construct Docker clients. |
| `internal/platform/githubapp/` | GitHub App clients and installation-token handling. |
| `internal/platform/httpx/` | Shared HTTP middleware, errors, and HTMX helpers. |
| `internal/platform/logging/` | Structured logging and secret redaction. |
| `internal/platform/metrics/` | Shared Prometheus metrics. |
| `internal/platform/queue/` | River background-job infrastructure. |
| `internal/platform/ui/` | Shared page layouts, navigation, and template components. |

## Feature packages: `internal/`

Every folder below currently contains **only its own `doc.go`**, plus the
`dbgen/doc.go` placeholders listed afterward. No feature implementation exists.

| Folder | What its `doc.go` says the package will own |
| --- | --- |
| `internal/app/` | Construct dependencies and connect server/worker components. This is the future composition root, not a user-facing feature. |
| `internal/auth/` | GitHub sign-in and dashboard sessions. |
| `internal/installations/` | GitHub App access to repositories selected by their owners. |
| `internal/webhooks/` | Authenticate GitHub events and dispatch background work. |
| `internal/environments/` | Preview lifecycle: build, update, and teardown. |
| `internal/repoconfig/` | Repository build/run configuration. Distinct from platform config, which handles Previa's own environment variables. |
| `internal/source/` | Fetch repository code at a specific commit. |
| `internal/builds/` | Build container images from source. |
| `internal/runtime/` | Run isolated, resource-limited preview containers. Distinct from `dockerx`, which constructs clients. |
| `internal/routing/` | Stable preview URLs, routing, and HTTPS. |
| `internal/reporting/` | PR comments and check results about preview status. |
| `internal/secrets/` | Safely manage configuration secrets for previewed applications. |
| `internal/buildlogs/` | Store and stream build output. |
| `internal/housekeeping/` | Expiry, reconciliation, and resource cleanup. |
| `internal/dashboard/` | Browser pages for previews and build status. |

These six additional files reserve future generated database packages:

- `internal/auth/dbgen/doc.go`
- `internal/installations/dbgen/doc.go`
- `internal/webhooks/dbgen/doc.go`
- `internal/environments/dbgen/doc.go`
- `internal/secrets/dbgen/doc.go`
- `internal/buildlogs/dbgen/doc.go`

Each contains just a package comment and declaration. They are handwritten
placeholders, not generated queries. No feature `queries.sql` files exist yet.

## Web assets, documentation, and automation

| Path | Purpose and current contents |
| --- | --- |
| [web/static/.gitkeep](web/static/.gitkeep) | Reserves space for future HTMX JavaScript, compiled CSS, icons, and other browser assets. No frontend assets exist yet. |
| [docs/local-development.md](docs/local-development.md) | Docker setup, environment loading, migration/test commands, schema explanation, and the Phase 2 verification record. |
| [docs/.gitkeep](docs/.gitkeep) | Empty skeleton placeholder; has no runtime effect. |
| [.github/workflows/.gitkeep](.github/workflows/.gitkeep) | Reserves the GitHub Actions workflow directory. No CI or release workflow is configured yet. |

`.gitkeep` is simply a convention for preserving an otherwise empty directory
in Git; neither Go nor Docker executes it. `doc.go` is a Go package documentation
file and is included when the package is compiled.

The workspace also has `.git/` for Git metadata and environment-managed hidden
directories `.agents/`, `.codex/`, and `.aws/`. These are outside the application
layout described here; their contents were not inspected for this guide. They
are not evidence that Previa implements agent features or AWS deployment.

## Development tools: `tools/`

| File | Purpose and current contents |
| --- | --- |
| [tools/go.mod](tools/go.mod) | Separate Go module with `tool` declarations and pinned dependencies for the five development commands below. Keeps their dependencies separate from application dependencies. |
| [tools/go.sum](tools/go.sum) | Checksums for that tools module's downloaded dependencies. |

| Tool | Pinned version | Used for |
| --- | --- | --- |
| templ | v0.3.1020 | Generate Go code from future `.templ` HTML templates. |
| sqlc | v1.31.1 | Generate typed Go database code from future SQL queries. |
| goose | v3.28.0 | Apply and roll back SQL migrations. |
| Air | v1.67.4 | Rebuild and restart the development server when files change. |
| golangci-lint | v2.14.0 | Check Go code for problems. |

The many indirect dependencies in `tools/go.mod` belong to these tools; they do
not mean all those libraries are part of Previa's runtime. Commands use
`go tool -modfile=tools/go.mod <tool>`, so globally installed copies are not needed.

## Make targets and a normal local workflow

Run these from the repository root:

| Command | What it does now |
| --- | --- |
| `make generate` | Runs templ generation; explicitly skips sqlc while its package list is empty. There are no templates to generate yet; templ can warn that the application has no templ dependency. |
| `make build` | Runs `go build ./...` to compile-check the application packages. Does not package a deployment. |
| `make test` | Runs `go test ./...`. Live database tests skip unless `TEST_DATABASE_URL` is set. |
| `make lint` | Runs `go vet ./...` and the pinned golangci-lint. |
| `make run-server` | Runs the empty server entry point; it exits. |
| `make run-worker` | Runs the empty worker entry point; it exits. |
| `make migrate-up` | Applies unapplied migrations to `DATABASE_URL`. |
| `make migrate-down` | Rolls back the latest migration, currently dropping the core tables. |

Migration targets use an exported `DATABASE_URL` first, then `.env`, then
`.env.example` when `.env` is absent. `ENV_FILE=.env.local` selects another local
file. These targets need only the database connection. Goose reads `GOOSE_DBSTRING`;
the River migration adapter reads `DATABASE_URL`. Neither puts the URL in arguments.

```sh
# Start the database and apply its schema.
docker compose -f deploy/docker-compose.yml up -d --wait
make migrate-up

# Check code and run all tests, including live PostgreSQL tests.
make build lint
TEST_DATABASE_URL='postgres://previa:change-me@localhost:5432/previa?sslmode=disable' \
  make test

# Optional watcher; useful once the server has an implementation.
go tool -modfile=tools/go.mod air
```

Phase 2 has already been verified against a fresh database, including migrate-up,
rollback, reapplication, pool connectivity, and schema constraints. The detailed
record is in [docs/local-development.md](docs/local-development.md).

For a first code-reading pass, open `.env.example`, `config/config.go`,
`deploy/docker-compose.yml`, `0001_init.sql`, `database/database.go`, and their
tests in that order. This follows configuration through storage setup to actual
Go database connectivity without requiring any of the future feature packages.
