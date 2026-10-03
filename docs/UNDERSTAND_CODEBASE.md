# Understand the Previa codebase

This guide maps the current implementation and its local runtime. Requirements
and technology choices are in [README.md](../README.md); feature behavior and
architecture are in [DESIGN.md](../DESIGN.md). For commands to get started, see
[DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md). This file is a code navigation aid,
not a product feature list.

## Current shape

Accepted next scheduling design: Manual per repository by default, with opt-in
Automatic after a five-minute trailing quiet period. Neither policy persistence
nor scheduling is implemented. The current parsed-event service and `finishJob`
still enqueue newer observed generations unconditionally; adapt both before live
build integration. PR discovery/detail UI is a recommendation, not implemented.
See [BUILD_POLICY_AND_USER_FLOW.md](BUILD_POLICY_AND_USER_FLOW.md).

Previa is a Go application for creating temporary preview environments for GitHub
pull requests. The repository currently has a working HTTP server with GitHub
OAuth sessions, verified GitHub App installation linking, PostgreSQL persistence, and a separate River worker. The browser
public home page leads to GitHub sign-in and a protected `/dashboard`, which
shows database-derived overview counts. Separate protected pages provide
paginated repositories (`/repositories`), placeholder builds (`/builds`) and
environments (`/environments`), and queue diagnostics (`/developer-tools`). Generation-fenced build/teardown orchestration and the durable-publication
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
| `web/layouts/`, `web/pages/` | Public HTML shell, authenticated app shell, and home/auth/dashboard/repository/build/environment pages; no backend imports. |
| `web/components/`, `web/styles/` | Stable templUI source, explicit navigation keys, Tailwind CSS with shared theme tokens and base/home/workspace/repositories/confirmation/motion sources. |
| `web/assets.go`, `web/static/` | Embedded browser assets, including generated CSS, local HTMX, and progressive menu/queue-form JavaScript. |
| `internal/auth/` | GitHub OAuth, user upsert, sessions, CSRF, and logout. |
| `internal/installations/` | Verified redirect linking, App user authorization, lifecycle functions, ownership-scoped search/access filters and 10-row database pagination; SQL and persistence tests; no webhook endpoint. |
| `internal/platform/githubapp/` | App JWT signing and bounded, cached installation authentication using ghinstallation v2.18.0. |
| `internal/dashboard/` | Public home, overview/session/tools and build/environment placeholder routes; CSRF-protected ping action. |
| `internal/webhooks/` | PR state/generation persistence and transactional enqueueing; HTTP validation/signature endpoint is not connected yet. |
| `internal/environments/` | Generation-aware build/teardown jobs, fenced active/pending state, durable operation outbox, operation worker/recovery, and reconciler interfaces. External adapters are not wired. |
| `db/migrations/` | Goose-managed application schema migrations. |
| `internal/*/dbgen/` | sqlc-generated database code for feature queries. |
| `deploy/docker-compose.yml` | Local PostgreSQL service and persistent data volume. |
| `docs/DEVELOPMENT_SETUP.md` | Full local prerequisites, configuration, migrations, run commands, and checks. |
| `docs/BUILD_POLICY_AND_USER_FLOW.md` | Accepted scheduling decision, proposed PR UI flow, current gaps and implementation checks. |
| `docs/FEATURE_IMPLEMENTATION_PROMPTS.md` | Five sequential next-phase tasks: repoconfig, durable intent, scheduling, webhooks and PR workspace; unresolved decisions and execution boundaries. |
| `TEMP.md` | Operational notes, process configuration, queue checks, and verification record. |
| `docs/DESIGN_DURABLE_PUBLICATION.md` | Durable, generation-fenced publication decision, implemented foundation, and remaining adapter boundary. |

## Processes and configuration

- PostgreSQL is shared by the server, worker, and migrations.
- `cmd/server` loads `ServerConfig`: database plus HTTP, OAuth, session values and an optional all-or-nothing GitHub App group.
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
`make templates` produces Templ code; `make generate` also produces sqlc code.
`make watch-templates` regenerates presentation code via Air polling without an
HTTP server or configuration; generated files are not edited by hand.

## Presentation and assets

See [UI_ARCHITECTURE.md](UI_ARCHITECTURE.md). Official Tailwind v4.1.18 is a
checksum-pinned standalone CLI; Node.js is unnecessary. `make assets` compiles CSS.
Build/test/lint/server targets prepare assets; Air does so before every rebuild.
Worker startup generates Go code but does not install or run Tailwind.
Component source is copied from templUI v1.13.2; generated Templ Go and CSS are
ignored. Update dependencies deliberately, retaining license/provenance.

Optional browser regression checks live in `web/tests/browser`: run
`make browser-install` once (Node.js 20+), then `make test-browser`. The temporary
Go fixture uses actual auth/session/CSRF middleware with fake external providers;
no database or credentials are required. Ordinary Go tests skip these checks.
See `web/tests/browser/README.md` for coverage and troubleshooting.

Installation setup/test commands and the callback/revision/token boundaries are in
[GITHUB_INSTALLATIONS.md](GITHUB_INSTALLATIONS.md). `make test-github-app` is an
opt-in real API proof; normal Go tests exercise fake GitHub APIs and skip that check.
