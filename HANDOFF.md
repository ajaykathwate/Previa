# Project handoff
Last updated: 2026-10-04

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- Phase 3 committed as 88e077d: docs/PHASE3_REPORT.md records open-only tracking,
  mergeability, test evidence and the pending live GitHub proof.
- docs/FEATURE_IMPLEMENTATION_PROMPTS.md replaces completed prompts with 12 remaining
  MVP tasks, source through deployment acceptance. These tasks are not yet executed.
- docs/DEVELOPMENT_SETUP.md, .env.example, Makefile, deploy/docker-compose.yml:
  setup/config; TEMP.md records remaining integration work.
- Intent/publication contracts: docs/BUILD_INTENTS.md, BUILD_SCHEDULING.md,
  GENERATION_ORDERING.md and DESIGN_DURABLE_PUBLICATION.md.
- Backend features stay flat under internal/<feature>; construction in internal/app.
  Presentation stays in web/, with view models and no backend imports.
- Preserve the "signal" theme (theme.css light/dark tokens, system sans + mono),
  quiet input focus, modal confirmations, server-rendered navigation, live search
  and ordinary GET fallback. Palette/theme toggle/view transitions/prefetch are
  presentation-only layers in web/static/js/ui.js and web/styles/motion.css.

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
- 2026-10-04 UI redesign (branch feat/ui-redesign): grouped sidebar -> icon rail (<1180px) ->
  drawer (<900px); header search opens Ctrl/⌘K command palette; theme toggle;
  Overview pipeline/checklist from real counts; sticky toolbars; PR rows become
  labelled cards on phones; PR detail lifecycle + labelled "Soon" placeholders.
- Repositories: responsive cards (<=3 columns, 1 on phones) in a 1240px column.
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
- 2026-10-04 UI redesign: make build, make lint (0 issues) and go test ./... pass
  (DB-backed tests skip without TEST_DATABASE_URL). Browser suite 22/22 passes with
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium (Playwright's bundled Chromium is not
  installed). Committed on branch feat/ui-redesign (not pushed or merged).
- Prior Phase 3 DB/race/migration/webhook checks: docs/PHASE3_REPORT.md.
