# Project handoff
Last updated: 2026-10-05

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 19 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06 source, 07 secrets, 08 logs, 09 builds, 10 runtime, 11 routing,
  12 teardown ... 19 roadmap). Update the matching doc with every change; a new doc
  takes its reading-order slot and later numbers shift (links, labels, H1s).
- docs/19-roadmap.md: Prompts 1-7 done; next is Prompt 8 (GitHub living comment and
  exact-SHA checks, internal/reporting), then 9-12.
- Contracts to preserve: docs/06-16 (source, secrets, logs, builds, runtime, routing,
  teardown, intents, scheduling, publication, ordering) and 17 realtime. Live hosted
  GitHub webhook/mergeability proof still pending (docs/05).
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports.
- Preserve the "signal" theme, modal confirmations, server-rendered navigation and
  ordinary GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 14 and River 007. The dev
  DB was not migrated in this session (last seen at 12-13): run make migrate-up.
  0014 fails if an existing environments.subdomain is not a lowercase DNS label.
- Live-schema tests (migration constraints, auth, queue) need the DB at the latest
  migration; others create isolated schemas. Verification used a throwaway schema.
- Worker: BUILD_*, EGRESS_IMAGE, PREVIEW_* (validated, unused until Prompt 10) and,
  when PREVIEW_BASE_DOMAIN is set, routing: APP_BASE_URL, ROUTER_DYNAMIC_DIR (Linux
  fs, not /mnt/c), ROUTER_CONTAINER/ENTRYPOINT/API_ADDRESS. Server: optional
  PREVIEW_BASE_DOMAIN (refuses dashboard host equal to/below it; warns if same site).
- Router: deploy/traefik/compose.yml via make router-up (checks ACME_* settings, chmod
  600 DNS credential file, runs make route-sync = cmd/route-sync, then compose up).
  Staging CA by default. No domain/token here, so never started for real.
- Real suites: make test-builder (~3 min), test-runtime (~2 min), test-routing (~3 min:
  Traefik v3.7.13 + Pebble DNS-01 + previews + throwaway schema; TEST_ROUTING_LOG=1).
  Browser tests need UI_BROWSER_EXECUTABLE=/snap/bin/chromium.
- Docker Desktop 29.8 (WSL2). Only previa-egress stays running after real tests.

## Prompts 1-5
- Verification + exact-SHA source (06); encrypted secrets, fork withholding (07);
  current-attempt logs with epochs and SSE (08); rootless BuildKit per build (09);
  hardened unrouted candidates, egress-only networking, readiness, capacity (10).

## Prompt 6: routing (docs/11)
- Owner decisions: Traefik file provider fed by a route controller instead of Docker
  labels (DESIGN §11 + README updated); hostname pr-<n>-<repo id b36>-<8 random>;
  Pebble for the TLS proof; unknown/removed hosts redirected to the dashboard's public
  /previews/unavailable (web/pages/routing).
- Fence in internal/environments (routes.go, route_queries.sql, migration 0014):
  environment_routes moves only forward in (generation, revision), removals are
  tombstones, route_table counter orders versions; accepted, served and
  unconfirmed_targets are protected from cleanup. internal/routing: Controller
  (Endpoint check -> Attach -> AcceptRoute -> forward-only file -> await loopback API
  -> MarkRouteServed), Sync, render (routers carry no tls block), File (flock + atomic
  rename), Traefik (docker exec wget), NewLabel. Admission provisions environments via
  Repository.WithHostnames; expires_at now nullable (expiry is Prompt 9).
- __Host- session/OAuth-state cookies over HTTPS; origin checks in config.
- Limits stated in docs/11: ~1 s reload lag, no per-request freshness, nothing served
  while Traefik is down or its file is missing, single host (file lock).

## Prompt 7: teardown and cleanup (docs/12)
- Close keeps its teardown job; revocation (webhook lifecycle tx: generation bump,
  teardown_requested_generation, superseded intent, environment_teardown_sweep job)
  and future owner/expiry requests use Repository.RequestTeardown / the sweep.
- Teardown never needs GitHub access (lockTeardownScope); retries of one teardown
  start once; the environment hands its artifacts to remove_route; removal relevance
  is route-based; completeRemoval finalizes only its own teardown_generation and
  always schedules the taken-over artifacts' cleanup; removal/cleanup retried until
  done; failed cleanup re-armed when rescheduled; failed activation -> env failed,
  old preview kept, candidate (and replaced candidate) cleaned, report queued.
- runtime: RemoveResource/CleanupExecutor (labels incl. PR, graceful stop, unforced
  images, idle networks), Endpoint, inventory lists build/idle preview networks.
- Fixed pre-existing bug: duplicate outbox enqueue compared JSONB bytes (sameJSON).
- environments.Runtime narrowed to Build + Resources; failed route ops protect nothing.

## Prompt 10 wiring notes
- Executors{activate/remove: routing.Controller, cleanup: runtime.CleanupExecutor(),
  report: Prompt 8}; backends adapter = runtime.Endpoint(runtime.Expected(c)) +
  runtime.AsOperationFailure (see real_harness_test.go); Controller.Sync at startup;
  Reconciler.WithRoutes; register TeardownSweepWorker on queue environment_teardown;
  NewRepository(...).WithHostnames(routing.NewLabel); operation timeouts < lease.

## Verification (2026-10-05)
- make build; make lint 0 issues; make test on a throwaway schema (Goose 14 + River
  007): all pass. -race: routing, runtime, dockerx, config, auth, environments.
- make test-routing 7/7, make test-runtime 7/7, make test-builder 4/4, browser 27/27.
- Not run: Let's Encrypt staging/live (needs domain + DNS token), gVisor, worker
  dispatch, hosted webhooks, multi-host routing.
