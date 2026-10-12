# Project handoff
Last updated: 2026-10-12

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- Edit technical-docs/inside-previa.md only when the owner asks (last updated 2026-10-12).
- docs/27-roadmap.md: Prompts 1-13 done; Prompt 14 (GitHub access reconciliation) not started.
- Backend packages are flat internal/<feature>; construction in internal/app;
  presentation in web/ with no backend imports. Preserve the signal theme,
  modal confirmations, server-rendered navigation and GET/POST fallbacks.

## Run and configuration
- Go 1.26.9+; make build/test/lint/vuln, river-ui (jobs, :8090), observability-check.
  Lint at 0 (gocognit 30, nestif, gocritic, revive): split functions, don't raise limits.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 28 and River 007.
  Dev DB was last checked at 21: run migrations before restarting server and worker.
  Processes refuse an older schema. make egress-image and make router-up prepare Docker.
- APP_ENV, LOG_LEVEL, INTERNAL_ADDR (127.0.0.1:9090), WORKER_INTERNAL_ADDR (:9091),
  TRUSTED_PROXIES, BACKUP_STATUS_FILE, BUILDS_PAUSED; /livez, /readyz and /metrics
  live only on internal listeners; healthcheck/check-config subcommands.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping,
  test-e2e. Browser: UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser;
  UI_TEST_PATTERN narrows cases. Make loads .env; direct Go commands need exports.
- Release: make release-images PREVIA_VERSION=x; make prod-smoke PREVIA_VERSION=x
  (needs age; SMOKE_OLD_DUMP rehearses restore; SMOKE_KEEP=1 keeps failure).
  Smoke uses 127.0.0.1:18080/18443/13000.
- WSL: cwd on /mnt/c can go stale; use absolute paths. Docker Desktop's credential
  helper sometimes fails pulls; retry or use an empty DOCKER_CONFIG. .env holds an
  empty ENV_FILE=: unset it after sourcing .env before invoking Make.

## Repository compatibility (2026-10-12, committed 2f7fdb7; migration 0028)
- Finished Claude's partial build-secret changes and the owner's compatibility list.
  Secrets default runtime-only; Allow opens an owner-only CSRF-protected modal.
  Provider.ForBuild + runtime recheck + BuildKit session gates withhold all credentials
  from forks/unknown heads. REGISTRY_AUTH supplies pull logins only; no app injection.
  Mounted values and registry tokens redact build logs and returned errors (docs/07,09).
- Follow-up fix: conflicting fork flags/matching IDs use an unverified
  cache scope. Retry image reuse checks both repository IDs. Unit regression failed
  before the fix, then passed; real BuildKit rejected other identities and kept retry reuse.
- context selects a contained subfolder; root config remains required. Source detects
  relevant .gitmodules and LFS pointers before building; no automatic fetching.
  build_timeout bounds the image stage within BUILD_TIMEOUT's overall deadline.
  Workers advertise their ceiling; Starter kits use the lowest live executor limit.
- Default catalog adds digest-pinned MySQL 8.4: isolated tmpfs, 1 CPU/1 GiB, generated
  app/root credentials, authenticated readiness, MYSQL_* and MYSQL_URL. DATABASE_URL
  favors PostgreSQL when both SQL services are selected (docs/11). Kits offer MySQL;
  Spring/Laravel wrappers map it; Rails notes explain mysql2 driver/settings.
- UI explains private packages, nginx 8080/nginx-unprivileged, limits, Compose,
  platforms/GPU/Windows and build-time settings. Runtime already permits low ports
  inside containers; stock nginx can still fail on read-only caches/PID paths.
- Verified: make build/generate; full Go tests with DB (1406 passed), go build/vet,
  lint 0; all 49 browser cases; full real BuildKit suite plus identity-reuse check;
  real MySQL readiness/data/isolation/cleanup. Full runtime had one Docker socket
  timeout; the unhealthy subtest passed alone on rerun. Backend and basic E2E passed,
  including 50 HTTPS requests + failed-rebuild preservation. Live private-registry pull untested.
- Regression harness: queue tests use isolated River schemas (a live dev worker
  claimed shared test jobs). Basic E2E uses httpd instead of prematurely closing
  netcat, on pinned BusyBox (Alpine omits httpd); 50 routed requests + failure diagnostics.
- README/DESIGN and docs/02,06,07,09,11,21 reflect the requested extensions.
  Follow-up regression fixes and verification docs are included on ui-improvements.

## Earlier improvements (committed 973907f / 93191aa; migrations 0022-0027)
- Forks never auto-build (LockIntentPR + ClaimBuildIntent fence; docs/16).
- Fairness: BUILDS_PER_ACCOUNT, BUILD_MINUTES_PER_ACCOUNT_DAY, account/quota waits,
  waiting_since, wakeNextWaitingTx; PREVIEW_MAX_PER_ACCOUNT and startup capacity
  warning (docs/17). Production example sized for VPS-2.
- Build traffic: loopback egress meter, build log totals and metrics/alerts;
  BUILD_EGRESS=registries fails closed (docs/09). 27 alert rules.
- Retention: BUILD_LOG_RETENTION_DAYS, log deletion, housekeeping_records retention;
  line timestamps local on page, UTC download (docs/08,15).
- Activity: crash watch -> finished build logs, visits from Traefik metrics,
  live App output, PREVIEW_IDLE_SLEEP and wake, private previews with forwardAuth
  (internal/appoutput, idle, previewaccess; docs/08,10).
- Private previews/wake remain untested against real Traefik; observability checks passed.

## Backend previews and outbound (docs/11)
- services {postgres|redis|mysql: version} resolves through PREVIEW_SERVICE_CATALOG
  (empty = pinned defaults; none = off), setup argv, <=3 processes, docs_path.
- Per-candidate services use internal services network, fresh tmpfs, generated
  redacted credentials; readiness -> setup -> processes -> app. All companions
  carry io.previa.candidate and are removed with the app; preview_components (0020).
- Outbound: <=10 owner-approved host:port destinations, one per port;
  PREVIEW_OUTBOUND_PORTS (none = off), gateway allow files refreshed every minute;
  /etc/hosts maps names to the proxy; forks get none.

## Branches, release and pending work
- Current branch: ui-improvements (tracks origin/ui-improvements).
  master changes only through main -> master PRs. CI runs for PRs
  into master and pushes to master; last observed green on c94d35f.
- docs/25 is the deployment guide: OVHcloud VPS-2 + Cloudflare DNS + R2 backups
  (~€7/mo), the owner's choice. NEXT-FEATURES.md lists ten candidates, none built.
- docs/26: ready for staging; production needs a real host, backups/restore/upgrade,
  domains + Let's Encrypt staging/production, hosted GitHub OAuth/App/webhooks,
  authorized disposable-PR source/reporting tests, protected release environment,
  Alertmanager and optional gVisor. E2E exercises real routing with test certificates;
  this task did not rerun the dedicated ACME routing suite.
- Regression tracker: 242 cases in 16 test-docs files; only tick recorded proof.
  Open owner decision: install-tailwind.sh curl has no retry/time limit.
