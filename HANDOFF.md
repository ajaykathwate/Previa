# Project handoff
Last updated: 2026-10-12

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 25 docs (02 setup, 21 UI, 22 operations, 25 deployment,
  26 release report, 27 roadmap); 03 and 18 are retired, numbers never reused.
  Edit technical-docs/inside-previa.md only when the owner asks (updated on request 2026-10-12).
- docs/27-roadmap.md: Prompts 1-13 done. Prompt 14 (detect missed uninstall/suspend/
  repository-removal webhooks by reconciliation) is planned, not started (an attempt
  was reverted at the owner's request). docs/26-release-report.md answers the release
  checklist: ready for staging; production needs the pending live checks.
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports. Keep the "signal"
  theme, modal confirmations, server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- Go 1.26.9+; make build/test/lint/vuln, river-ui (jobs, :8090), observability-check.
  Lint at 0 (gocognit 30, nestif, gocritic, revive): split functions, don't raise limits.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 27 and River 007 (dev DB
  still at 21: run it, then make egress-image and make router-up). Processes refuse
  an older schema ("run migrations first").
- Prompt 11 settings: APP_ENV, LOG_LEVEL, INTERNAL_ADDR (127.0.0.1:9090),
  WORKER_INTERNAL_ADDR (:9091), TRUSTED_PROXIES, BACKUP_STATUS_FILE, BUILDS_PAUSED.
  /livez, /readyz, /metrics only on internal listeners; healthcheck/check-config subcommands.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping,
  test-e2e (TestRealEndToEnd + TestRealBackendEndToEnd). Browser:
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser (UI_TEST_PATTERN=regex).
- Release: make release-images PREVIA_VERSION=x (targets server, worker, egress); make
  prod-smoke PREVIA_VERSION=x (needs age; SMOKE_OLD_DUMP rehearses an older-schema
  restore; SMOKE_KEEP=1 keeps a failure). Uses 127.0.0.1:18080/18443/13000.
- WSL: cwd on /mnt/c can go stale; use absolute paths. Docker Desktop's credential
  helper sometimes fails pulls; retry or use an empty DOCKER_CONFIG. .env holds an
  empty ENV_FILE=: unset it after sourcing .env in a script, or Make cannot find it.

## Prompts 1-12 (details in docs/06-26)
- Server records, worker executes; internal/app/pipeline.go composes the worker from
  validated dependencies. Prompt 11: redaction, metrics, nonce CSP, kill switch.
  Prompt 12: distroless images, deploy/production, observability, CI/release.

## Committed 2026-10-11 (docs/21)
- Dashboard redesign, user-level home page (landing page lives in its own repo),
  Starter kits (/starter-kits, two-tab editor; HTMX settles `class`, so use data-*),
  failure attention (7 days, Dismiss/Undo, 0021) and Environments renamed Previews.
  6a42a66 and 5e4d30a are not pushed.

## Safety, capacity, retention, activity (2026-10-12, UNCOMMITTED; migrations 0022-0027)
- Forks never auto-build (LockIntentPR untrusted + ClaimBuildIntent fence; docs/16).
- Fairness (docs/17): BUILDS_PER_ACCOUNT, BUILD_MINUTES_PER_ACCOUNT_DAY (waits
  account/quota), turns by account (waiting_since), wakeNextWaitingTx; per-account
  previews PREVIEW_MAX_PER_ACCOUNT (io.previa.account-id label). Startup capacity
  warning (app/capacity_check.go); prod example sized for VPS-2.
- Build traffic (docs/09): egress meter via loopback control (/_previa/meter,
  previa-egress meter), build log totals, previa_build_egress_bytes + alerts;
  BUILD_EGRESS=registries (builds.RegistryHosts, fail closed). 27 alert rules.
- Retention (docs/08, 15): BUILD_LOG_RETENTION_DAYS, Delete logs (deleted_at),
  housekeeping_records (30/90 days); line timestamps (local on page, UTC download).
- Activity (docs/08, 10): crash watch (Docker die events -> build log via
  Store.OpenFinished; io.previa.redact-env label), visits from Traefik metrics
  (ROUTER_METRICS_ADDRESS), live App output (internal/appoutput), idle sleep/wake
  (internal/idle, PREVIEW_IDLE_SLEEP, /previews/wake), private previews
  (internal/previewaccess, forwardAuth /previews/auth, PREVIEW_ACCESS_URL).
- Verified: lint 0, all Go tests with DB, make test-builder, make test-runtime,
  observability-check, browser tests (49). Not run: test-routing/test-e2e; private
  previews and the wake redirect are untested against a real Traefik.

## Branches and CI (technical-docs/ci-in-action.md, cd-in-action.md)
- Work on main; master is the release branch, changed only through PRs (main ->
  master). CI runs for PRs into master and pushes to master; green on c94d35f.

## Prompt 13: backend previews (docs/11)
- .previewrc.yaml: services {postgres|redis: version} from PREVIEW_SERVICE_CATALOG
  (empty = pinned PG 17/16, Redis 8/7 by digest; none = off), setup argv, <=3
  processes, docs_path (repoconfig.ParseFor + Platform; source.NewPreparer).
- Runtime (backend/candidate/services.go): per-candidate services on the PR's
  internal "-services" network, generated credentials (redacted, never stored),
  tmpfs data, readiness services -> setup -> processes -> app; containers labelled
  io.previa.candidate and removed with the app. preview_components (migration 0020).
- Outbound (internal/outbound, web/pages/outbound): <=10 host:port, one per port,
  PREVIEW_OUTBOUND_PORTS (none = off); worker writes allow files on previa-egress
  (outbound_refresh every minute); /etc/hosts maps names to the proxy; forks get none.

## Regression cycle (2026-10-09; test-docs/)
- 242 cases in 16 files; tick a case only when its evidence passed in a recorded run.
- Open (owner decision): install-tailwind.sh curl has no retry/time limit.

## Deployment and next features (2026-10-09)
- docs/25 is the one deployment guide: OVHcloud VPS-2 + Cloudflare DNS + R2 backups
  (~€7/mo), the owner's choice. Do not reintroduce the earlier providers.
- NEXT-FEATURES.md (root): 10 candidate features, none built.

## Pending (docs/26)
- A real host (setup.sh, stack, backups to object storage, restore, upgrade);
  domains + DNS token (Let's Encrypt staging then production); hosted GitHub
  (OAuth/App, webhooks, real repo, make test-source / test-reporting with
  TEST_GITHUB_* of an authorized disposable PR, not in .env); the protected release
  environment; Alertmanager; optional gVisor.
