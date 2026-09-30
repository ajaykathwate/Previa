# Project handoff

Last updated: 2026-09-30

## Start here

- `README.md` and `DESIGN.md` are source of truth; read only task-relevant sections.
- Check `git status` before acting; verify this handoff against current code.
- `docs/DEVELOPMENT_SETUP.md`, `.env.example`, `Makefile`, and `deploy/docker-compose.yml`
  describe setup. `TEMP.md` explains process/configuration and records verification.
- `docs/GENERATION_ORDERING.md` explains the generation SQL, job handoff, cleanup,
  timeout/rescue policy, tests, and boundaries requiring future adapters.
- `docs/DESIGN_DURABLE_PUBLICATION.md` is the target outbox/publication architecture;
  `GENERATION_ORDERING.md` remains the record of the current implementation.
- Feature packages stay flat; shared infrastructure is under `internal/platform`.
  Do not build later-phase features without a task asking for them.

## Run and verify

- Go/toolchain dependencies are pinned. `make generate`, `make build`, `make test`,
  and `make lint` wrap generation/build/tests/vet/golangci-lint.
- PostgreSQL 17 runs through Compose. `make migrate-up` applies Goose then River.
  Current application schema: Goose 3; River 007 (River v0.47.0).
- River migrations use `cmd/river-migrate` / `internal/app/river_migrate.go` and
  DATABASE_URL from environment; Goose uses GOOSE_DBSTRING. No credential flags.
- Set TEST_DATABASE_URL for DB tests. New ordering tests create/drop isolated schemas.
- Export `.env`, then run `make run-server` and `make run-worker` separately.
  `make dev` runs Air for server reload only; generated dbgen/templ output is excluded.
- Config: SharedConfig has DATABASE_URL (also River's connection); ServerConfig adds
  HTTP/OAuth/session settings; WorkerConfig adds MAX_CONCURRENT_BUILDS and optional
  BUILD_TIMEOUT (default 12m). Unbuilt integrations have no config fields.
- `docs/UNDERSTAND_CODEBASE.md` is the current codebase map;
  `docs/DEVELOPMENT_SETUP.md` is the local setup guide. `README.md` links the setup guide.

## Completed foundation and authentication

- Thin cmd entrypoints; app constructs dependencies and handles graceful shutdown.
  Server and worker DB pools are health-checked on startup.
- chi request IDs/logging/recovery; templ shell; embedded local HTMX 2.0.11 assets.
- GitHub OAuth, users keyed by GitHub ID, signed state, login rotation, SCS pgxstore,
  RequireUser, session CSRF, logout. Session lifetime 24h, idle timeout 30m.
- OAuth callback: http://localhost:8080/auth/github/callback. Configure server-only
  OAuth ID/secret and SESSION_SECRET. Browser checks: `docs/AUTH_TESTING.md`.
- `/protected` requires login; home shows login/username/logout and a ping form.
  POST /queue/ping requires login+CSRF; returns job_id/duplicate. Ping round-trip tested.
- Default production server/worker still expose/process only ping. Webhook HTTP,
  Docker runtime, Traefik, GitHub publication, provisioning, and reconciliation
  remain unimplemented. New ordering code uses small interfaces for these dependencies.

## Generation ordering core (DESIGN Sections 7 and 13)

- `0003_pr_generation.sql`: PR generation BIGINT, environment applied_generation,
  and deployed head_sha. Existing PRs start at zero; first recorded event becomes 1.
- `webhooks.Service.ProcessPR` accepts validated opened/synchronize/closed events.
  Its sqlc UPSERT increments generation exactly once per committed invocation and
  enqueues via River InsertTx in the same transaction. No HTTP/signature handler added.
- `environments/jobs.go`: immutable generation/SHA/state args for build/teardown,
  separate queues, same PR-only uniqueness. New desired state lives on the PR row
  while an older job is active. Finish locks PR, completes old job, and inserts the
  latest successor atomically; terminal worker errors use the same handoff.
- `environments/queries.sql` ApplyResult uses MATERIALIZED CTE SELECT ... WHERE
  generation=expected FOR UPDATE on the PR, then UPDATE environments ... RETURNING.
  A stale result performs no environment write. The PR lock stays held through
  the publication callback, preventing a newer event between guard and registration.
- `service.go` guards success/failure, skips stale queued builds, logs supersession,
  and preserves the old preview on current rebuild failure. `cleanup.go` explicitly
  removes obsolete containers then owned images, preserving referenced/newer resources.
  Runtime resource discovery/removal and Publisher operations are interfaces; real
  adapters must be idempotent, cancellation-aware, labeled, and compensate failures.
- `worker.go` registers build/teardown workers when real adapters exist. Build budget
  comes from BUILD_TIMEOUT + 1m completion grace; teardown 2m + 1m; ping 5s.
  River rescue scan floor is 30s; River also honors each registered worker's Timeout.
  All maintenance-capable workers must register the same kinds/timeouts.
- DB transactions do not make external effects atomic. Crash reconciliation,
  including exhausted crash-recovery jobs, still belongs to the future adapters/
  Section 15. See generation documentation for exact integration requirements.

## Verification

- Application migration 3 applied locally; no real containers/images were built.
- PostgreSQL/River tests cover late success/failure, close during build, stale queued
  work, cleanup retry/order, old-preview preservation, both lock-race directions,
  exact concurrent increments, enqueue rollback, and captured generation/handoff.
- Real River rescuer test verifies expired ping/build/teardown handling while a
  legitimately slow build remains running. Build/tests/vet/lint passed.
- Earlier checks confirmed worker startup with OAuth unset and migration credentials
  absent from 39 ps auxww samples. PostgreSQL remains running; no app processes left.
