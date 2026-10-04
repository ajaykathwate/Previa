# Project handoff
Last updated: 2026-10-04

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- Phase 3 implemented: docs/PHASE3_REPORT.md records open-only tracking, mergeability,
  test evidence and the pending live GitHub proof. Stop at this phase.
- docs/DEVELOPMENT_SETUP.md, .env.example, Makefile, deploy/docker-compose.yml:
  setup/config; TEMP.md records remaining integration work.
- Intent/publication contracts: docs/BUILD_INTENTS.md, BUILD_SCHEDULING.md,
  GENERATION_ORDERING.md and DESIGN_DURABLE_PUBLICATION.md.
- Backend features stay flat under internal/<feature>; construction in internal/app.
  Presentation stays in web/, with view models and no backend imports.
- Preserve the approved palette, fonts, quiet input focus, modal confirmations,
  server-rendered navigation, bounded tables, live search and ordinary GET fallback.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc and run build/tests/vet/lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 10 and River 007.
  Connection credentials use environment, never migration CLI arguments.
- make run-server/run-worker autoload .env; exported overrides win. make dev
  reloads the server via Air; restart the worker after worker changes.
- SharedConfig: DATABASE_URL. Server: HTTP/OAuth/session, optional all-or-nothing
  five-setting App group, independent server-only GITHUB_WEBHOOK_SECRET.
- Worker: ping concurrency, BUILD_TIMEOUT (12m), optional App ID/private-key pair.
  No OAuth/session secrets needed; without App pair discovery/checks stay queued.
  PEM must have owner-only access.
- Real GitHub setup: docs/GITHUB_INSTALLATIONS.md; OAuth callback
  http://localhost:8080/auth/github/callback; App setup /installations/callback,
  App authorization callback /installations/authorize/callback.
- make test-webhook loads env and signs a harmless ping; App webhooks need the
  same secret and a public HTTPS receiver URL.

## Working features
- Local HTMX 2.0.11, official Tailwind 4.1.18 and templUI 1.13.2; generated templ
  files/CSS ignored, generated sqlc files committed. No frontend runtime service.
- OAuth/users, signed state, SCS sessions (24h/30m idle), rotation, CSRF/logout modal.
- Home, protected Overview, Repositories, PR detail, current Builds/Environments,
  Developer Tools ping and /protected. Installation-owner scoping on queries/actions.
- Repositories uses compact cards at /repositories: two desktop columns, one mobile;
  1000px content width, natural height, count and pagination only for multiple pages.
  Cards open /repositories/<id>; PRs open /repositories/<id>/pull-requests/<number>.
- PR list alone has Sync Pull Requests + soft-background Enable Automatic Builds.
  Manual is off; enabling needs confirmation, disabling saves directly. Sync polls every 3s, pausing for
  focused PR search/open modal. Desktop filters one row; short/mobile pages scroll.
- PR detail: saved SHA/link/fork/access, Open/Checking/Conflicts/Closed/Merged.
  Active PR detail and Open/All lists poll saved status every 3s with focus/modal guards.
  Build disabled; direct POST gives conflict/checking reason, never authorizes work.
- Builds shows actual current/latest intents, no archive. Environments shows actual
  deployment state; no fabricated URL/health/logs. Empty records stay empty.

## Ingress/discovery consistency
- POST /webhooks/github is outside browser session/CSRF; max 2 MiB, raw HMAC-SHA256
  verification before parse. Unconfigured secret 503; invalid signature 401.
- Atomic delivery-ID receipt + PR/lifecycle changes + coordinator intent/wake.
  Concurrent duplicates apply once; failures roll back receipt and can retry.
  No GitHub IO inside ingress/locks. Receipts store hash/identity, never full payload.
- PR source_updated_at orders upstream snapshots separately from generation.
  Stale observations ignored; conflicting equal-time heads invalidate work and
  require refresh; closed/revoked state wins ties. Fork metadata retained.
- Lifecycle uses shared parser and ApplyEventTx. Deletion terminal, repository
  removal tombstones protect ties. Revocation sets durable cleanup_pending;
  absent cleanup adapter is never reported successful. Verified link can restore access.
- internal/pullrequests owns workspace and discovery. River pr_discovery queue:
  two workers, 45s/page, <=100 API records/page, rate-limit snoozes, eight attempts.
- Request/run revision coalesces clicks and fences old jobs. Capture installation
  revision + repo observation_version before API; recheck under install→repo→sync→PR
  locks before save. Page/progress/continuation commit together. No inferred closure
  from omitted/partial pages, no automatic build authorization from discovery.

## Execution boundary and next work
- Non-executable Broker persists Manual/Automatic observation coordination;
  Automatic has a durable five-minute trailing deadline. Close supersedes work.
- Build/teardown/wake/publication workers are still not registered in production.
  Durable intent/wake-ups wait for real adapters; no fake runtime is supplied.
- Result/outbox/finalization/reporting/cleanup guards carry generation and revision.
  External publication uses bounded calls outside short transactions with leases,
  relevance checks, adapter fencing/compensation and protected resource references.
- Before enabling Build: fresh upstream verification, exact-SHA source/config,
  Docker runtime, fork/secrets, routing, reporting, cleanup/recovery/expiry.
- Discovery is state=open at connection and every sync; callbacks/additions seed once.
- Unknown closures never insert; tracked closure/merge retains row ID. Legacy data retained.
- Migration 10 stores nullable mergeable, mergeable_state and derived build_status.
  Null checks use tracked-PR GETs with 2s/eight-attempt polling and revision/SHA fences.
  Conflict/checking gates cover manual, dispatch, runtime admission and publication.

## Verification
- Build/tests/vet/lint and final PR/environment/webhook race checks pass.
  PostgreSQL full suite and signed open-only/null/conflict checks pass. Local Git
  conflict/push proves both modes refuse then admit runtime. River continuation,
  dedup/rollback/source-access races, ownership, policy and CSRF remain covered.
- All 20 browser checks pass, including modal/focus guards, navigation, responsive
  and no-JS flows. Temporary Chromium/libs used; command is in PHASE3_REPORT.md.
- Phase 3 disposable DB/env cleaned; rollout build/tests/vet/lint checked.
- Server/worker DB startup and signed local webhook ping previously verified.
- Application DB migrated through 10 (pending 9/10 applied); Goose 10/River 007,
  PR columns and rollback-protected constraint tests verified. Restart server/worker.
  Live GitHub PR proof pending chosen disposable repo/authenticated write access;
  CLI unauthenticated; .env webhook secret unset. App key accessible. Execution disabled.
