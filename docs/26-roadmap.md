# 26 · Roadmap: remaining MVP work

The MVP work, as **12 sequential tasks**. All twelve are done: Previa builds and
serves real previews, is observable and hardened, and is packaged and rehearsed for
production on local staging infrastructure. The release acceptance checklist at
the end is answered item by item in [25](25-release-report.md); its live checks on
hosted GitHub, real DNS and a real host are still pending. Each task is written as
a self-contained prompt (for a developer or a coding agent) with its acceptance
boundary. Run one task at a time with the shared
instructions below. This plan does not certify the current application for
production; the release checklist at the end does.

**Prompt 13** is planned and not started. It extends the MVP so that backend and API
repositories (an app with a database, a cache, migrations and background
workers) preview as well as web frontends do.

## Verified starting point

- Implemented: PostgreSQL/Goose, River, OAuth/SCS/CSRF, verified GitHub App linking,
  installation-token authentication, Manual/Automatic intent and five-minute
  scheduling contracts.
- **Prompt 1 done:** `pullrequests.SyncService.VerifyUpstream` (the
  `environments.UpstreamVerifier` contract) and `internal/source` (exact-SHA archive
  through the base repository, credential-free download, commit identity check,
  hardened extraction, crash-discoverable workspaces, config from the same tree),
  verified against real GitHub with `make test-source`
  ([06](06-source-and-configuration.md)). Called by the Build action and the worker since Prompt 10.
- **Prompt 2 done:** `internal/secrets` (AES-256-GCM keyring with versions and
  `make secrets-reencrypt`, owner Secrets tab, fork withholding, secret-over-YAML
  precedence, `Provider.ForContainer` worker contract, streaming `Redactor`)
  ([07](07-repository-secrets.md)). Containers consume it from Prompt 5.
- **Prompt 3 done:** `internal/buildlogs` (current-attempt streams with an epoch
  reset inside admission, fenced bounded writer with redaction, owner page with live
  SSE resume/reset, stream caps, retention job) and `environments.Attempt` passed to
  `Runtime.Build` ([08](08-build-logs.md)). Every build writes through it since Prompt 10.
- **Prompt 4 done:** `internal/builds` (fresh rootless BuildKit builder per build on a
  private network, Go egress proxy, per-source-repository caches, enforced limits,
  size-capped export, labelled cleanup) verified with `make test-builder`
  ([09](09-image-builds.md)).
- **Prompt 5 done:** `internal/runtime` (hardened unrouted candidates on per-PR
  internal networks, egress only via the shared `previa-egress` proxy, secrets after
  a fresh access/fork recheck, readiness from the proxy container, crash-loop
  detection, host-derived capacity, owner-verified inventory and removal), verified
  with `make test-runtime` ([10](10-preview-runtime.md)). Called by the worker since Prompt 10.
- **Prompt 6 done:** `internal/routing` (owner decisions: Traefik's file provider fed
  by a route controller instead of Docker labels, DESIGN §11 updated; hostnames
  `pr-<n>-<repo id>-<token>`; Pebble for the TLS proof; unknown hosts redirected to a
  dashboard page). Route versions are accepted behind a PostgreSQL (generation,
  revision) fence with removal tombstones, materialized into a forward-only file,
  confirmed through Traefik's loopback API before old artifacts retire; environments
  are provisioned with their hostname at admission; wildcard DNS-01 with staging
  default, secret-file credentials and persistent ACME storage
  (`deploy/traefik/compose.yml`, `make router-up`, `cmd/route-sync`); `__Host-`
  cookies and origin checks. Verified with `make test-routing`
  ([11](11-routing-and-https.md)). Let's Encrypt staging/live issuance is pending a
  domain and DNS token.
- **Prompt 7 done:** teardown for close, revocation (requested in the lifecycle
  transaction, started by `TeardownSweeper`) and future local requests through the
  existing teardown/outbox contracts; access-free locks; route removal before
  owner-verified cleanup (graceful stop, unforced images, idle networks); removal and
  cleanup retried until done; idempotent retries; old teardowns never remove a
  reopened PR's replacement. Verified with real Docker/Traefik in `make test-routing`
  ([12](12-teardown-and-cleanup.md)). Workers, the periodic sweep and the owner's
  teardown action are connected since Prompt 10.
- **Prompt 8 done:** `internal/reporting` renders each PR's persisted state into one
  living comment (stored ID, App-owned marker, bounded discovery for deleted comments
  and ambiguous creates) and one check run per (head SHA, revision) with a stable
  external ID. Every reportable change records a report version and a durable
  `report_status` operation in its own transaction; one leased reporter per PR writes
  the latest state, so stale or duplicate reports cannot remain final. Rate limits,
  permission changes and safe error categories are handled; deployments never wait
  for GitHub. Prompt 10 enabled `WithReports` and registered the reporter
  ([13](13-pr-feedback.md)). The hosted proof (`make test-reporting`) needs an
  authorized disposable PR.
- **Prompt 9 done:** `internal/housekeeping` startup and periodic River jobs (owner
  decisions: `ENV_TTL_HOURS` lifetime from the latest publication, deletion only
  after a 15-minute grace period with `HOUSEKEEPING_DRY_RUN`, PR reconciliation of
  live-work PRs every 15 minutes, wait-and-retry when capacity is full). Recovery of
  wake-ups, leases, exhausted builds, sweeps and route drift; expiry as a teardown
  request (no rebuild loops); orphan inventory with grace periods, unknown PRs and a
  vanished active preview; pruning of untagged images, workspaces and caches within
  `BUILD_CACHE_BUDGET`; global build slots, atomic preview admission counting old
  active plus candidate, a disk check across running builds; `make
  housekeeping-dry-run`. Every Docker object now carries its database's instance
  (migration 0016). Verified with PostgreSQL/River tests and `make test-housekeeping`
  / `make test-runtime` on real Docker ([14](14-housekeeping-and-capacity.md)).
- **Prompt 10 done:** the worker composes the whole pipeline in
  `internal/app/pipeline.go` from validated dependencies only (owner decisions:
  `worker_capabilities` heartbeat in migration 0017; an optional worker secrets
  keyring that fails closed only for repositories with secrets;
  `SOURCE_WORKSPACE_ROOT`; no invented queue positions). Without the App the
  worker runs log retention only; without a preview domain, discovery only; with
  it but without Docker it refuses to start. Per-kind queues and timeouts, a
  30-second drain, interrupted builds that are no failures, permanent failures
  that end after one attempt and a fresh GitHub read at admission. The server
  records requests (`NewRequestBroker`) and never executes. The owner's Build latest
  commit, Build now, Retry build, Rebuild preview and Tear down preview (modal,
  CSRF, owner-only) replace the unavailable endpoint; PR detail, Builds,
  Environments and Overview show real queued/running work, commits, wait and
  failure reasons, preview address, expiry and worker capabilities. Proven by
  `make test-e2e` on real Docker, BuildKit and Traefik with a fake GitHub over TLS,
  plus DB and browser regressions; the capability matrix, proof matrix and the
  remaining live proofs are in [21](21-mvp-operations.md).
- **Prompt 11 done** ([22](22-observability-and-hardening.md), [23](23-incident-runbook.md)):
  correlated, redacted JSON logs; bounded Prometheus metrics with the headline
  request-to-preview latency and scrape-time state exported by the server;
  `/livez`, `/readyz` and `/metrics` on internal listeners with `healthcheck` and
  `check-config` subcommands; a provisioned Grafana dashboard and 24 unit-tested
  alert rules; a nonce Content-Security-Policy and security headers, host and
  trusted-proxy validation, body and header limits, per-client sign-in rate limits
  and a webhook concurrency cap; `APP_ENV=production` checks; the
  `BUILDS_PAUSED` kill switch (migration 0018); govulncheck (Go 1.26.8 and
  containerd 2.3.6 remove every reachable finding); an incident runbook.
- **Prompt 12 done** ([24](24-production-deployment.md), [25](25-release-report.md)):
  reproducible multi-stage release images (distroless, non-root, health checks,
  cross-compiled); production Compose with a migration step, resource limits,
  stop budgets and the router's dashboard route; `cmd/migrate` with embedded
  migrations and a schema check in every process; host setup, encrypted backup and
  restore scripts with a nightly timer; upgrade and rollback procedures; GitHub
  Actions CI and tag-driven releases with GoReleaser; a GitHub-outage phase in
  `make test-e2e`; and `make prod-smoke`, which ran the whole production stack
  locally (43 checks: HTTPS through the router, hardening, metrics, restarts,
  upgrade and rollback, backup and restore, and an older-schema restore).
- Implemented: signed/deduplicated webhook ingress, bounded open-only PR discovery,
  tracked close/merge retention, bounded mergeability checks and build refusal for
  Checking/Conflicts ([05](05-webhooks-and-pr-discovery.md)).
- Implemented: live dashboard updates over SSE from PostgreSQL triggers through
  `internal/platform/realtime` ([19](19-realtime-updates.md)); reuse it for live logs.
- Implemented: repository/PR pages, owner policy controls, build and teardown
  actions, and current Builds/Environments views. The approved presentation and
  no-JavaScript flows exist.
- Existing environment code provides tested admission, generation/revision fences,
  candidate/active artifacts, outbox, cleanup and reconciliation contracts.
  Extend those contracts rather than create a second orchestration system.
- Production worker wiring registers ping, build-log retention, App-configured
  discovery/mergeability and, with a validated preview domain, Docker and router,
  every build, wake, teardown, route, report, cleanup, sweep and housekeeping
  worker ([21](21-mvp-operations.md)). The dashboard reads which capabilities a
  live worker provides and refuses build requests with 503 when none can build.
- Live proofs remain pending: hosted GitHub webhooks and discovery, real source
  through `codeload.github.com`, PR feedback on a real PR, Let's Encrypt
  staging/production over real DNS, the whole pipeline and capacity on a target
  host ([21](21-mvp-operations.md#live-proofs-still-required-before-a-public-rollout)).
  Local fixtures and a real local Git conflict are evidence, but do not prove hosted
  webhook delivery.

Verify this baseline against current code and [HANDOFF.md](../HANDOFF.md) at the
start of each task. Do not rerun old phases or purge retained PR records.

## Shared instructions for every prompt

Read AGENTS.md, HANDOFF.md and Git status first. Read the task-relevant sections of
[README.md](../README.md) and [DESIGN.md](../DESIGN.md). Use DESIGN Section 20's
feature packages and existing stack; its feature layout takes precedence over
README's older illustrative tree. Keep backend files flat within each feature,
presentation in web/, and construction in internal/app. Use small consumer-owned
interfaces; do not create global handler/store layers or speculative frameworks.

Preserve [15-build-policy-and-intents.md](15-build-policy-and-intents.md),
[16-build-scheduling.md](16-build-scheduling.md),
[17-durable-publication-design.md](17-durable-publication-design.md) and
[18-generation-ordering.md](18-generation-ordering.md). Every executable
request and external artifact/operation carries PR identity, exact SHA and the
ordered (generation, revision) fence. Same-SHA Retry/Rebuild changes revision.
Observation/discovery/reconciliation alone never authorizes a Manual build.
Automatic retains its five-minute trailing deadline; enabling it does not
bulk-build existing PRs. Close, merge, revocation and teardown defeat stale work.

Keep external IO outside short PostgreSQL transactions and row locks. Recheck
ownership, access, lifecycle, freshness, mergeability and authorized intent at the
relevant boundaries. Design for duplicate delivery, expired leases, cancellation,
ambiguous external success and process crashes. Persist cleanup/outbox intent;
never substitute a no-op adapter or claim missing-resource cleanup succeeded
without checking it. Credentials stay out of job payloads, URLs, logs and errors.

Preserve the "signal" theme and UI patterns in [20-ui-architecture.md](20-ui-architecture.md),
accessible modal confirmations, CSRF, scoped queries, useful links, bounded lists,
focus-preserving HTMX and ordinary GET/form fallbacks. Show actual work and
deployed state separately. One lifetime environment/current intent per PR;
current-build logs only, with no permanent build history or invented metrics.

Implement only the numbered task. Introduce migrations/configuration only when
needed. Extend .env.example, config loaders, Makefile/Air as appropriate and update
docs/02-development-setup.md in the same task with commands and expected results.
Distinguish Make targets' automatic .env loading from direct commands' manual
exports. Document required server versus worker secrets and restart behavior.

Run gofmt/goimports as appropriate, make build, make test and make lint
(including go build ./..., go vet ./... and pinned golangci-lint run).
Run meaningful targeted tests, PostgreSQL integration tests for persistence, and
browser tests for changed UI. Include real adapter integration tests when adding
an adapter, with explicit prerequisites and isolated, cleaned fixtures. Report
skips/unavailable dependencies and packages without tests honestly.

Stop at each prompt's acceptance boundary. Summarize changes, commands/results,
remaining blockers and any README/DESIGN conflict before changing architecture.
Update HANDOFF.md under 100 lines. Preserve unrelated work; do not commit, push,
publish or deploy unless that task/session authorizes it. External GitHub writes
for live proofs require an explicitly authorized disposable repository.

## Sequence and execution gates

| Prompt | Deliverable | Prerequisites |
| --- | --- | --- |
| 1 ✅ | Fresh upstream verification and safe exact-SHA source | Existing App/config/PR contracts |
| 2 ✅ | Encrypted repository secrets and enforced fork policy | 1 |
| 3 ✅ | Bounded, redacted current-build logs and SSE | 2 |
| 4 ✅ | Isolated BuildKit image builder | 1–3 |
| 5 ✅ | Hardened Docker candidates and readiness | 1–4 |
| 6 ✅ | Stable HTTPS routes and fenced blue/green publication | 5, existing outbox |
| 7 ✅ | Durable teardown, revocation and resource cleanup | 5–6 |
| 8 ✅ | GitHub living comment and exact-SHA checks | 3, 6–7 |
| 9 ✅ | Recovery, expiry, reconciliation and capacity | 4–8 |
| 10 ✅ | Real worker orchestration and completed dashboard actions | 1–9 |
| 11 ✅ | Observability and production hardening | 10 |
| 12 ✅ | Deployment packaging, CI and release acceptance | 1–11 |
| 13 | Backend previews: managed PostgreSQL/Redis, setup command, worker processes, approved outbound connections | 1–12 |

Prompts 1–9 exercised adapters using isolated test harnesses with normal preview
dispatch disabled. Prompt 10 connected the complete pipeline in a controlled
local/staging configuration ([21](21-mvp-operations.md)). Public production rollout requires the
observed release checks in Prompt 12, including isolation, recovery and restore.
Prompt 13 does not depend on the pending live checks; like Prompt 10, it is accepted
on local/staging infrastructure, and it must not weaken any release check that has
already passed.

## Prompt 1 — Fresh PR verification and safe exact-SHA source fetching

> Implement the source-preparation boundary for the remaining MVP. Read DESIGN
> Sections 5, 7, 8 and 13, docs/06-source-and-configuration.md, and the existing PR
> observation/admission and GitHub App clients. Keep normal build execution off.
>
> Add bounded installation-authenticated verification of a tracked PR's current
> lifecycle, head SHA, fork identity and mergeability before manual authorization
> and before executing eligible work. Define the narrow interface now; connect it
> to the real action/worker in Prompt 10. Capture installation/repository revisions
> before IO and reconcile under existing lock order afterward. If upstream is
> unavailable, unresolved, conflicting, closed or inaccessible, fail closed.
> A verification observation is not a new Manual authorization; preserve the
> distinction between event-authorized Automatic work and discovery.
>
> Implement internal/source archive fetching at the requested SHA, using the base
> repository's supported PR source/ref behavior for forks. Never silently fetch a
> branch tip, substitute a different commit or require permission to a private fork
> the App cannot read. Verify the chosen source identity. Apply HTTP timeouts,
> bounded redirects, compressed/uncompressed byte caps, file counts and cancellation.
> Do not forward installation credentials to arbitrary redirect hosts.
>
> Extract into a fresh worker-owned directory. Reject traversal, absolute paths,
> unsafe links, device/FIFO entries and archive expansion abuse; prevent writing
> through a symlink. Parse .previewrc.yaml from that same exact source using the
> existing validator/defaults/clamping/hash. Bound Dockerfile/context paths to the
> extracted tree. Remove directories on failure/cancellation; make leftovers
> discoverable after a crash for later housekeeping.
>
> Test malicious archives, missing/invalid config, inaccessible/fork source, exact
> SHA selection and source/access changes during IO. Stop with a real fetch/prepare
> adapter and safe fixtures; provide an opt-in real-source command and record what
> it actually verified. Do not add image builds or enable the Build button.

## Prompt 2 — Repository secrets and fork trust enforcement

> Implement DESIGN Section 16 in internal/secrets, using existing env_secrets and
> owner-scoped repository queries. Add only required schema changes. Runtime
> integration remains an interface until Prompt 5; normal execution stays off.
>
> Use standard-library AES-GCM with random nonces, authenticated repo/key identity,
> explicit key versions and validated process key configuration. Owners can create,
> replace and delete secrets through CSRF-protected forms; deletion uses an
> accessible modal. Show key names only after saving. Never return plaintext or
> ciphertext through pages, jobs, metrics or routine errors.
>
> Define collision precedence between non-secret YAML env and stored secrets.
> Supply values only in worker memory immediately before container creation.
> Repo secrets are runtime-only for MVP; never send them as Docker build args,
> build context or image layers. Add rotation support using key versions and a
> documented safe re-encryption process; retain old keys until migration completes.
>
> Enforce the existing rule that fork PRs receive no secrets, regardless of mode,
> retries or later approval. Treat missing/ambiguous source identity as untrusted.
> Do not silently introduce optional maintainer approval as an MVP requirement.
> If optional approval is implemented, it is bound to the exact SHA and cannot
> override withholding secrets from forks.
>
> Add streaming-safe redaction for known injected values and platform credentials,
> including values split across write boundaries, before storage or streaming.
> Define bounds and limitations; do not claim redaction prevents untrusted code
> from encoding/exfiltrating a credential it legitimately receives.
>
> Test cross-owner access, CSRF, encrypt/decrypt tamper cases, missing keys, rotation,
> collision rules, fork withholding and redaction boundaries. Stop with secret
> management and tested worker/redaction contracts. Document preview-only
> credential guidance and server/worker key provisioning without real secrets.

## Prompt 3 — Current-build log storage and resumable live logs

> Implement DESIGN Section 17 in internal/buildlogs and web/pages/builds, using
> the current-build-only storage policy. Reuse the existing admitted-build log
> clearing transaction; rejected jobs must retain their previous logs.
>
> Bind each writer to environment, generation and revision so a late old writer
> cannot append to the new attempt's logs. Allocate monotonic sequence IDs safely,
> with bounded batches, line lengths, bytes per build and age retention. Apply
> Prompt 2 redaction before persistence, SSE or diagnostic output; represent
> truncation and storage failures explicitly rather than silently discarding them.
>
> Build on internal/platform/realtime: use realtime.NewSSE with log sequence
> numbers as event ids and a hub topic only as the wake-up for new rows.
> Add owner-scoped initial log reads and SSE with authenticated ownership checks,
> Last-Event-ID resume, bounded replay, heartbeat, cancellation and reconnect.
> Clearing current logs needs an attempt identity/reset signal so an old cursor
> cannot skip a replacement build. Fence late writes during clear/rebuild races.
> Cap concurrent streams and avoid holding a DB transaction/pool slot per viewer.
>
> Add a useful current log page/link, safe HTML escaping, accessible live updates
> and an ordinary reload/paginated fallback without JavaScript. Preserve theme and
> navigation. Show “no build requested”, “queued”, “stream disconnected”, “logs
> truncated” and terminal state accurately. Build/runtime adapters will supply
> real output later; do not populate sample logs as application activity.
>
> Test ownership, reconnect/reset, same-SHA revision changes, late writers,
> truncation, redaction, script-looking output and slow/disconnected clients.
> Run affected browser checks. Stop with a bounded log sink and functioning
> authenticated SSE UI, ready for the real builder.

## Prompt 4 — Isolated BuildKit image builds

> Implement DESIGN Section 9 in internal/builds with shared client construction
> in internal/platform/dockerx only where needed. Consume Prompt 1's prepared
> exact-SHA source and Prompt 3's bounded log sink. Normal dispatch remains off.
>
> Use BuildKit through the accepted Go client or Docker BuildKit-backed API.
> Produce uniquely owned images identified by environment/PR, SHA, generation
> and revision. Retries must discover/reuse or safely remove partial results.
> Scope cache per repository and prevent cross-repo cache contamination.
>
> Make build isolation concrete: untrusted Dockerfile steps cannot reach the
> control plane, Postgres, host services, metadata endpoints or other previews,
> cannot mount the host Docker socket or obtain App/platform credentials, and have
> enforced CPU/memory/process/disk budgets. A timeout alone is not resource
> isolation. Use a constrained builder whose network allows only the documented
> external dependencies needed for builds; test internal destination denial.
> Docker network attachment alone is insufficient evidence of isolation.
>
> Enforce build timeout/cancellation, bounded output, disk low-water admission
> and resulting image-size limits. Do not pass repo secrets to build args/context;
> runtime-only secrets are the MVP boundary. Do not implement a build-time secret
> UI. Never persist a source download token in an image/cache/log.
>
> Persist safe failures and cleanup references through existing contracts, leaving
> an existing active preview untouched. Always close response streams and clean
> source workspaces; later housekeeping must identify leftovers after crashes.
>
> Test real successful/cached builds, malformed Dockerfile, timeout/cancellation,
> excessive output/image size, unique revision identity and isolation probes on
> a disposable builder. Keep unit tests deterministic and integration opt-in.
> Stop with a usable image builder and documented builder prerequisites, capacity
> limits and cleanup; do not start public containers.

## Prompt 5 — Hardened Docker candidates and readiness

> Implement DESIGN Section 10 and README Section 4.6 in internal/runtime.
> Compose source, builder and secrets behind the existing environments.Runtime
> contract; keep lifecycle transactions in internal/environments. A successful
> Build returns a healthy candidate with no public route.
>
> Use the official Docker client, context-aware calls and immutable ownership
> labels on containers and images. Apply clamped resource tiers, CPU/memory/PID
> limits, capability dropping, no-new-privileges and bounded writable tmp space.
> Prefer non-root and read-only rootfs; any compatibility exception must be
> operator-controlled and explicit, never an arbitrary PR-provided privilege.
> Do not mount host paths/socket, allow privileged mode or publish host ports.
>
> Implement and verify host/network rules so previews cannot reach host services,
> server/worker, Postgres, metadata or other previews, including IPv6 and DNS
> resolution to private destinations. Give the router a deliberate path to each
> preview without letting that path become access to the control plane.
> Protect platform processes from aggregate resource exhaustion. gVisor is
> optional under the accepted design; clearly describe the actual isolation level.
>
> Inject authorized repo secrets only at runtime after an access/fork recheck.
> Validate readiness through configured HTTP health path or TCP listening fallback,
> with startup deadline, bounded probes and no arbitrary external probe URLs.
> On failure capture bounded redacted diagnostics and durably clean candidates.
> Detect unexpected exits/crash loops; expose observed health to later recovery/UI.
>
> Implement inventory and idempotent removal by stable IDs and verified ownership.
> Test real healthy/unhealthy apps, limits, non-root/read-only behavior, fork
> withholding, cancellation and network-denial attempts. Test crash discovery
> of partial resources. Stop with an isolated healthy unrouted candidate; no
> Traefik announcement or normal worker dispatch yet.

## Prompt 6 — Stable HTTPS routing and fenced blue/green publication

> Implement DESIGN Sections 11 and 13 and the route boundary in
> docs/17-durable-publication-design.md. Read existing operation leases, candidate/
> active artifacts and finalization SQL before adding internal/routing adapters.
> Preserve Traefik v3; do not replace the router or add a second orchestrator.
>
> Resolve the documented tension explicitly: Docker labels/watch events do not
> supply compare-and-set for (generation, revision), and labels are not freely
> mutable after container creation. Checking DB then writing labels/config is
> insufficient to prove stale publication rejection. Design the smallest durable,
> ordered route controller permitted by the approved publication decision.
> Flag any required provider/architecture change against DESIGN Section 11 before
> implementing it, and update the design consistently. State convergence limits
> honestly; do not promise strict per-request freshness from an unfenced provider.
>
> Allocate globally unique DNS-safe stable hostnames per lifetime environment,
> using immutable repository identity to avoid name collisions/renames. Persist
> accepted route versions including removals so late activations cannot resurrect
> a removed route. Write routes only for healthy accepted candidates, recover
> controller state after restart, and confirm route convergence before retiring
> previous artifacts. Do external IO outside DB locks.
>
> Configure wildcard DNS and HTTPS using Traefik DNS-01, with staging certificate
> tests before real issuance, protected DNS credentials and persistent restrictive
> ACME storage. Separate dashboard and untrusted preview origins/cookies; unknown/
> expired hosts get the specified safe fallback. Expose no public admin API.
> Reconcile Docker socket access with README's worker-only rule; flag any required
> restricted discovery mechanism rather than mounting the raw socket everywhere.
>
> Test real HTTPS routing, stable URLs, successful/failed swaps with the old
> healthy preview retained, delayed stale activate/remove, same-generation
> Rebuild, close during publication and controller/proxy restart recovery.
> Stop with real route adapters and demonstrated fences. Keep normal dispatch off.

## Prompt 7 — Durable teardown and ownership-safe cleanup

> Implement DESIGN Section 14 and lifecycle cleanup using real runtime/routing
> adapters from Prompts 5–6. Reuse existing teardown/outbox/resource-reference
> contracts; do not add a competing teardown queue/state machine.
>
> Close/merge, installation suspension/deletion and repository removal must revoke
> publication and durably remove routes, active/candidate containers, exclusively
> owned image tags and temporary resources. Preserve repo-level secrets unless
> their documented lifecycle requires removal; clear any environment-scoped
> copies. Retain tracked PR and lifetime environment identity for later reopen.
>
> Use bounded graceful stop followed by force removal. Not-found is success only
> for the verified resource. Inspect IDs and labels, protect active/pending/
> operation-referenced artifacts, and never run global Docker prune. Remove images
> only after all references are safe. Do not confuse external cleanup completion
> with a DB state change to torn_down; keep pending/failure work visible.
>
> Resume after any crash or partial failure, even after GitHub App access is
> revoked: local teardown must not require a fresh GitHub token. Reopening/newer
> revisions must not let an old teardown remove the replacement. User-requested
> teardown will use this service in Prompt 10 and must not automatically recreate
> a preview from the same consumed Automatic event.
>
> Test duplicate teardown, route outage, container/image already gone, revocation,
> close while building/publishing, old teardown after reopen and crash after each
> side effect. Run real Docker/Traefik cleanup tests and assert no owned leftovers
> while unrelated containers/images survive. Stop with durable verified cleanup;
> register normal workers only in Prompt 10.

## Prompt 8 — GitHub living comment and exact-SHA check runs

> Implement DESIGN Section 12 in internal/reporting through existing durable
> report operations, installation clients and independent retry budgets.
> Report queued, building, ready, failed, expired and removed states from persisted
> truth. Ready requires accepted routing/deployment, not merely an image/container.
>
> Maintain one living comment per tracked PR using stored ID and a stable marker.
> Recover from a deleted comment and from “create succeeded, DB save failed”
> by bounded discovery of the existing App-owned marked comment before creating
> another. Use exact-SHA check runs with stable external identity; a same-SHA
> Retry/Rebuild must reflect the current revision without duplicate reports.
>
> Preserve current/deployed SHA distinction, latest failure and an older healthy
> URL when appropriate. Link only to functioning owner-authenticated log pages
> and actually published previews. Withheld fork secrets and Checking/Conflicts
> give useful, safe feedback. Do not invent an exact queue position if unavailable.
>
> Serialize/reconcile reporting updates so a delayed response from an old version
> cannot remain the final current status. GitHub APIs do not provide DB-style CAS;
> document transient limits and implement latest-state repair. Honor rate-limit/
> retry-after, permission changes, bounded pagination and safe error categories.
> A reporting outage never rolls back a healthy deployment.
>
> Test duplicate jobs, stale success/failure, deleted comments, ambiguous create,
> same-SHA retries, rate limits and revoked permissions. Supply an opt-in proof
> for an authorized disposable GitHub PR, including resulting comment/check IDs
> and cleanup steps; fixtures alone are not a hosted proof. Stop with reporting
> adapters and required GitHub App permission documentation.

## Prompt 9 — Recovery, expiry, reconciliation and capacity controls

> Implement DESIGN Section 15 in internal/housekeeping, extending existing
> environments.Reconciler and scheduling recovery. Read protected-resource
> references and admission/handoff SQL first. Keep normal dispatch off.
>
> Add bounded River periodic/startup jobs to recover due intents, lost operation
> wake-ups, expired leases, interrupted candidates, stuck builds/teardowns and
> route drift. Inventory real Docker/BuildKit/router resources by platform labels,
> compare with active/pending/operation references and apply grace periods.
> Respect current Manual/Automatic authorization and deadlines; recovery must not
> turn a newer observed SHA or an expired preview into an unauthorized build.
>
> Reconcile tracked PR lifecycle/access with bounded installation-authenticated
> GETs, fenced against concurrent webhook changes. Open-only discovery remains
> open-only; omission never means closed and historical PRs are not backfilled.
> Respect GitHub rate limits and unavailable installations.
>
> Implement configurable expiry with accurate expires_at and user feedback.
> Expiry/manual teardown suppresses recreation until an explicit new Manual
> request or later relevant Automatic event. Define how rebuild affects expiry.
> Prune expired current logs, owned source directories, obsolete images and
> repository-scoped build cache within budgets, without deleting live references.
>
> Enforce global build concurrency, per-repo preview limits and host CPU/memory/
> disk headroom with atomic reservations across retries and crash recovery.
> Count old active plus candidate resources during blue/green swaps. Release
> reservations only after verified cleanup; stale ones reconcile rather than leak.
> Keep cleanup responsive when build capacity is full.
>
> Add dry-run inventory/prune diagnostics, bounded batches and safe operational
> failure visibility. Test restart at every lifecycle boundary, lost jobs,
> exhausted retries, orphan grace periods, expiry without rebuild loops, missed
> close, capacity races and disk pressure using real resources. Stop with tested
> startup/periodic maintenance services, ready for Prompt 10's production wiring.

## Prompt 10 — Connect real workers and complete MVP dashboard actions

> Connect Prompts 1–9 in internal/app and the existing feature handlers. Read
> DESIGN Sections 7, 13 and 17 and docs/03-user-guide.md. This task makes a complete
> pipeline work locally/staging; it does not authorize public production rollout.
>
> Compose the real environments.Runtime and operation executor for routing,
> reporting and cleanup. Register build/teardown/wake/operation/housekeeping
> workers only when their real dependencies are configured and validated. Keep
> server/worker config split: a worker needs no human OAuth/session credentials.
> Make per-kind queues, concurrency, cancellation, lease/rescue timeouts and
> graceful draining match actual work. Missing execution capability remains an
> explicit unavailable state, never a job silently handled by a no-op.
>
> Replace the unavailable manual Build endpoint with upstream freshness checking,
> ownership/access/mergeability gates and durable idempotent intent/wake insertion.
> Connect Automatic event/deadline scheduling end to end. Add explicit Retry after
> failure and Rebuild after success using expected revision. Connecting a repo,
> syncing or enabling Automatic must not bulk-build. Repeat eligibility at worker
> admission and before candidate publication; keep external calls outside locks.
>
> Finish PR detail, Builds, Environments and Overview with real queued/current
> work, latest/requested/deployed SHA, current log links, health, stable preview
> URL, expiry and failure reasons. Add owner-only CSRF-protected teardown with a
> confirmation modal. Preserve list/search/focus/modal live-update guards and no-JS
> flows. Current Builds is not a historical archive; show old serving preview
> separately while a newer request builds or fails.
>
> Prove real source → config → image → healthy candidate → route → GitHub status
> → logs → teardown on isolated fixtures. Test Manual push waits, Automatic bursts
> coalesce, mode switch, duplicate click, same-SHA retry, failed rebuild retains old
> preview, fork withholding, mergeability and access changes, close mid-build,
> restart recovery and cross-owner rejection. Run DB and browser regressions.
> Stop with an operational local/staging MVP and an explicit capability/config
> matrix. Record every remaining live proof required before public rollout.

## Prompt 11 — Observability and production security hardening

> Implement DESIGN Section 18 and harden the connected MVP using the accepted
> slog/Prometheus/Grafana stack. Optional tracing remains outside this task.
>
> Add safe correlated logs for delivery, repo/PR/environment, job, SHA and request
> version. Add bounded-cardinality metrics for webhook outcomes, queue wait/depth,
> build duration/outcome, active previews, readiness failures, pending/failed
> operations, cleanup backlog, reconciler repairs, disk/capacity and GitHub rate
> limits. Do not put arbitrary PR IDs, URLs or secrets in metric labels.
> Provide Grafana dashboards and concrete alert rules for actionable failures.
>
> Separate cheap liveness from dependency readiness and expose real worker
> heartbeat/capability. Protect metrics/admin/diagnostic endpoints from public
> access; keep test ping tools deliberately scoped. Bound request/header/body/
> OAuth/network timeouts, pagination, SSE connections and shutdown drain.
> Add ingress abuse controls that preserve webhook retry/dedup behavior.
>
> Verify production HTTPS/Secure-cookie/origin/CSRF behavior, trusted proxy
> handling and forwarded-host validation. Dashboard sessions must never be sent
> to untrusted preview origins. Check secret/key/ACME file permissions, encrypted
> storage, process/container privileges, build/runtime network denial, internal
> listener exposure and disk-pressure admission against the actual deployed path.
> Do not weaken isolation to get a sample app running.
>
> Add dependency/Go vulnerability checks and a practical incident runbook:
> execution kill switch that leaves cleanup working, retry/reconcile pending
> operations, certificate/GitHub outage, disk exhaustion and key rotation.
> Test readiness under outages, bounded metric labels, cancellation/resource
> leaks and security regression cases. Stop with observable, hardened staging;
> clearly report unresolved release blockers rather than claiming readiness.

## Prompt 12 — Production packaging, CI, restore and release acceptance

> Deliver the deployment path for the completed single-host MVP. Read README's
> accepted single VPS/Docker Compose stack, DESIGN Sections 10–11/15/18,
> docs/02-development-setup.md and current deploy files. Prepare deployable artifacts
> and validate them on disposable local/staging infrastructure. Publishing or
> deploying to a real account requires authorization for that target.
>
> Add reproducible multi-stage server/worker images with generated Templ/CSS,
> minimal non-root runtime where compatible, pinned supported dependencies,
> health checks, restart policy, persistent Postgres/ACME/needed controller state,
> graceful stop budgets and CPU/memory/disk reservations. Provide production
> Compose separately from the existing local Postgres-only setup.
> Wire BuildKit, Docker isolation and Traefik exactly as validated earlier.
> The server/previews get no Docker socket; any restricted router discovery
> mechanism must match the documented accepted decision.
>
> Document fresh-host prerequisites, firewall and network rules, dashboard and
> wildcard-preview DNS, DNS-01 provider credentials, GitHub OAuth/App URLs and
> permissions, matching webhook secret, encryption keys, migrations and startup
> order. Keep secrets out of images/Git/CLI arguments; validate complete production
> config before accepting builds. Use staged ACME testing before live issuance.
> Provide the documented MVP host setup script allowed by README Section 4.9;
> Terraform is optional. Keep current config names consistent with the loaders,
> explaining differences from README's illustrative environment variable list.
>
> Provide repeatable upgrade and rollback procedures, explicit migration
> compatibility boundaries and backups of Postgres, encryption key versions and
> essential TLS/controller state. Test restore into a fresh disposable database/
> host and reconcile with actual Docker state before reopening admission.
> Record restore time and data-loss window; a backup script alone is insufficient.
> Schedule nightly pg_dump to protected object storage as required by README
> Section 4.9, with retention and failure alerts; never expose keys or dump contents.
>
> Use GitHub Actions and GoReleaser for versioned releases per README Section 4.9.
> Add CI for generation consistency, build/tests/vet/lint, migrated Postgres
> integration, browser regressions and image build/scan. Keep untrusted fork CI
> away from deployment credentials and privileged production Docker.
> Add a repeatable real-adapter end-to-end suite with cleanup and failure injection.
>
> Execute the release checklist below where prerequisites and authorization
> exist. Write a production deployment document (in its reading-order slot) and
> a release report with exact commands, environment/version evidence,
> passed/failed/skipped checks and outstanding inputs. Update setup/handoff links. Missing domain, DNS token,
> disposable GitHub repo/write authorization or VPS access stays a specific
> pending gate; do not fabricate successful verification or silently deploy.
> Stop with reviewable deployable artifacts and an evidence-based readiness
> verdict. Production-ready is conditional on all mandatory checks actually passing.

## Prompt 13 — Backend previews: managed services, setup, processes and outbound connections

> Make backend and API repositories as previewable as web frontends. Today a preview
> is one container whose only outbound route is HTTP(S) on ports 80/443 through the
> egress proxy. An app that needs PostgreSQL, Redis, migrations, seed data or a
> background worker therefore cannot run. Read README Sections 2.3 and 2.5, DESIGN
> Sections 5, 9–11 and 13–18, and docs/06, 10, 11, 12, 14 and 17 before designing.
> This task narrows README 2.5's exclusion of multi-service apps. Flag that change,
> then update README and DESIGN consistently in the same task. Arbitrary Compose
> files, images chosen in a PR, and buildpacks stay out of scope.
>
> Add platform-managed backing services that a commit declares in .previewrc.yaml.
> Support only PostgreSQL and Redis, at versions from an operator-pinned catalog:
> images pinned by digest in worker configuration, never chosen in PR YAML. Extend
> the existing validator, defaults, clamping and config hash. An unknown service or
> version fails preparation with a clear message. Each service belongs to its
> candidate, so every build revision creates its own. Each service:
> - is attached to that PR's internal network only, with no egress and no route;
> - carries ownership labels and credentials generated for that candidate;
> - has resource limits that count toward preview admission and host capacity;
> - uses bounded storage that cannot fill the host disk.
>
> A failed rebuild must leave the old preview and its data serving. State plainly
> that data does not survive a rebuild. Harden service containers the way previews
> are hardened where the official images allow it, and document every
> operator-controlled exception.
>
> Inject connection settings into the app and its processes: DATABASE_URL and the
> PG* variables for PostgreSQL, REDIS_URL for Redis. Previa's values override YAML
> and secrets, with a notice like the existing precedence notices. Register the
> generated credentials with the redactor. Keep them out of job payloads, logs,
> metrics, routine errors and plaintext storage.
>
> Add an optional setup command in argv form, for migrations and seed data. It runs
> once per candidate, in a one-off container from the built image, after services are
> ready and before the app starts. It gets the preview's hardening and a timeout, and
> its output goes to the current build log. Its failure fails the build with its own
> reason. Add a small, capped list of unrouted processes from the same image, such
> as a queue worker; the candidate is healthy only while they keep running. Add an
> optional docs path (for example /docs) that the PR comment and dashboard link to;
> it is a path on the preview host, never an external URL.
>
> Readiness runs in this order. Services pass a protocol-level check (a bare TCP
> accept is not enough), then setup runs, then processes must be running, then the
> existing app readiness check runs. Only after all of these may the route be
> accepted. Treat services, setup containers, processes and their storage as
> candidate/active artifacts in these paths, through the existing
> resource-reference and cleanup contracts rather than a second orchestrator:
> - blue/green publication and the (generation, revision) fences;
> - teardown and expiry;
> - recovery, orphan inventory and pruning.
>
> A crash after any side effect must leave nothing owned behind.
>
> Add owner-approved outbound connections for apps that use an external database or
> message broker. Each repository has a list of host:port destinations that its
> owner manages in the dashboard: owner-scoped, CSRF-protected, with a modal
> confirmation before removal, and never set in PR-controlled YAML. An operator
> setting limits the allowed ports and can turn the feature off. The egress
> component forwards only to the destinations approved for the connecting preview's
> repository. It identifies that repository by the preview's network, never by
> anything the preview sends. It resolves names itself and refuses private,
> loopback, link-local, metadata and other special ranges, so DNS tricks fail. Apps
> keep using the real hostname, so TLS verification still works. Bound connections
> and bytes, and add metrics with bounded labels plus logs. Fork PRs get managed
> services but no approved outbound connections, the same rule as secrets.
>
> Show service, setup and process state on the PR page, in the build log and in the
> PR comment. If something is unavailable (for example, no service catalog is
> configured), label it rather than hiding it. Extend .env.example, the config
> loaders and migrations (only where needed), and update docs/02 and the user guide.
> In technical-docs, update the user guide, manual checklist, tech stack, database
> schema and Mermaid diagrams. Add a backend previews document in its reading-order
> slot, with .previewrc.yaml examples for common backend stacks.
>
> Test with a real backend fixture: an HTTP API that uses PostgreSQL and Redis, has a
> migration-and-seed setup command and one queue worker. Run it in make test-e2e on
> real Docker, BuildKit and Traefik and prove that:
> - it builds, becomes ready and serves over HTTPS;
> - a rebuild gets a fresh database;
> - a failed migration keeps the old preview serving;
> - teardown and crash recovery leave no owned containers, volumes or networks;
> - services cannot reach the internet, the host, the control plane or other
>   previews, and other previews cannot reach the services;
> - approved destinations work, while unapproved, private and DNS-rebinding
>   destinations fail;
> - fork PRs get no outbound connections.
>
> Add unit, PostgreSQL and browser tests for the config, the approval list and the
> new UI. Stop when the backend fixture previews end to end and the limits are
> documented. Do not add arbitrary Compose, custom service images, data that
> persists across rebuilds, database copies from production, or non-HTTP ingress
> (gRPC or raw TCP).

## Release acceptance checklist

The final report must give evidence for every item, using isolated resources and
an authorized disposable GitHub repository for external writes:

- Fresh setup and clean migration/startup work from documented commands; server
  and worker signal real readiness/capability and fail closed on incomplete config.
- Real hosted OAuth/App installation and signed GitHub delivery work. Initial
  discovery contains only open PRs; a tracked close/merge retains row identity.
  Real conflicting and unresolved PRs cannot execute in either mode.
- Manual builds the freshly verified exact SHA; later pushes alone do not build.
  Automatic bursts deploy the latest eligible SHA after five quiet minutes.
  Retry/Rebuild revisions fence old work and duplicate submissions coalesce.
- A real Dockerfile-only app builds, becomes healthy, serves a stable valid HTTPS
  preview, streams redacted current logs and has one living comment/valid check.
  Secrets work for trusted source and are absent for forks.
- Successful rebuild swaps without an avoidable serving gap; failed rebuild keeps
  the old healthy deployment. Delayed stale route/report/cleanup operations and
  worker crashes cannot leave the final state on an obsolete version.
- Close/merge, owner teardown, expiry and access revocation remove routes/resources
  durably; reopen follows policy. No automatic recreation loop or owned orphan
  remains after recovery; unrelated Docker resources survive.
- Runtime/build probes cannot reach host/control-plane/Postgres/metadata/other
  previews. Limits and concurrent capacity races cannot exhaust the host.
  Preview origins cannot receive dashboard cookies or platform credentials.
- Browser checks cover owner isolation, CSRF, modals, keyboard/mobile navigation,
  logs reconnect/reset, useful empty/failure states and no-JavaScript workflows.
- GitHub/proxy/DB outages, disk pressure and abrupt worker restarts recover without
  losing authorized intent or cleanup. Operators can see and act on failures.
- Production images/Compose/CI validate; wildcard TLS is verified; backup/restore
  and an upgrade/rollback rehearsal pass on fresh disposable infrastructure.

MVP exclusions remain unchanged: no multi-service Compose apps, buildpacks,
granular team roles, custom per-repository domains, horizontal workers or permanent
build-history archive. Prompt 13 narrows only the first exclusion. It adds
platform-managed PostgreSQL and Redis and processes from the same image, but still
no arbitrary Compose. This plan introduces no implemented architecture/theme
change; routing/socket tensions must be resolved against the approved decisions
and documented during their implementation.
