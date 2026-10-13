# Project handoff
Last updated: 2026-10-13

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- Edit technical-docs/inside-previa.md only when the owner asks (last updated 2026-10-12).
- docs/27-roadmap.md: Prompts 1-13 done; Prompt 14 (GitHub access reconciliation) not started.
- Flat features: internal/<feature>, wiring: internal/app; web/ has no backend imports.
  Preserve signal theme, modal confirmations and server-rendered GET/POST fallbacks.

## Run and configuration
- Go 1.26.9+; make build/test/lint/vuln, river-ui (jobs, :8090), observability-check.
  Lint at 0 (gocognit 30, nestif, gocritic, revive): split functions, don't raise limits.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 29 and River 007.
  Dev DB last checked at 29 on 2026-10-13; refuses older schemas. make egress-image/router-up prepare Docker.
- /livez, /readyz, /metrics: internal listeners :9090/:9091; healthcheck/check-config.
- Real suites: make test-builder, test-runtime, test-routing, test-housekeeping,
  test-e2e. Browser: UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser;
  UI_TEST_PATTERN narrows cases. Make loads .env; direct Go commands need exports.
- Release: make release-images / prod-smoke PREVIA_VERSION=x (smoke needs age).
- WSL: use absolute paths; Docker pulls may need empty DOCKER_CONFIG. .env holds
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
- Committed ea08ddf: 1411 Go tests, 51 browser cases, build/vet/lint and real Docker
  follow/redaction/sleep/wake passed; queue failures reproduced before the fixes.

## Repository compatibility (2026-10-12, committed 2f7fdb7; migration 0028)
- Finished Claude's partial build-secret changes and the owner's compatibility list.
  Secrets default runtime-only; Allow opens an owner-only CSRF-protected modal.
  Provider.ForBuild + runtime recheck + BuildKit session gates withhold all credentials
  from forks/unknown heads. REGISTRY_AUTH supplies pull logins only; no app injection.
  Mounted values and registry tokens redact build logs and returned errors (docs/07,09).
- Conflicting fork flags/matching IDs use an unverified cache scope. Retry image
  reuse checks both repository IDs; unit and real BuildKit regressions passed.
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
- Prior: 1406 Go tests, 49 browser, lint, real BuildKit/MySQL/E2E passed; registry pull untested.
- Queue tests isolate River schemas from dev workers; basic E2E uses BusyBox httpd. Follow-up fixes committed 2cb0ae1.

## Earlier improvements (committed 973907f / 93191aa; migrations 0022-0027)
- Forks never auto-build (LockIntentPR + ClaimBuildIntent fence; docs/16).
- Fairness: BUILDS_PER_ACCOUNT, BUILD_MINUTES_PER_ACCOUNT_DAY, account/quota waits,
  waiting_since, wakeNextWaitingTx; PREVIEW_MAX_PER_ACCOUNT and startup capacity
  warning (docs/17). Production example sized for VPS-2.
- Build traffic: egress meter, totals/metrics; BUILD_EGRESS=registries fails closed.
- Retention: BUILD_LOG_RETENTION_DAYS, log deletion, housekeeping_records retention;
  line timestamps local on page, UTC download (docs/08,15).
- Activity: crash/visits/App output, PREVIEW_IDLE_SLEEP/wake, private forwardAuth.

## Routing, merge recovery and TLS (2026-10-13; uncommitted)
- Missing PREVIEW_ACCESS_URL caused activation 112/120 failures; 0029/UI/POST require configured routing workers.
- Locked AcceptRoute rejects missing auth; private_preview_unconfigured stays permanent if sync fails; startup warns.
- Sync refreshes same-version settings and restores persisted preview networks after router down/up; older tables stay fenced. River snoozes lease/backoff waits.
- WSL forwarded Go's dual-stack IPv6 listener only on Windows ::1; explicit IPv4 binds now use tcp4 (IPv6 preserved).
- Local .env binds 0.0.0.0:8080 and sets http://host.docker.internal:8080/previews/auth; Air loaded the fixed server.
- Missed merge delivery failed HTTP 502 at the dev tunnel; rebuilt PR retained a teardown marker, hiding it from reconciliation and misreporting removal. New owner intents clear the marker atomically; live-work confirmation no longer excludes legacy markers.
- Live PR 61 is merged, environment 58 torn_down, route removed (HTTPS 302 unavailable); PR 129 remains running and HTTPS 200. Worker heartbeat + /readyz verified after the limit interruption; all capabilities ready.
- Staging certificate replaced by trusted production CA + /acme/acme-production.json; same previously working hostname, historical TLS/browser state unknown.
- ACME_STORAGE_FILE (local Compose) preserves staging state; router-up restores preview networks after recreation.
- Proof: 1424 Go tests/81 packages, 39 opt-in skips, 8 real Traefik; build/vet/lint passed. Regressions reproduced before fixes; prior UI proof: 52 browser cases.
- docs/02,12,13,15 cover recovery/setup; make test-routing includes TestRealPrivateRouting. No migration beyond 0029; server reload + worker restart needed for code fixes. Expired quick tunnel replaced with outside-covered-soundtrack-royalty.trycloudflare.com; existing App webhook updated, signed ping 204, tunnel ready (session 10708). Keep tunnel running; a new URL requires updating the App.

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
- Regression tracker: 242 cases/16 files; only tick proof. Tailwind curl retry/time limit undecided.
