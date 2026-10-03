# Project handoff
Last updated: 2026-10-03

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/DEVELOPMENT_SETUP.md, .env.example, Makefile and deploy/docker-compose.yml:
  setup/configuration. TEMP.md records verification and remaining integration work.
- docs/FEATURE_IMPLEMENTATION_PROMPTS.md: Prompts 1–3 complete; next is Prompt 4
  (authenticated durable webhook ingress), then Prompt 5 (PR discovery/UI).
- docs/REPO_CONFIGURATION.md: approved YAML defaults, named tiers and errors.
- docs/BUILD_INTENTS.md: approved Manual/Automatic, coalescing, Retry/Rebuild and mode rules.
- docs/BUILD_SCHEDULING.md: coordinator APIs, exact claim/result SQL, wake uniqueness,
  revision fencing, handoff/recovery and fail-closed production boundary.
- docs/GENERATION_ORDERING.md and docs/DESIGN_DURABLE_PUBLICATION.md:
  ordering/outbox consistency, external-adapter contracts and tests.
- Backend features remain flat under internal/<feature>; construction in internal/app.
  UI lives in web/; see docs/UI_ARCHITECTURE.md. No global layered backend folders.
- RTK instructions read, but rtk binary unavailable; standard shell commands used.

## Run and verify
- Pinned toolchain/dependencies. make generate/build/test/lint wrap templ/sqlc,
  Go build/tests/vet/golangci-lint. Browser checks optional for UI changes.
- PostgreSQL 17 via Compose. make migrate-up applies Goose then River.
  Current schema: Goose 7; River 007 (River v0.47.0).
- Credentials use DATABASE_URL environment (Goose internally GOOSE_DBSTRING), no CLI flags.
- TEST_DATABASE_URL enables DB tests; environment tests use/drop isolated schemas.
- make run-server/run-worker load .env with exported overrides. make dev reloads
  server; restart worker after changes. make watch-templates polls templates only.
- SharedConfig: DATABASE_URL. ServerConfig: HTTP/OAuth/session and optional
  all-or-nothing GitHub App group. WorkerConfig: concurrency and BUILD_TIMEOUT (12m).
  Worker requires no OAuth settings; no new Prompt 3 environment values.
- Server/worker health-check DB at startup; graceful shutdown drains River jobs.

## Completed application surface
- Thin cmd entrypoints; chi request logging/recovery/request-ID middleware.
- Official Tailwind 4.1.18, stable templUI 1.13.2, local HTMX 2.0.11, shared theme/icons.
- GitHub OAuth/users, signed state, rotation, SCS pgxstore, RequireUser and CSRF/logout.
  Sessions: 24h lifetime, 30m idle. OAuth callback: /auth/github/callback on localhost:8080.
- Public home; protected Overview, repositories, build/environment placeholders,
  Developer Tools ping and /protected. Sticky navigation; modal confirmations.
- Repositories: ownership-scoped SQL pagination (10 rows), live search/access tabs,
  table scrolling. Installation linking verified via redirect + App authorization;
  cached installation tokens, repo access/revision fences and parsed lifecycle functions.
- Real App API proof requires credentials/installation; docs/GITHUB_INSTALLATIONS.md.
- repoconfig: exact-SHA fetch/strict validation/hash; defaults/tier contract approved.
- installations.BuildPolicyService and environments.IntentService provide policy/current
  intent APIs. Migration 6 defaults Manual and keeps one intent row per PR, no history.

## Prompt 3 coordination and publication
- webhooks.ProcessPR accepts parsed opened/reopened/synchronize/closed(+merged) events.
  Locks installation → repo → PR, increments generation once, reconciles intent and
  inserts wake-up atomically. Delivery/auth/upstream ordering remains Prompt 4.
- Manual observation creates no automatic work/environment; Automatic has a durable
  five-minute trailing deadline. Close invalidates builds and wakes teardown immediately.
- Broker.RequestBuild commits approved manual intent + wake-up. Repeated Build coalesces;
  explicit Retry after failure/Rebuild after success creates revision + 1 without PR bump.
- WakeWorker uses a separate coordination key/queue, timeout 15s (rescue floor 30s).
  Build/teardown still share one active PR key. Dispatch checks generation/revision/
  access/mode/deadline; marks queued only for a matching returned River job.
- Service claims current eligible intent before runtime. finishJob releases the old
  River key and admits only current eligible successor in one transaction. Future
  deadlines keep wake-ups; a new Manual observation alone never starts a successor.
- RecoverScheduling uses bounded keyset scans and recreates wake-ups. Production
  periodic runner absent. Closed PR completion marker tracks executable handoff,
  not external route removal. No-environment teardown creates no preview row.
- Migration 7 propagates revision through immutable job args, result/pending/active
  fields, operation keys/payloads, finalization/reporting and resource references.
  Generation zero is valid; no fake webhook is created. Legacy unauthorized work fails closed.
- Apply locks ownership scope/PR before guarded SQL to see same-generation revisions
  after a lock wait. Short transactions only; external operations run outside DB locks.
- OperationBroker persists outbox + River wake atomically. OperationWorker leases,
  runs bounded executor, rechecks generation/revision and finalizes. Active/pending
  references and leased operations remain protected; stale external effects require
  adapter fencing/compensation. Inventory cleanup also carries revision bounds.
- NewBroker cannot dispatch executable previews. NewExecutionBroker requires runtime
  and publication dependencies; future consuming processes must register all kinds/queues.
  Production app still registers ping only. No fake build/runtime/router/reporting adapters.
- Reconciler recovers operation wake-ups and schedules orphan cleanup, but no live timer.
  Build timeout uses configured budget + 1m completion grace; teardown 2m + 1m; ping 5s.
- Docker/source/runtime, router fence, GitHub reporting, webhook HTTP and PR UI remain
  unimplemented. Real resources must have revision-specific identities/labels and safe cleanup.

## Verification
- Prompt 3 build/tests/vet/lint pass (0 issues); full DB-backed ./... suite passes.
- All environment/scheduling tests pass under race detector; real River wake/handoff,
  same-generation request/result/publication races, rollback, generation-zero,
  mode/access checks, due recovery and close-during-build covered.
- Worker DB health check/start/graceful stop verified with OAuth/session/App env unset.
- Migration 7 applied locally; rollback/reapply of older migration tested only in an
  isolated schema. Browser/GitHub/Docker checks not run (no UI/external adapter changes).
