# Project handoff
Last updated: 2026-10-05

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 21 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06-12 source..teardown, 13 PR feedback, 14 housekeeping ... 21
  roadmap). Update the matching doc with every change; a new doc takes its
  reading-order slot and later numbers shift (links, labels, H1s; the renumbering
  must include untracked new docs).
- docs/21-roadmap.md: Prompts 1-9 done; next is Prompt 10 (wire real workers and
  dashboard actions), then 11-12.
- Contracts to preserve: docs/06-18 (source .. ordering, incl. 13 PR feedback and 14
  housekeeping/capacity) and 19 realtime. Live hosted GitHub webhook/mergeability
  proof still pending (docs/05).
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports.
- Preserve the "signal" theme, modal confirmations, server-rendered navigation and
  ordinary GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 16 and River 007. The dev
  DB was not migrated in this session (last seen at 12-13): run make migrate-up.
  0014 fails if an existing environments.subdomain is not a lowercase DNS label.
- Live-schema tests (migration constraints, auth, queue) need the DB at the latest
  migration; others create isolated schemas. Verification used a throwaway schema.
- Worker settings (validated, used from Prompt 10): BUILD_*, EGRESS_IMAGE, PREVIEW_*,
  ENV_TTL_HOURS, HOUSEKEEPING_*, BUILD_CACHE_BUDGET; routing ones when
  PREVIEW_BASE_DOMAIN is set (ROUTER_DYNAMIC_DIR on a Linux fs, not /mnt/c).
- Router: deploy/traefik/compose.yml via make router-up (checks ACME_* settings, chmod
  600 DNS credential file, runs make route-sync = cmd/route-sync, then compose up).
  Staging CA by default. No domain/token here, so never started for real.
- Real suites: make test-builder (~3 min), test-runtime (~2.5 min, uses DATABASE_URL
  for a throwaway schema), test-routing (~3 min: Traefik v3.7.13 + Pebble DNS-01 +
  previews + throwaway schema; TEST_ROUTING_LOG=1), test-housekeeping (~30 s).
  make housekeeping-dry-run inspects the configured DB/Docker, changes nothing.
  Browser tests need UI_BROWSER_EXECUTABLE=/snap/bin/chromium.
- Docker Desktop 29.8 (WSL2). Only previa-egress stays running after real tests.

## Prompts 1-7 (details in docs/06-12)
- Source (06), secrets (07), logs (08), BuildKit builds (09), hardened previews (10).
- Routing (11): Traefik file provider fed by a fenced route controller; hostnames
  pr-<n>-<repo id b36>-<8 random>; forward-only file confirmed via loopback API.
- Teardown (12): close, revocation, owner and expiry requests share RequestTeardown /
  TeardownSweeper; never needs GitHub; route removal before verified cleanup.

## Prompt 8: PR feedback (docs/13)
- Latest-state sync: reportTx (same tx as each lifecycle change) bumps
  pull_request_reports.wanted_version + a report_status op; a leased Reporter renders
  current state into one living comment + one check run per (sha, revision). Off
  unless Repository.WithReports(). First report once a PR has preview work.
- Live proof make test-reporting needs TEST_GITHUB_INSTALLATION_ID/REPO_ID/PR_NUMBER
  of an authorized disposable PR: values not in .env, so NOT run (rehearsal passes).

## Prompt 9: housekeeping and capacity (docs/14)
- Owner decisions: ENV_TTL_HOURS (README name; default 48) from the latest
  publication (Repository.WithLifetime; failed attempts keep it; teardown clears);
  delete orphans after HOUSEKEEPING_GRACE (15m), HOUSEKEEPING_DRY_RUN logs only; PR
  reconcile only live-work PRs every 15m, 10/pass; capacity full = wait (WaitingError,
  build_intents.waiting_reason capacity|disk, River JobSnooze 30s..5m).
- Added and flagged: platform_instance (migration 0016) labels/names every Docker
  object (io.previa.instance, previa-<inst>-pr-N, previa/<inst>-pr-N:..); OwnedBy
  checks; advisory-lock keys include it. Pre-0016 objects are invisible.
- environments: WithBuildSlots (advisory xact lock + running count in claimBuild),
  Service.WithHeadroom (runtime.Admit), Reconciler WithGrace/WithDryRun/Inventory,
  PlanResourceCleanup (rolled-back tx), MarkActiveMissing (preview_missing).
- runtime: Parts.Instance + Admission (NewAdmissionLock: session advisory lock from
  capacity check to ContainerCreate); per-repo limit counts PRs; NewInventory for
  inventory-only use. builds: Config.Instance, Job.Instance, disk check counts other
  running builders, Caches/RemoveCache/DanglingImages/RemoveDanglingImage.
- internal/housekeeping: Recovery, Resources (+unknown PRs), PullRequests
  (VerifyUpstream), Expiry, Prune, Housekeeper.Register (queue housekeeping, 5
  periodic jobs RunOnStart, MaxAttempts 1, unique while unfinished); emergency prune
  when a build waits for disk. cmd/housekeeping = dry-run diagnostics.

## Prompt 10 wiring notes
- Executors{activate/remove: routing.Controller, cleanup: runtime.CleanupExecutor(),
  report: reporting.NewReporter(pool, NewGitHubAPI(app, App().ID, slug), Settings{
  APP_BASE_URL, PREVIEW_BASE_DOMAIN}), report timeout < 90 s}; WithReports; backends
  adapter = runtime.Endpoint + AsOperationFailure (real_harness_test.go); Controller.Sync at startup;
  Reconciler.WithRoutes; register TeardownSweepWorker on queue environment_teardown;
  NewRepository(...).WithHostnames(routing.NewLabel).WithLifetime(cfg.PreviewLifetime())
  .WithBuildSlots(cfg.MaxConcurrentBuilds); Service.WithHeadroom(runtime); instance via
  database.Instance for runtime.Parts{Instance, Admission} and builds.ConfigFrom;
  housekeeping parts per docs/14 "Wiring"; Housekeeper.Register into queue.NewWorker;
  operation timeouts < lease.

## Verification (2026-10-05, Prompts 8-9)
- make build; make lint 0 issues; go test ./... on a throwaway schema (Goose 16 +
  River 007): all pass. -race: environments, housekeeping, reporting, runtime,
  builds, pullrequests, dockerx, config.
- Real Docker: test-builder 4/4, test-runtime 8/8 (incl. atomic admission race),
  test-routing 7/7 (fixed a flaky restart test: Pebble's 6-day certs were renewed at
  every Traefik start; harness sets certificatesduration=144), test-housekeeping 2/2.
- Browser 27/27 last run in Prompt 7; not re-run (no UI change).
- Not run: Let's Encrypt staging/live (needs domain + DNS token), gVisor, worker
  dispatch, hosted webhooks, multi-host routing.
