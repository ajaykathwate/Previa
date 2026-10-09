# Project handoff
Last updated: 2026-10-09

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 25 docs (02 setup, 11 backend previews, 19 publication
  decision + fencing, 22 operations, 23 observability, 24 incidents, 25 deployment,
  26 release report, 27 roadmap). 03 and 18 are retired (user guide in
  technical-docs, decision merged into 19); numbers are not reused or shifted.
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
- Prompt 11 (docs/23, 24): platform/logging redaction, bounded metrics,
  httpx/ingress.go (client IP, host check, nonce CSP, body/rate/concurrency limits);
  headline metric previa_request_to_preview_seconds; kill switch Service.Paused().
- Prompt 12 (docs/25, 26): distroless Dockerfile (UID 65532), deploy/production
  (compose, setup/backup/restore/smoke), deploy/observability, CI/release workflows,
  .goreleaser.yaml; router-config writes previa-dashboard.yml; TLS-ALPN-01.
- UX pass (docs/21): disabled actions say why; PR status panel; secrets editor and
  .env import; sync stalls after 5m; mergeability "Unknown" after 8 tries (0019);
  .env.example lines all labelled (config/example_test.go).

## CI (guides technical-docs/ci-in-action.md, cd-in-action.md)
- Trivy via digest-pinned image; runner allows unprivileged userns for rootless
  BuildKit; "Free runner disk" step; build tests use small disk thresholds.
  Not pushed yet: confirm a run tests the pushed SHA.

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
- Committed d612277 (feature), 6174a28 (CI disk fix), 3e055a1 (DESIGN diagram).

## Code quality pass (2026-10-08; behavior unchanged)
- .golangci.yml adds gocognit (30, tests exempt), nestif, gocritic and revive style
  rules; make lint must stay at 0 issues. Split rather than raise limits.
- Long functions became named steps (environments operation_finalize.go; runtime
  candidate/services/outbound.go; app execution.go, process.go). Shared rules:
  cleanupFence, lockOwnerIntentScope, transitionAllowed, judgeObservation.
- webhooks delivery SQL in sqlc; cmd mains return exit codes so defers run.

## Regression cycle (2026-10-09, committed e9aac61; test-docs/)
- test-docs/: 242 cases in 16 module files (ID, type, priority, level, steps,
  expected, evidence); README holds strategy, commands and cycle results.
  Tick a case only when its evidence passed in a recorded run.
- Fixed: D-01 htmx auth refusals now plain text (shown as the reason); D-02 GitHub
  link only for https://; D-03 Go 1.26.9 + x/net v0.60.0 (13 new advisories);
  D-04 smoke.sh reads the Go version from the Dockerfile's GO_IMAGE pin.
- Open (owner decision): install-tailwind.sh curl has no retry/time limit; a GitHub
  download stall hung a release-images run for 27 minutes.

## Deployment and next features (2026-10-09)
- docs/25 is the one deployment guide (cd-in-action §9-11 point to it): OVHcloud
  VPS-2 + Cloudflare DNS + R2 backups (~€7/mo), the owner's choice for cost and
  account handling. Do not reintroduce the earlier providers (owner's request).
  Starter/VPS-4 budgets; "Account safety" (Manual builds for public repos).
- NEXT-FEATURES.md (root): 10 candidate features, none built.

## Pending (docs/26)
- A real host (setup.sh, stack, backups to object storage, restore, upgrade);
  domains + DNS token (Let's Encrypt staging then production); hosted GitHub
  (OAuth/App, webhooks, real repo, make test-source / test-reporting with
  TEST_GITHUB_* of an authorized disposable PR, not in .env); workflows on GitHub
  and a protected release environment; Alertmanager; optional gVisor.
