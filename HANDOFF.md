# Project handoff
Last updated: 2026-10-08

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 26 numbered docs; 02 setup, 21 operations, 22 observability, 23 incidents, 24 production deployment, 25 release report, 26 roadmap.
  New numbered docs shift later slots (links, labels, H1s).
- docs/26-roadmap.md: Prompts 1-12 done. Prompt 13 (backend previews: managed PostgreSQL/Redis, setup command, worker processes, owner-approved outbound
  connections) and Prompt 14 (detect missed uninstall/suspend/repository-removal
  webhooks by reconciliation) are planned, not started. docs/25-release-report.md answers the
  release checklist: ready for staging; production needs the pending live checks.
- Backend features flat under internal/<feature>; construction in internal/app; presentation in web/ with view models and no backend imports. Keep the "signal"
  theme, modal confirmations, server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- Go 1.26.8+; make build/test/lint/vuln; make observability-check (promtool via Docker).
- PostgreSQL 17 via Compose; make migrate-up applies Goose 19 and River 007. The dev DB was last seen at 16: run make migrate-up before make run-*.
  Processes refuse an older schema ("run migrations first").
- Settings added in Prompt 11: APP_ENV (development|production), LOG_LEVEL, INTERNAL_ADDR (server 127.0.0.1:9090), WORKER_INTERNAL_ADDR (127.0.0.1:9091),
  TRUSTED_PROXIES, BACKUP_STATUS_FILE, BUILDS_PAUSED (kill switch, migration 0018).
- /livez, /readyz, /metrics on the internal listeners; previa-server|worker healthcheck and check-config subcommands.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping, test-e2e (~75 s, includes a GitHub reporting outage). Browser:
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser.
- Release: make release-images PREVIA_VERSION=x (Dockerfile targets server, worker, egress); make prod-smoke PREVIA_VERSION=x (needs age; SMOKE_OLD_DUMP to
  rehearse an older-schema restore; SMOKE_KEEP=1 to inspect a failure). Uses
  ports 127.0.0.1:18080/18443/13000 and project previa-smoke.
- WSL quirk: the shell's cwd on /mnt/c sometimes goes stale ("Failed to get current directory"); use absolute paths (the Makefile does for prod-smoke).
- Docker Desktop's credential helper sometimes fails pulls ("error getting credentials"); retry, and avoid unneeded frontends (Dockerfile has no syntax line).

## Prompts 1-10 (details in docs/06-21)
- Source, secrets, logs, BuildKit builds, hardened previews, fenced routing, teardown, PR feedback, housekeeping; the worker composes the pipeline from
  validated dependencies (internal/app/pipeline.go); server records, worker
  executes; owner Build/Retry/Rebuild/Tear down; worker_capabilities heartbeat.

## Prompt 11: observability and hardening (docs/22, docs/23)
- internal/platform/logging (redaction by key and shape, correlation via
  logging.With), metrics (bounded labels; state gauges exported by the server at
  scrape time via app/observe.go), queue/observe.go (River middleware),
  httpx/ingress.go (client IP, host check, nonce CSP + headers, body limit,
  RateLimit, LimitConcurrency), httpx/internal.go (internal listener, Probe).
- Headline metric previa_request_to_preview_seconds (from available_at).
- Bug found by the smoke test and fixed: queue.Stats FILTER placement (now tested).
- Kill switch: Service.Paused(); WaitPaused; capability "paused"; dashboard and
  PR comment say "Builds paused".
- Deps: Go 1.26.8, containerd 2.3.6; prometheus client, x/time/rate, goose (cmd/migrate).

## Prompt 12: packaging and release (docs/24, docs/25)
- Dockerfile (cross-compiled, distroless, UID 65532; reproducible: identical
  binaries on a clean rebuild), deploy/production (compose.yml, compose.smoke.yml,
  previa.env.example, setup.sh, backup.sh, restore.sh, smoke.sh),
  deploy/observability (prometheus.yml, alerts.yml + tests, Grafana), CI and
  release workflows, .goreleaser.yaml. db/migrations.go embeds migrations.
- Router: router-config one-shot writes previa-dashboard.yml into the route dir
  (a config mount inside the read-only bind failed); dashboard cert TLS-ALPN-01.

## UX pass (2026-10-05, docs/20 Pull request page, Overview, Secrets tab)
- Rule change: a blocked build action is shown disabled with the reason and fix
  (Actions.Blocked/Hint); missing PREVIEW_BASE_DOMAIN is named explicitly.
- PR page status panel (headline, address + Copy, actions, failed-log tail via
  buildlogs.Store.Tail); lists: titles, times, Preview column; repo cards: facts;
  Overview: checklist until done, previews/builds lists; secrets: multi-line
  editor + .env (ParseDotenv, PutMany/DeleteMany, all or nothing); logs: Copy,
  Download (…/logs/download), error tint; relative times (time[data-relative]).
- Sync never spins forever: no discovery worker -> "Waiting for a worker" + notice;
  older than SyncStaleAfter (5m) -> "Sync stalled", Request starts a new revision.
  Mergeability gives up after 8 attempts -> pull_requests.mergeability_unresolved_at
  (migration 0019) -> "Unknown"; cleared by a new observation or check.
- .env.example: every line uncommented with a one-line label ([Required] /
  [Required for <step>] / [Optional, default x]; optional lines hold the defaults,
  enforced by config/example_test.go). technical-docs/config-setup.md explains
  every setting (local, command line, production) with steps to get each value.
  Router dir errors name the fix. Brand PNGs + favicon in web/static/brand.
- Conversations (docs/20): home How it works and FAQ (initStory, initFAQ; signed-out /faq -> /#faq).
- Overview (docs/20): pipeline shares, coverage ring, tabs with expandable rows,
  preview lifetimes (initOverview); same Summary, presentation only.
- technical-docs/inside-previa.md: the end-to-end book (35 Mermaid diagrams render).

## CI follow-up (2026-10-08)
- Earlier fixes de40bb3/16e1d22: templates before tidy, tools tidy from tools/,
  Docker-job generation, executable production scripts, smoke expects Goose 19.
- Scanner installer archive v0.65.0 returns 404; CI now uses digest-pinned official
  aquasec/trivy:0.65.0, local Docker images, shared runner DB cache and same gate.
- Ubuntu 24.04 runner sets apparmor_restrict_unprivileged_userns=0 for rootless
  BuildKit; setup/deployment docs give temporary/persistent host commands.
- sandbox.go detects exited builders and retains bounded startup logs/probe errors;
  sandbox_test.go covers stopped daemons, oversized logs, log errors and cancellation.
- Previous proof: all real Docker suites, release images/scans; smoke 43/43 (encrypted backup, rollback, restore 18 -> 19).
- Rechecked 2026-10-08: build, vet/lint, PostgreSQL race tests, real BuildKit suite,
  three release images, both Trivy gates, actionlint and docs fences/111 local links pass.
- Last observed GitHub CI 37508788804 tests 16e1d22. Push the CI fix commit, then confirm the new run tests its SHA.
  Re-running the old job uses the old commit; guides now explain this.
- GitHub Ubuntu host verification still needs a new run with these changes.

## Pending (docs/25)
- A real host (setup.sh, stack, backups to object storage, restore, upgrade);
  domains + DNS token (Let's Encrypt staging then production); hosted GitHub
  (OAuth/App, webhooks, real repo, make test-source / test-reporting with
  TEST_GITHUB_* of an authorized disposable PR, not in .env); workflows on GitHub
  and a protected release environment; Alertmanager; optional gVisor.
