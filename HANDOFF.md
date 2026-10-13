# Project handoff
Last updated: 2026-10-13

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
- /livez, /readyz, /metrics: internal listeners :9090/:9091; healthcheck/check-config.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping,
  test-e2e. Browser: UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser;
  UI_TEST_PATTERN narrows cases. Make loads .env; direct Go commands need exports.
- Release: make release-images / prod-smoke PREVIA_VERSION=x (smoke needs age).
- WSL: cwd on /mnt/c can go stale; use absolute paths. Docker Desktop's credential
  helper sometimes fails pulls; retry or use an empty DOCKER_CONFIG. .env holds an
  empty ENV_FILE=: unset it after sourcing .env before invoking Make.

## Preview and login fixes (2026-10-13)
- App output + Private Preview failed real River inserts: custom unique states
  omitted required Running. Followers now deduplicate per watch epoch; route syncs
  per route version. Privacy setting/version/job save atomically via InsertTx.
- Build now applies only to pending Automatic quiet periods; queued/running
  current commits show disabled progress controls. SSE ready refetches current
  state; closing any confirmation resumes deferred refreshes.
- RequireUser preserves safe GET/HEAD URLs through OAuth (HTMX uses same-host
  document); invalid/direct login clears stale next; mutation URLs aren't replayed.
- Regressions reproduced queue failures/enabled queued action before fixes.
  Real River + disposable schema tests cover dedup/replacement/privacy rollback.
  All 51 browser cases and real Docker follow/redaction/sleep/wake test passed.
  Full Go: 1411 passed across 81 packages; build/vet/lint passed. docs/02,08 updated.

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
- Prior verification: 1406 Go tests, 49 browser cases, build/vet/lint, real BuildKit
  identity-reuse, MySQL and backend/basic E2E (50 HTTPS requests + failed rebuild).
  One runtime socket timeout passed alone on rerun; live private-registry pull untested.
- Queue tests isolate River schemas from dev workers. Basic E2E uses BusyBox httpd.
- README/DESIGN and docs/02,06,07,09,11,21 reflect the requested extensions.
  Follow-up regression fixes committed in 2cb0ae1 on ui-improvements.

## Earlier improvements (committed 973907f / 93191aa; migrations 0022-0027)
- Forks never auto-build (LockIntentPR + ClaimBuildIntent fence; docs/16).
- Fairness: BUILDS_PER_ACCOUNT, BUILD_MINUTES_PER_ACCOUNT_DAY, account/quota waits,
  waiting_since, wakeNextWaitingTx; PREVIEW_MAX_PER_ACCOUNT and startup capacity
  warning (docs/17). Production example sized for VPS-2.
- Build traffic: egress meter, totals/metrics; BUILD_EGRESS=registries fails closed.
- Retention: BUILD_LOG_RETENTION_DAYS, log deletion, housekeeping_records retention;
  line timestamps local on page, UTC download (docs/08,15).
- Activity: crash/visits/App output, PREVIEW_IDLE_SLEEP/wake, private forwardAuth.
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
- docs/26: staging ready; production needs host/backups/domains/ACME, hosted GitHub
  OAuth/App/webhooks, disposable-PR proof and protected releases. ACME not rerun.
- Regression tracker: 242 cases in 16 test-docs files; only tick recorded proof.
  Open owner decision: install-tailwind.sh curl has no retry/time limit.
