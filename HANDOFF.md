# Project handoff
Last updated: 2026-10-11

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 25 docs (02 setup, 21 UI, 22 operations, 25 deployment,
  26 release report, 27 roadmap); 03 and 18 are retired, numbers never reused.
  Do not edit technical-docs/inside-previa.md (owner's rule, 2026-10-09).
- docs/27-roadmap.md: Prompts 1-13 done. Prompt 14 (detect missed uninstall/suspend/
  repository-removal webhooks by reconciliation) is planned, not started (an attempt
  was reverted at the owner's request). docs/26-release-report.md answers the release
  checklist: ready for staging; production needs the pending live checks.
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports. Keep the "signal"
  theme, modal confirmations, server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- Go 1.26.9+; make build/test/lint/vuln; make observability-check (promtool via Docker).
- PostgreSQL 17 via Compose; make migrate-up applies Goose 20 and River 007. The
  dev DB was last seen at 19: run make migrate-up before make run-*/make dev.
  Processes refuse an older schema ("run migrations first").
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
- Source, secrets, logs, BuildKit builds, hardened previews, fenced routing,
  teardown, PR feedback, housekeeping; the worker composes the pipeline from
  validated dependencies (internal/app/pipeline.go); server records, worker executes.
- Prompt 11 (docs/23, 24): log redaction, bounded metrics, httpx/ingress.go (nonce
  CSP, limits), kill switch Service.Paused(). Prompt 12 (docs/25, 26): distroless
  image, deploy/production, deploy/observability, CI/release, .goreleaser.yaml.
- UX pass (docs/21): disabled actions say why; secrets editor and .env import; sync
  stalls after 5m; mergeability "Unknown" after 8 tries (0019).

## Dashboard redesign (2026-10-11, uncommitted; UI only, docs/21)
- Header trail (layouts.AppTrail, components.Crumb), RepositoryHero, underline tabs,
  web/styles/surfaces.css. Overview: stat tiles, activity table; Workers panel removed.
- PR page: status card + Details panel; tracker (progress.go, ui.js
  initBuildProgress); diagnosis.go explains FailureCode. PR list: author, branch,
  updated time, commit link, one Preview status. Handlers pass existing columns
  only (failure code, html_url, source_updated_at). Button: "Install on GitHub" /
  "Choose repositories on GitHub". Verified: lint 0, make test, browser 45/45.
- Home page (uncommitted): user-level, hosted, free in early access; "works today"
  only (no roadmap); FAQ without internals. Must not depend on index.html.
- The landing page (index.html) moved to its own repository; nothing here uses it.
  Proposed, not built: more PR detail facts; deleting build attempts.

## Branches and CI (technical-docs/ci-in-action.md, cd-in-action.md)
- master: default and release branch, changed only through PRs (by convention: the
  private repo's Free plan has no branch protection); work happens on main. CI runs
  for PRs into master, pushes to master and manual runs; Release refuses tags off master.
- CI green on c94d35f (2026-10-10). Smoke checks that read state settling after
  startup must retry; userns for rootless BuildKit; "Free runner disk" step.

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

## Code quality (2026-10-08)
- .golangci.yml adds gocognit (30), nestif, gocritic, revive; make lint stays at 0
  issues. Split long functions into named steps rather than raising limits.

## Regression cycle (2026-10-09, committed e9aac61; test-docs/)
- test-docs/: 242 cases in 16 module files (ID, type, priority, level, steps,
  expected, evidence); README holds strategy, commands and cycle results.
  Tick a case only when its evidence passed in a recorded run.
- Fixed D-01..D-04 (auth refusals as text, https-only links, Go/x-net, GO_IMAGE).
- Open (owner decision): install-tailwind.sh curl has no retry/time limit; a GitHub
  download stall hung a release-images run for 27 minutes.

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
