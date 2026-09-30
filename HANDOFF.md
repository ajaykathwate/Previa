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

- Completed GitHub OAuth login and sessions after Phase 3, following DESIGN.md
  Sections 2 and 3. No repo installation or actual dashboard data is built.
- `internal/app/server.go` loads config, opens the DB pool (startup ping), mounts
  routes, and runs the HTTP server with graceful shutdown. `cmd/server/main.go`
  owns signals and logging; construction stays in app.
- `internal/platform/httpx` builds chi with request IDs, slog logging, and panic
  recovery. Middleware tests verify recovered status and shared request IDs.
- `internal/platform/ui/layout.templ` is the HTML shell. The placeholder dashboard
  shows a login link or the current GitHub username and logout link.
- `web/assets.go` embeds `web/static`; HTMX 2.0.11 is served locally at
  `/static/htmx.min.js` with upstream license and checksum recorded alongside it.
- `HTTP_ADDR` defaults to `127.0.0.1:8080`. Export `.env` variables before
  `make run-server`; template generation runs automatically.
- `internal/auth` owns OAuth handlers, profile fetch, user repository, signed state,
  SCS sessions, RequireUser, session-bound CSRF, routes, and error/logout templates.
- `APP_BASE_URL` defaults to `http://localhost:8080`; the exact OAuth App callback
  is `http://localhost:8080/auth/github/callback`. Configure the OAuth ID/secret and
  a random SESSION_SECRET in `.env`; no GitHub App installation is needed for login.
- `0002_auth.sql` adds avatars and the SCS sessions schema/index. Run
  `make migrate-up`. sqlc generates user upsert/lookup and session inspection;
  SCS pgxstore owns session CRUD, hashes stored tokens, and cleans expired rows.
- Session lifetime is 24h with a 30m idle timeout. Cookies are HttpOnly/Lax and
  Secure for HTTPS (HTTP permitted only locally). Login rotates tokens; logout
  uses GET confirmation then CSRF-protected POST `/auth/logout` and deletes sessions.
- `/protected` demonstrates RequireUser; callback always returns home rather than
  using a client-controlled redirect. OAuth tokens are discarded after `/user`.
- `make dev` runs Air with `.env`; generation includes templ and sqlc. It watches
  Go/templ/CSS/JS/SQL and excludes generated output to avoid reload loops.
- `docs/AUTH_TESTING.md` covers setup, manual browser login, error cases, tests,
  and a checklist for all eight Section 2 steps.
- Build, tests (including PostgreSQL), vet, and lint passed. Live checks verified
  home/login redirects, callback URI, state rejection, and CSRF rejection.
  GitHub exchange/profile are simulated in automated tests; real consent requires
  the user's OAuth App and browser. Test DB rows are cleaned up/rolled back.
- When a task changes project state, decisions, verification results, or next
  steps, refresh this file before handing off. Keep it brief and under 100 lines.
