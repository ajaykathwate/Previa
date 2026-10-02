# Understand the Previa codebase

This guide maps the current implementation and its local runtime. Requirements
and technology choices are in [README.md](../README.md); feature behavior and
architecture are in [DESIGN.md](../DESIGN.md). For commands to get started, see
[DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md). This file is a code navigation aid,
not a product feature list.

## Current shape

Previa is a Go application for creating temporary preview environments for GitHub
pull requests. The repository currently has a working HTTP server with GitHub
OAuth sessions, PostgreSQL persistence, and a separate River worker. The browser
home page and protected ping form provide a small runnable example. Generation-
generation-fenced build/teardown orchestration and the durable-publication
database/River foundation are implemented and tested behind interfaces. The real
webhook HTTP adapter, Docker runtime, router publication, GitHub reporting, and
production reconciliation runner are not connected. Do not infer that a preview
build can currently be deployed end to end.

The code follows `cmd → internal/app → feature/platform packages`. Feature code
is grouped by feature with flat package files; shared infrastructure lives under
`internal/platform`. All presentation is under `web/`; handlers remain feature-owned. SQL migrations define the database and sqlc generates typed
queries into feature `dbgen` packages.

## Important files

| Path | Role |
| --- | --- |
| `cmd/server/main.go`, `cmd/worker/main.go` | Thin entry points for the HTTP server and background worker. |
| `internal/app/` | Dependency construction, startup health checks, process wiring, and shutdown. |
| `internal/platform/config/` | Separate shared, server, and worker configuration loaders. |
| `internal/platform/database/` | pgx pool construction and health checks. |
| `internal/platform/queue/` | River client, ping job, queue registration, and per-kind timeout policy. |
| `internal/platform/httpx/` | chi router and shared HTTP middleware. |
| `web/layouts/`, `web/pages/` | Shared HTML shell and feature-grouped pages; no backend imports. |
| `web/components/`, `web/styles/` | Stable templUI component source, official Tailwind entry CSS and theme tokens. |
| `web/assets.go`, `web/static/` | Embedded browser assets, including generated CSS and local HTMX. |
| `internal/auth/` | GitHub OAuth, user upsert, sessions, CSRF, and logout. |
| `internal/dashboard/` | Home page and ping form. |
| `internal/webhooks/` | PR state/generation persistence and transactional enqueueing; HTTP validation/signature endpoint is not connected yet. |
| `internal/environments/` | Generation-aware build/teardown jobs, fenced active/pending state, durable operation outbox, operation worker/recovery, and reconciler interfaces. External adapters are not wired. |
| `db/migrations/` | Goose-managed application schema migrations. |
| `internal/*/dbgen/` | sqlc-generated database code for feature queries. |
| `deploy/docker-compose.yml` | Local PostgreSQL service and persistent data volume. |
| `DEVELOPMENT_SETUP.md` | Full local prerequisites, configuration, migrations, run commands, and checks. |
| `TEMP.md` | Operational notes, process configuration, queue checks, and verification record. |
| `docs/DESIGN_DURABLE_PUBLICATION.md` | Durable, generation-fenced publication decision, implemented foundation, and remaining adapter boundary. |

## Processes and configuration

- PostgreSQL is shared by the server, worker, and migrations.
- `cmd/server` loads `ServerConfig`: database plus HTTP, OAuth, and session values.
- `cmd/worker` loads `WorkerConfig`: database plus queue concurrency and build timeout. It does not need OAuth variables.
- River workers run in the worker process; there is no separate queue server.
- Air (`make dev`) watches and reloads the server only; restart the worker manually after worker changes.
- Compose starts PostgreSQL by default. The application server and worker are
  started separately with their Make targets; neither is a Compose service.

## Database and queue behavior

Goose migrations are in `db/migrations/`; River owns its own schema migrations.
`make migrate-up` applies both. Database connection values are read from the
environment (Goose uses `GOOSE_DBSTRING` internally); migration CLIs do not receive
credentials through command-line flags. `.env.example` groups settings by process.

The configured live queue job is `ping`, enqueued through the authenticated,
CSRF-protected `POST /queue/ping` endpoint. River uniqueness is keyed by PR key
across relevant active states. Build and teardown jobs capture monotonic PR
generations. Environment result writes are fenced in SQL; successful candidates
remain pending until a durable publication operation is finalized. `Repository.Apply`
inserts the operation row and River wake-up transactionally, while the operation
executor runs outside the DB transaction. Operation worker and reconciler contracts
are implemented but not registered in production because the Docker/router/GitHub
adapters do not exist. Current details and tests are in
[GENERATION_ORDERING.md](GENERATION_ORDERING.md); architecture and remaining
integration requirements are in [DESIGN_DURABLE_PUBLICATION.md](DESIGN_DURABLE_PUBLICATION.md).

## Local commands

For complete prerequisites and expected output, use `DEVELOPMENT_SETUP.md`. The
normal flow is:

```sh
docker compose -f deploy/docker-compose.yml up -d --wait postgres
make migrate-up
make run-server   # terminal 1; loads .env automatically
make run-worker   # terminal 2; loads .env automatically
```

Useful checks are `make generate`, `make build`, `make lint`, and `make test`.
Database integration tests require `TEST_DATABASE_URL`; exact commands and
expected results are documented in `DEVELOPMENT_SETUP.md`.

## Development tools

Pinned tools live in the separate `tools` module and are invoked through
`go tool -modfile=tools/go.mod`: templ, sqlc, Goose, Air, and golangci-lint.
`make generate` produces templ/sqlc code; generated files are not edited by hand.

## Presentation and assets

See [UI_ARCHITECTURE.md](UI_ARCHITECTURE.md). Official Tailwind v4.1.18 is a
checksum-pinned standalone CLI; Node.js is unnecessary. `make assets` compiles CSS.
Build/test/lint/server targets prepare assets; Air does so before every rebuild.
Worker startup generates Go code but does not install or run Tailwind.
Component source is copied from templUI v1.13.2; generated Templ Go and CSS are
ignored. Update dependencies deliberately, retaining license/provenance.
