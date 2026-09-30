# Project handoff

Last updated: 2026-09-30

## Project

Previa is a self-hosted service intended to build isolated preview environments
for GitHub pull requests. Requirements and selected stack are in `README.md`;
feature behavior, edge cases, and package structure are in `DESIGN.md`.

## Start here

- `README.md` — requirements and technology choices.
- `DESIGN.md` — implementation design; Section 20 describes package layout.
- `DEVELOPMENT_SETUP.md` — local prerequisites, PostgreSQL, migrations, and Go commands.
- `Makefile`, `.env.example`, `deploy/docker-compose.yml` — actual local commands,
  configuration keys, and PostgreSQL service definition.

Read only the sections relevant to the current task. Check `git status` first;
this handoff is a navigation aid, not a substitute for checking the working tree.

## Repository conventions

- Go project; package by feature, shared infrastructure under
  `internal/platform/` (see `DESIGN.md` Section 20).
- Keep scope to the requested feature and follow README/DESIGN as source of truth.
- Go tool dependencies are pinned in `tools/go.mod`; Make targets wrap common
  build, test, lint, generate, run, and migration commands.
- Keep secrets in ignored `.env` or external key files; never commit them.

## Current development setup

- PostgreSQL 17 is provided by `deploy/docker-compose.yml`; the default local
  connection is in `.env.example`.
- Run `make migrate-up` to apply pending migrations.
- Run `make build`, `make test`, and `make lint` for standard Go checks.
- Set `TEST_DATABASE_URL` to the local database URL to run database integration
  tests rather than having them skip.
- Follow `DEVELOPMENT_SETUP.md` for full setup, manual command equivalents,
  startup, shutdown, and troubleshooting.

## Session notes

- Completed the minimal HTTP/UI skeleton after Phase 2. Read DESIGN.md Section 17
  for the dashboard pattern; only the hello page was requested.
- `internal/app/server.go` loads config, opens the DB pool (startup ping), mounts
  routes, and runs the HTTP server with graceful shutdown. `cmd/server/main.go`
  owns signals and logging; construction stays in app.
- `internal/platform/httpx` builds chi with request IDs, slog logging, and panic
  recovery. Middleware tests verify recovered status and shared request IDs.
- `internal/platform/ui/layout.templ` is the HTML shell; `internal/dashboard`
  owns the root route and hello template. A handler test verifies the layout.
- `web/assets.go` embeds `web/static`; HTMX 2.0.11 is served locally at
  `/static/htmx.min.js` with upstream license and checksum recorded alongside it.
- `HTTP_ADDR` defaults to `127.0.0.1:8080`. Export `.env` variables before
  `make run-server`; template generation runs automatically.
- `make dev` starts pinned Air using `.air.toml`, loading `.env` and polling
  Go/templ/CSS/JS changes. Each rebuild generates templates; generated Go files
  are excluded from watching. Refresh the browser after rebuilds.
- Verified build, all unit tests, vet, and lint; localhost returned HTTP 200 and
  served the exact local HTMX bytes. Air regenerated templates and restarted
  after a temporary template edit; the changed page was served successfully.
- Scope stops at the skeleton: auth, dashboard data, and other features are
  unimplemented. Await the user's next task rather than extending them.
- When a task changes project state, decisions, verification results, or next
  steps, refresh this file before handing off. Keep it brief and under 100 lines.
