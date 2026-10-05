# Project handoff
Last updated: 2026-10-05

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 22 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06-12 source..teardown, 13 PR feedback, 14 housekeeping, 21 MVP
  operations, 22 roadmap). Update the matching doc with every change; a new doc
  takes its reading-order slot and later numbers shift (links, labels, H1s).
- docs/22-roadmap.md: Prompts 1-10 done; next is Prompt 11 (observability and
  hardening), then 12 (packaging, CI, restore, release acceptance).
- docs/21-mvp-operations.md: who does what, capability/config matrix, queues and
  draining, the proof matrix (scenario -> test) and the live proofs still owed.
- Contracts to preserve: docs/06-18 and 19 realtime. Backend features flat under
  internal/<feature>; construction in internal/app; presentation in web/ with view
  models and no backend imports. Keep the "signal" theme, modal confirmations,
  server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 17 and River 007. The dev
  DB may lag (last seen at 12-13): run make migrate-up before make run-*.
- Live-schema tests need the DB at the latest migration. Verification used a
  throwaway database (create, migrate-up, go test ./..., drop).
- Worker: the App settings turn on discovery; PREVIEW_BASE_DOMAIN + APP_BASE_URL +
  ROUTER_* (+ Docker, egress image, router) turn on builds, routes, reports,
  cleanup, teardown and housekeeping; without a domain it logs "capability
  unavailable" and registers none of them. Optional worker SECRETS_* keyring.
  SOURCE_WORKSPACE_ROOT (absolute; Linux fs, not /mnt/c).
- Router: make router-up (ACME_* settings, chmod 600 DNS credential, route-sync).
  Staging CA by default. No domain/token here, so never started for real.
- Real suites: make test-builder (~3 min), test-runtime (~2.5 min), test-routing
  (~3 min, Traefik + Pebble), test-housekeeping (~30 s), test-e2e (~40 s, whole
  pipeline: real Docker/BuildKit/Traefik default cert + fake GitHub over TLS).
  Browser tests: UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser.
- Docker Desktop 29.8 (WSL2). Only previa-egress stays running after real tests.

## Prompts 1-9 (details in docs/06-14)
- Source (06), secrets (07), logs (08), BuildKit builds (09), hardened previews (10),
  routing via a fenced controller + Traefik file provider (11), durable teardown and
  revocation sweep (12), living comment + check run per (sha, revision) with a
  leased reporter (13), housekeeping/expiry/capacity with instance-scoped Docker
  objects (14, migration 0016).

## Prompt 10: real workers and dashboard actions
- Server records, worker executes. Server: environments.NewRequestBroker (intent +
  wake-up in one tx, never dispatches), WithReports when the App is configured,
  pullrequests.WithActions{Builds, Verifier, Teardowns, Capabilities}.
- Worker: internal/app/pipeline.go buildPipeline composes everything only from
  validated deps and lists unavailable reasons; worker.go adds the capability
  heartbeat (internal/platform/workers, migration 0017 worker_capabilities: 30 s
  interval, stale after 90 s, row deleted on stop) and a 30 s River drain.
- Queues: build (MAX_CONCURRENT_BUILDS + global build slots), teardown 2,
  coordination 4, route 2, report 4, cleanup 2, sweep 1, housekeeping 1; operation
  lease 4 min, timeouts 2m/2m/90s/1m (+30 s).
- Service.Build: DB match -> VerifyUpstream (outside locks) -> headroom -> claim.
  Permanent failures (runtime Permanent(), upstreamFailure) end after one attempt;
  ErrInterrupted on shutdown snoozes without an attempt; UpstreamRetryAfter snoozes
  for GitHub's delay.
- Build endpoint order: owner -> open -> mergeability -> live execution capability
  (503, nothing recorded) -> VerifyUpstream -> RequestBuild (per-form idempotency
  key, expected revision). Buttons: Build latest commit / Build now / Retry build /
  Rebuild preview. Teardown: modal, CSRF, owner-only, reason requested,
  return=environments only to a fixed path. No bulk builds.
- UI: PR detail (waiting/failure reasons, preview URL, expiry, older-preview note,
  removed note), Builds/Environments rows (Open preview, Expires, Tear down), Overview
  delivery counts + stages from worker capabilities. Removed the stale sidebar
  "Coming next: Preview URLs" item; home page status columns updated.
- Deviations (flagged in docs): worker_capabilities table; optional worker keyring;
  SOURCE_WORKSPACE_ROOT; no "queued, position N" (DESIGN §7); Environments shows
  expiry not age; e2e uses Traefik's default cert (issuance proven by test-routing).

## Verification (2026-10-05, Prompt 10)
- make build; make lint 0 issues; go test ./... on a throwaway DB (Goose 17 + River
  007): 29 packages ok. -race -v on changed packages: 190 pass, 0 races; skips are
  only the opt-in real/browser suites.
- Real Docker: test-builder, test-runtime, test-routing (7/7), test-housekeeping,
  test-e2e (publish, failed rebuild keeps v1, teardown, Automatic burst, close with
  no container/image/network left); browser 30/30. Afterwards only previa-egress
  (container + network) remains.
- Fixed during verification: routing leftover check now scoped to its instance;
  e2e cleanup detaches egress before removing networks and fails on any leak (four
  leaked test networks from earlier runs removed).
- Dev DB is at Goose 16: run make migrate-up (0017) before make run-server/worker.

## Live proofs still owed (docs/21)
- Hosted webhooks + discovery; real source via codeload (make test-source); PR
  feedback on a real PR (make test-reporting needs TEST_GITHUB_* of an authorized
  disposable PR: not in .env, NOT run); Let's Encrypt staging/production with a
  domain + DNS token; whole pipeline and capacity on the target host; gVisor;
  backups/metrics/CI (Prompts 11-12). One worker host only.
