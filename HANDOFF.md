# Project handoff
Last updated: 2026-10-08

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 27 docs (02 setup, 11 backend previews, 22 operations,
  23 observability, 24 incidents, 25 deployment, 26 release report, 27 roadmap).
  A new doc takes its reading-order slot; later numbers, links, labels and H1s shift.
- docs/27-roadmap.md: Prompts 1-13 done. Prompt 14 (detect missed uninstall/suspend/
  repository-removal webhooks by reconciliation) is planned, not started (an attempt
  was reverted at the owner's request). docs/26-release-report.md answers the release
  checklist: ready for staging; production needs the pending live checks.
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports. Keep the "signal"
  theme, modal confirmations, server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- Go 1.26.8+; make build/test/lint/vuln; make observability-check (promtool via Docker).
- PostgreSQL 17 via Compose; make migrate-up applies Goose 20 and River 007. The
  dev DB was last seen at 19: run make migrate-up before make run-*/make dev.
  Processes refuse an older schema ("run migrations first").
- Settings added in Prompt 11: APP_ENV (development|production), LOG_LEVEL, INTERNAL_ADDR (server 127.0.0.1:9090), WORKER_INTERNAL_ADDR (127.0.0.1:9091),
  TRUSTED_PROXIES, BACKUP_STATUS_FILE, BUILDS_PAUSED (kill switch, migration 0018).
- /livez, /readyz, /metrics on the internal listeners; previa-server|worker healthcheck and check-config subcommands.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping,
  test-e2e (TestRealEndToEnd + TestRealBackendEndToEnd). Browser:
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser.
- Release: make release-images PREVIA_VERSION=x (Dockerfile targets server, worker, egress); make prod-smoke PREVIA_VERSION=x (needs age; SMOKE_OLD_DUMP to
  rehearse an older-schema restore; SMOKE_KEEP=1 to inspect a failure). Uses
  ports 127.0.0.1:18080/18443/13000 and project previa-smoke.
- WSL: cwd on /mnt/c can go stale ("Failed to get current directory"); use
  absolute paths. Docker Desktop's credential helper sometimes fails pulls; retry
  or use an empty DOCKER_CONFIG. .env holds an empty ENV_FILE=: unset it after
  sourcing .env in a script, or Make cannot find the env file.

## Prompts 1-10 (details in docs/06-22)
- Source, secrets, logs, BuildKit builds, hardened previews, fenced routing,
  teardown, PR feedback, housekeeping; the worker composes the pipeline from
  validated dependencies (internal/app/pipeline.go); server records, worker
  executes; owner Build/Retry/Rebuild/Tear down; worker_capabilities heartbeat.

## Prompt 11: observability and hardening (docs/23, docs/24)
- platform/logging (redaction, logging.With), metrics (bounded labels; server
  state gauges via app/observe.go), queue/observe.go, httpx/ingress.go (client IP,
  host check, nonce CSP, body limit, rate/concurrency limits), httpx/internal.go.
- Headline metric previa_request_to_preview_seconds; kill switch Service.Paused().

## Prompt 12: packaging and release (docs/25, docs/26)
- Dockerfile (distroless, UID 65532, reproducible), deploy/production (compose,
  env template, setup/backup/restore/smoke scripts), deploy/observability,
  CI/release workflows, .goreleaser.yaml; db/migrations.go embeds migrations.
- Router: router-config writes previa-dashboard.yml into the route dir; TLS-ALPN-01.

## UX pass (2026-10-05, docs/21: PR page, Overview, Secrets tab, conversations)
- Blocked build actions show disabled with reason and fix; PR status panel with
  address, actions and failed-log tail; secrets multi-line editor + .env import;
  sync never spins forever (worker notice, stalled after 5m); mergeability gives
  up after 8 attempts (migration 0019, "Unknown").
- .env.example: every line labelled; optional lines hold the defaults
  (config/example_test.go). technical-docs/config-setup.md explains each setting.
- Home How it works + FAQ conversations (static copy: keep answers true);
  Overview pipeline (initOverview); technical-docs/inside-previa.md (the book).

## CI (commits de40bb3..ee7c85e; guides technical-docs/ci-in-action.md, cd-in-action.md)
- Templates before tidy, tools tidy from tools/, executable production scripts;
  Trivy via digest-pinned aquasec/trivy:0.65.0; Ubuntu 24.04 runner sets
  apparmor_restrict_unprivileged_userns=0 for rootless BuildKit; sandbox.go reports
  exited builders with bounded startup logs.
- Push, then confirm a new GitHub run tests the pushed SHA (re-running an old job
  uses the old commit).

## Prompt 13: backend previews (docs/11)
- .previewrc.yaml: services {postgres|redis: version} from PREVIEW_SERVICE_CATALOG
  (empty = pinned PG 17/16, Redis 8/7 by digest; none = off), setup argv, <=3
  processes, docs_path (repoconfig.ParseFor + Platform; source.NewPreparer).
- Runtime (internal/runtime/backend.go): per-candidate services on the PR's
  internal "-services" network (no egress/router), generated credentials (redacted,
  never stored), tmpfs data, readiness services -> setup -> processes -> app; all
  containers labelled io.previa.candidate and removed/inventoried/counted with the
  app's artifact. preview_components (migration 0020) feeds PR page and comment.
- Outbound (internal/outbound, web/pages/outbound): owner tab, <=10 host:port, one per
  port, PREVIEW_OUTBOUND_PORTS (none = off); worker writes allow files on
  previa-egress (Join clears reused addresses; outbound_refresh every minute);
  /etc/hosts maps names to the proxy; forks get none. previa-egress is replaced
  when EGRESS_IMAGE changes; ports are runtime state, not container spec.
- Metrics previa_egress_connections_total{outcome}, previa_egress_bytes_total.
- Restored 2026-10-08 from a stash onto ee7c85e (CI work kept; doc links
  renumbered; smoke.sh and docs expect Goose 20). Verified on a throwaway Goose-20
  DB: go test -race (35 packages), lint 0, browser 36/36, all 5 real suites
  incl. both e2e tests, govulncheck, promtool, actionlint, links, 48 Mermaid diagrams.

## Pending (docs/26)
- A real host (setup.sh, stack, backups to object storage, restore, upgrade);
  domains + DNS token (Let's Encrypt staging then production); hosted GitHub
  (OAuth/App, webhooks, real repo, make test-source / test-reporting with
  TEST_GITHUB_* of an authorized disposable PR, not in .env); workflows on GitHub
  and a protected release environment; Alertmanager; optional gVisor.
