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
- `TEMP.md` — configuration sources, processes, queue checks, and approval proposals.
- `Makefile`, `.env.example`, `deploy/docker-compose.yml` — actual local commands,
  configuration keys, and PostgreSQL service definition.

Read only the sections relevant to the current task. Check `git status` first;
this handoff is a navigation aid, not a substitute for checking the working tree.

## Repository conventions

- Go project; package by feature, shared infrastructure under
  `internal/platform/` (see `DESIGN.md` Section 20).
- Go tool dependencies are pinned in `tools/go.mod`; Make targets wrap common
  build, test, lint, generate, run, and migration commands.
- Keep secrets in ignored `.env` or external key files; never commit them.

## Current development setup

- PostgreSQL 17 is provided by `deploy/docker-compose.yml`; the default local
  connection is in `.env.example`.
- Run `make migrate-up` for Goose application migrations then River's own migrations.
- Run `make build`, `make test`, and `make lint` for standard Go checks.
- Set `TEST_DATABASE_URL` to the local database URL to run database integration
  tests rather than having them skip.

## Session notes

- Completed GitHub OAuth login and sessions after Phase 3, following DESIGN.md
  Sections 2 and 3. No repo installation or actual dashboard data is built.
- Completed Phase 4 continuation: River queue/worker foundation per Section 7.
  Only `ping` is implemented; no build/deploy/teardown jobs or environment behavior.
- River runtime, pgx driver, and CLI are pinned to v0.47.0. River owns its schema
  and migration history; `make river-migrate-up` / `make river-status` use its CLI.
  Local River schema is version 007; Goose remains version 2.
- `internal/platform/queue` constructs insert-only server and executing worker
  clients, owns PingArgs/PingWorker, and enqueues through a pgx transaction.
  `ActivePRUniqueOpts` uses tagged JSON `pr_key`, excludes kind/queue, and covers
  available/pending/running/retryable/scheduled. Future PR jobs must share this policy.
- `internal/app/worker.go` loads config, health-checks DB, starts River, and drains
  on signals; `cmd/worker/main.go` stays thin. Run `make run-worker` alongside server.
  The isolated ping queue cap is MAX_CONCURRENT_BUILDS per worker process; run one
  worker locally. Ping timeout 10s, rescue threshold 1m, max attempts 5/default backoff.
- Home has an authenticated ping form. `POST /queue/ping` uses RequireUser and
  session CSRF, validates the synthetic PR key, returns 202 with job_id/duplicate.
  See `internal/dashboard/ping_handler.go` and auth/queue integration tests.
- `internal/app/server.go` loads config, opens the DB pool (startup ping), mounts
  routes, and runs the HTTP server with graceful shutdown. `cmd/server/main.go`
  owns signals and logging; construction stays in app.
- `internal/platform/httpx` builds chi with request IDs, slog logging, and panic
  recovery. Middleware tests verify recovered status and shared request IDs.
- `internal/platform/ui/layout.templ` is the HTML shell. The placeholder dashboard
  shows a login link or the current GitHub username and logout link.
- `web/assets.go` embeds `web/static`; HTMX 2.0.11 is served locally at
  `/static/htmx.min.js` with upstream license and checksum recorded alongside it.
- Export `.env` before run targets; HTTP_ADDR defaults to `127.0.0.1:8080`.
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
- `/protected` demonstrates RequireUser; callback returns home; OAuth tokens discarded.
- `make dev` runs Air with `.env`; generation includes templ and sqlc. It watches
  Go/templ/CSS/JS/SQL and excludes generated output to avoid reload loops.
- `docs/AUTH_TESTING.md` covers setup, manual browser login, error cases, tests,
  and a checklist for all eight Section 2 steps.
- Build, tests (including PostgreSQL), vet, and lint passed. Live checks verified
  home/login redirects, callback URI, state rejection, and CSRF rejection.
  GitHub exchange/profile are simulated in automated tests; real consent requires
  the user's OAuth App and browser. Test DB rows are cleaned up/rolled back.
- Queue live check: server HTTP enqueue → separate worker → DB job 10 completed,
  attempt 1/finalized_at set. Temporary test session/user cleaned; ping row retained.
  Queue tests verify transaction rollback/visibility, cross-kind/queue uniqueness,
  terminal release, concurrency, completion, permanent cancellation; auth test checks
  ping login/CSRF/key validation. All-package DB tests and vet/lint passed again.
  Server/worker stopped; local PostgreSQL remains running. Air reloads server only.
- No dedicated unit tests for entrypoints/app orchestration; live startup/shutdown
  exercised them. TEMP.md contains proposed follow-ups only, awaiting approval.
