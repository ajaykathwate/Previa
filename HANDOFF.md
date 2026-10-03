# Project handoff
Last updated: 2026-10-03

## Start here
- `README.md` and `DESIGN.md` are source of truth; read only task-relevant sections.
- `docs/DEVELOPMENT_SETUP.md`, `.env.example`, `Makefile`, and `deploy/docker-compose.yml`
  describe setup. `TEMP.md` records verification; `docs/GITHUB_INSTALLATIONS.md` covers App setup.
- `docs/GENERATION_ORDERING.md` explains the generation SQL, job handoff, outbox,
  retries, cleanup, timeout/rescue policy, tests, and adapter boundaries.
- `docs/DESIGN_DURABLE_PUBLICATION.md` records the durable-publication decision,
  implemented database/River foundation, and integrations still required.
- Feature packages stay flat; shared infrastructure is under `internal/platform`.
  Presentation lives under `web/`; see `docs/UI_ARCHITECTURE.md`.
- `docs/BUILD_POLICY_AND_USER_FLOW.md`: accepted Manual default + opt-in Automatic
  five-minute debounce, not implemented; next tasks: docs/FEATURE_IMPLEMENTATION_PROMPTS.md.
  Adapt ProcessPR/finishJob to authorized intent before live builds; no build history.

## Run and verify
- Go/toolchain dependencies are pinned. `make generate`, `make build`, `make test`,
  and `make lint` wrap generation/build/tests/vet/golangci-lint.
- PostgreSQL 17 runs through Compose. `make migrate-up` applies Goose then River.
  Current application schema: Goose 5; River 007 (River v0.47.0).
- River migrations use `cmd/river-migrate` / `internal/app/river_migrate.go` and
  DATABASE_URL from environment; Goose uses GOOSE_DBSTRING. No credential flags.
- Set TEST_DATABASE_URL for DB tests. New ordering tests create/drop isolated schemas.
- `make run-server` and `make run-worker` load `.env` with exported overrides.
  `make dev` reloads server; `make watch-templates` polls templates only (no env needed).
- Config: SharedConfig has DATABASE_URL (also River's connection); ServerConfig adds
  HTTP/OAuth/session plus optional all-or-nothing App settings; WorkerConfig adds concurrency and
  BUILD_TIMEOUT (default 12m). Unbuilt integrations have no config fields.
- Setup guide covers config sources/OAuth/Make env loading; direct Go needs exports.
  Restart processes after config edits.

## Completed foundation and authentication
- Thin cmd entrypoints; app constructs dependencies and handles graceful shutdown.
  Server and worker DB pools are health-checked on startup.
- chi middleware; all presentation in `web/` (pages/layouts/components/styles).
  Official Tailwind v4.1.18 + stable templUI v1.13.2; local HTMX 2.0.11; shared SVG arrows.
- GitHub OAuth, users keyed by GitHub ID, signed state, login rotation, SCS pgxstore,
  RequireUser, session CSRF, logout. Session lifetime 24h, idle timeout 30m.
- OAuth callback: http://localhost:8080/auth/github/callback. Configure server-only
  OAuth ID/secret and SESSION_SECRET. Browser checks: `docs/AUTH_TESTING.md`.
- `/` is public; GitHub sign-in redirects to protected `/dashboard` (sticky navbar/sidebar).
  Overview uses actual counts; `/repositories` has ownership-scoped SQL pagination
  (10 rows), search/access filters; `/builds` and `/environments` are separate placeholders.
  `/developer-tools` holds ping; `/protected` shows sessions; logout uses a shared modal.
- Installations: verified redirect + separate App user authorization; status/repo access,
  revision fences, cached tokens, parsed lifecycle functions. Cleanup adapter is not wired.
- Default production queue still runs only ping. Webhook HTTP,
  Docker runtime, Traefik, GitHub publication, and environment provisioning remain
  unimplemented. A bounded reconciliation contract exists but is not wired to a
  production runner. Adapter dependencies remain behind small interfaces.

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
- `environments/queries.sql` ApplyResult uses a materialized generation-guarded PR
  lock and updates environment state in the same statement. Success stores a pending
  candidate; active fields change only after operation finalization.
- `Repository.Apply` inserts an `environment_operations` row and River wake-up in
  the same short transaction. No external call runs under the PR lock. The operation
  worker claims with a lease, executes through `OperationExecutor` outside a DB
  transaction, then finalizes with a generation check. Retry uses capped exponential
  backoff; safe error codes are stored, not raw provider errors.
- `Reconciler.RunOnce` recovers due operation jobs and schedules cleanup for
  unreferenced inventory results. It has no production timer/adapter wiring.
- `service.go` skips stale queued builds and schedules durable cleanup. Docker,
  route generation fencing, GitHub reporting, and real resource inventory remain
  adapter work; operation workers are not registered in the production app.
- `worker.go` registers build/teardown workers when real adapters exist. Build budget
  comes from BUILD_TIMEOUT + 1m completion grace; teardown 2m + 1m; ping 5s.
  River rescue scan floor is 30s; River also honors each registered worker's Timeout.
  All maintenance-capable workers must register the same kinds/timeouts.
- DB transactions do not make external effects atomic. Crash reconciliation,
  including exhausted crash-recovery jobs, still belongs to the future adapters/
  Section 15. See generation documentation for exact integration requirements.

## Verification
- Latest docs task: build/tests/vet/lint pass; links checked. DB/browser checks passed earlier.
  10-row pages, internal scrolling/live search; Available-default radio tabs; shared field focus.
  Optional `make browser-install`/`make test-browser`; App proof pending, see TEMP.md.
- Goose 5 applied locally; installation ownership/lifecycle/revision/rename/reinstall
  tests pass in isolated schemas. Real App proof requires credentials and an installation.
- PostgreSQL/River tests cover late success/failure, close during build, stale queued
  work, cleanup retry/order, old-preview preservation, both lock-race directions,
  exact concurrent increments, enqueue rollback, captured generation/handoff,
  durable operation retry/finalization/recovery, pending artifact protection, and
  nonblocking external publication.
- Worker/OAuth-unset and credential-free migrations verified earlier. README/DESIGN
  document accepted policy; five next-feature prompts drafted, no implementation started.
- Local `.env` line 38 is not a shell assignment; sourcing fails. Configuration left
  unchanged. Comment/remove that non-assignment before using make run-server/dev.
