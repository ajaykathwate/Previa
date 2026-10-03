# Project handoff
Last updated: 2026-10-03

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- Phase 1 complete: docs/PHASE1_AUDIT.md covers Prompts 1–5 and environment/log fixes.
- Phase 2 complete: persistent repository directory + HTMX PR/detail panels.
- STOP: await confirmation before Phase 3 (open-only PR discovery/tracking); not started.
- docs/APP_USER_FLOW.md: working browser features, steps and Mermaid flows.
- docs/WEBHOOKS_AND_PR_DISCOVERY.md: ingress/discovery contracts, setup and fixtures.
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
- PostgreSQL 17 via Compose; make migrate-up applies Goose 9 and River 007.
  Connection credentials use environment, never migration CLI arguments.
- make run-server/run-worker autoload .env; exported overrides win. make dev
  reloads the server via Air; restart the worker after worker changes.
- SharedConfig: DATABASE_URL. Server: HTTP/OAuth/session, optional all-or-nothing
  five-setting App group, independent server-only GITHUB_WEBHOOK_SECRET.
- Worker: ping concurrency, BUILD_TIMEOUT (12m), optional App ID/private-key pair.
  No OAuth/session/slug/App user secret needed. Without App pair, ping works and
  discovery stays queued for a configured worker. PEM must have owner-only access.
- Real GitHub setup: docs/GITHUB_INSTALLATIONS.md; OAuth callback
  http://localhost:8080/auth/github/callback; App setup /installations/callback,
  App authorization callback /installations/authorize/callback.
- Server/worker health-check DB at startup; graceful worker shutdown drains jobs.
- make test-webhook loads env and signs a harmless local ping fixture in memory.
  Optional WEBHOOK_TEST_URL/WEBHOOK_FIXTURE/WEBHOOK_EVENT/WEBHOOK_DELIVERY_ID are
  tooling-only. App webhook requires the same secret and a public HTTPS receiver URL.

## Working features
- Local HTMX 2.0.11, official Tailwind 4.1.18 and templUI 1.13.2; generated templ
  files/CSS ignored, generated sqlc files committed. No frontend runtime service.
- OAuth/users, signed state, SCS sessions (24h/30m idle), rotation, CSRF/logout modal.
- Home, protected Overview, Repositories, PR detail, current Builds/Environments,
  Developer Tools ping and /protected. Installation-owner scoping on queries/actions.
- Repositories: persistent narrow directory; 10 rows, search and access tabs.
  /repositories?repo_id=<id>&pr=<number> selects PRs/details; legacy paths redirect.
- HTMX targets: repository-results, repository-detail, pull-request-results.
  Independent filters/history/no-JS fallback; stacked mobile panels and inline errors.
- Refresh/policy stay in place; 3s polling pauses for focused PR search/open modal.
  PR state tabs, actual sync progress/last success, accessible build-mode modal.
- PR detail: latest/requested/deployed SHA, actual GitHub link, fork/refresh/access
  notices. Build button disabled; direct POST rejects without authorizing work.
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
- Before enabling Build: verify fresh upstream state outside locks, fence admission,
  implement exact-SHA source/config, Docker build/runtime, fork/secrets, routing,
  GitHub reporting, durable cleanup and recovery/expiry scheduling.
- Discovery still uses state=all and explicit initial Refresh; Phase 3 changes this.
- README/DESIGN/UI/setup docs reflect the workspace; PR-scoping update awaits Phase 3.

## Verification
- Build/tests/vet/lint pass; PostgreSQL-backed full suite and webhook/discovery race
  tests pass. Real River pagination, concurrent dedup, rollback/retry, source/access
  races, Manual no-build, policy, ownership and CSRF covered.
- All 19 browser checks pass: panel-only selection, stale/failed requests, filters,
  history, policy retries, polling/modal focus, session expiry, mobile and no-JS flow.
- Compiled localhost server DB health/startup, invalid signature 401 and signed
  make test-webhook 204 verified; worker starts with all GitHub/session env unset.
- Phase 1/2 isolated test DBs/env files cleaned up; application DB not migrated.
- No real GitHub delivery or preview build tested; execution controls stay disabled.
