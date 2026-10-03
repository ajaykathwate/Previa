# Next feature implementation prompts

## Where the project stands

Prepared on 2026-10-03 against the current repository. Verify this summary against
`HANDOFF.md`, Git status and the relevant code before executing any prompt.

- Implemented: PostgreSQL/migrations, split process config, HTTP server, sessions,
  GitHub OAuth, verified App installation linking and cached installation tokens.
- Implemented UI: home, Overview, ownership-scoped repository search/access tabs
  and ten-row pagination, shared layout/theme, logout modal and diagnostic tools.
- Implemented behind tested interfaces: generation-fenced build/teardown,
  durable publication operations, cleanup and reconciliation contracts.
- Production River worker executes ping only. Real preview builds, webhook HTTP
  ingress, PR browsing and Docker/router/GitHub publication adapters are unbuilt.
- Accepted but unimplemented: Manual default per repository; owner-enabled Automatic
  with a five-minute trailing quiet period. Comments/reviews do not trigger builds.
- Existing `webhooks.Service.ProcessPR` enqueues unconditionally, and
  `environments.finishJob` schedules newer generations unconditionally. Both must
  become intent-aware before real builds are connected.

## Recommended next phase

Build the **configuration, policy and PR control plane** in the five steps below.
This closes the scheduling correctness gap before adding expensive execution.
These are sequential, copy-ready implementation prompts, not instructions to
execute all five at once. Each numbered prompt is one task with its own stop point.

| Order | Deliverable | Depends on |
| --- | --- | --- |
| 1 | Standalone repo config fetch/parse/validation | Existing App authentication |
| 2 | Repository policy and durable authorized request model | Accepted build policy |
| 3 | Debounce, manual request coordination and intent-aware handoff | Prompt 2 |
| 4 | Authenticated, durable GitHub webhook ingress | Prompt 3 |
| 5 | Existing-PR discovery, repository/PR pages and owner controls | Prompts 1–4 |

No prompt in this phase implements Docker builds, creates a real preview URL or
registers fake production build workers. Production admission/dispatch must fail
closed until real execution dependencies exist. Record observation and policy safely;
do not accept a browser build action and misleadingly claim it can execute.

## Instructions applying to every prompt

Read `AGENTS.md`, `HANDOFF.md` and Git status first. Follow README/DESIGN and the
approved decision docs. Read the named sections and needed interfaces, rather than
rescanning the repository. Preserve unrelated/uncommitted changes; do not commit
or push unless asked.

Before code changes, briefly confirm the prompt fits the current architecture and
explain the package, schema, process and UI changes it will make. Do not silently
decide consequential unresolved behavior: identify the choice and obtain the
user's answer before dependent implementation. Continue independent work where safe.
Executing a prompt authorizes its stated scope; drafting this file does not authorize
feature implementation or resolve the decisions listed below.

Keep backend features flat under `internal/<feature>` and construction in
`internal/app`; presentation belongs under `web/pages`, `web/layouts` and
`web/components`. Preserve existing palette, fonts, focus style, sticky navigation,
table scrolling and progressive HTMX patterns. Use small consumer-defined
interfaces, context deadlines and safe errors; do not add speculative frameworks.

Keep observed PR generation, authorized build intent and actually deployed SHA
distinct. Preserve atomic generation checks, external calls outside DB transactions,
durable operation recovery and ownership-safe cleanup. Keep one environment per
PR once work is requested, current-build logs only, and no historical builds table.

For each implementation, run generation when needed, Go build/tests/vet/lint and
relevant PostgreSQL integration tests. Run browser regressions for UI changes.
Use isolated schemas/fixtures; do not reset application data. Report unavailable
checks honestly. Do not claim real GitHub success from mock tests.

Update relevant README/DESIGN sections only where the implemented contract changes,
plus development setup/config documentation, code navigation, TEMP and HANDOFF
(under 100 lines). Never put tokens, private keys or secrets in docs/test output.
End with actual verification, local testing steps and the exact scope still unbuilt.

## Prompt 1 — Standalone repository configuration

> Read DESIGN.md Section 5 in full, README.md Section 2.3's `.previewrc.yaml`
> example, and the existing `internal/platform/githubapp` authentication boundary.
> Implement `internal/repoconfig` as a standalone feature. No webhook, job, UI or
> environment wiring in this task.
>
> Before implementing the public YAML contract, resolve the current mismatch:
> README requires Dockerfile/port, while DESIGN permits defaults and the earlier
> feature request asks for defaults. Propose the exact schema/defaults and confirm
> them. Proposed starting values—not already approved—are `Dockerfile`, port 3000
> and a platform ceiling of 2 vCPU / 2 GiB per preview. Resource tier names/mappings
> and whether numeric resource requests exist must be explicit; do not invent a
> second competing resource schema. Missing optional health path must not silently
> require an endpoint the app may not serve.
>
> Implement a typed config matching the agreed YAML: Dockerfile path, application
> port, non-secret env entries, optional health path and resource declaration.
> Preserve the existing `env: [NAME=value]` shape unless a change is agreed.
> Reject unknown keys, duplicate/ambiguous keys and invalid types. Validate port
> range, environment names and Dockerfile paths, rejecting traversal and absolute
> paths. Lexical path checks do not replace later source extraction/symlink safety;
> document that remaining boundary instead of claiming all filesystem risks solved.
>
> Fetch `.previewrc.yaml` at an exact commit SHA with an installation-authenticated
> GitHub client through a small consumer interface. Reuse existing token caching;
> do not introduce another JWT/token implementation. Bound response size/time.
> Distinguish missing file from access denial, rate limit and transient API failure.
>
> Apply agreed defaults and clamp requested resources to agreed platform maxima,
> returning effective values and any useful clamp notice. Reject malformed or
> nonsensical resources. Return structured, field-specific errors safe for future
> PR comments; omit sensitive env values and provider credentials.
> Produce a deterministic hash of the effective config; do not persist it yet.
>
> Use the DESIGN libraries, pin compatible versions and explain any necessary
> deviation first. Add YAML fixtures covering valid/minimal config, missing file,
> unknown keys, duplicate keys, path traversal/absolute path, invalid port/env name,
> malformed resources and over-ceiling clamping. Test exact-SHA fetching, bounded
> input, safe API error classification and stable effective-config hashing.
>
> Stop after this standalone feature. Show fixture cases, test results, confirmed
> defaults and maximum resource values. Update config-format docs to the agreed
> contract; no GitHub comment posting or build execution.

## Prompt 2 — Repository policy and durable build intent

> Read `docs/BUILD_POLICY_AND_USER_FLOW.md` in full and DESIGN.md Sections 7 and 13.
> Inspect current PR/event queries, environment job arguments and publication
> generation guards. Implement only the persistence/domain foundation for
> repository policy and authorized work; no live webhook, browser controls or
> production build workers yet.
>
> First propose the minimum schema and state transitions. Confirm unresolved choices
> before coding: repeated requests for the same SHA, explicit retry after completion,
> request revision/idempotency, and treatment of an already-running build when the
> owner switches to Manual. The proposed behavior is to cancel unstarted automatic
> work and let executing work finish under its generation guards, but that latter
> choice is not yet approved. Do not overload observed generation to fake a webhook
> whenever a user clicks Build.
>
> Add additive Goose migrations and sqlc queries for owner-controlled repository
> mode (`manual` by default, including existing repositories) and the minimum durable
> request state required by the agreed design: PR identity, requested generation/SHA,
> request source/revision, eligibility deadline and lifecycle information. Store only
> current coordination state, not a historical builds archive. Define indexes,
> constraints and transaction/lock ordering; preserve existing data and policies
> through repository relinking/upserts.
>
> Separate observed PR state from authorized work. Observing an open PR in Manual
> creates no build intent and no environment merely for listing. Closed/merged state
> invalidates build intent and permits teardown in either mode. Enabling Automatic
> applies to subsequent events, not all existing PRs. Switching to Manual cancels
> unstarted automatic intent without removing an existing preview.
>
> Expose small ownership-scoped service/repository methods for policy and requests.
> Make concurrency and idempotency guarantees explicit. Keep the existing deployed
> fields, pending publication state and result generation fence intact. Determine
> whether repeated same-generation requests need an additional request fence across
> publication/reporting/cleanup; do not assume PR generation alone distinguishes
> separate attempts for the same commit.
>
> Test migration defaults, mode validation, inaccessible/foreign repository rejection,
> policy preservation on relink, duplicate request behavior, rollback and concurrent
> request/mode changes. Include the agreed same-SHA retry fence behavior.
>
> Stop with tested persistence/domain APIs. Do not leave half-wired code calling the
> old enqueue path under new policy assumptions. Explain the schema, atomicity and
> what Prompt 3 must change before this policy can control scheduling.

## Prompt 3 — Durable scheduling and intent-aware worker handoff

> Continue from Prompt 2. Read DESIGN.md Sections 6, 7 and 13, and the build-policy,
> generation-ordering and durable-publication docs. Implement coordination using
> parsed events/direct tests; do not add HTTP webhook ingress or Docker adapters.
>
> Adapt `webhooks.Service.ProcessPR` for opened, reopened, synchronize and closed/
> merged state. Record each successfully processed relevant event's generation
> exactly once. Manual tracks state without automatic builds. Automatic persists a
> trailing five-minute deadline; newer relevant head changes replace/reset pending
> intent. Comments/reviews do nothing. Close/merge invalidates pending builds and
> records immediate teardown intent, without debounce.
>
> Implement explicit per-PR manual request coordination against current state. It
> bypasses the quiet period, coalesces matching automatic work and follows Prompt 2's
> duplicate/retry semantics. Recheck owner access, installation status and PR state.
> Persist intent and River wake-up atomically. Use durable delayed wake-ups, not
> process-local timers or sleeping workers; stale wake-ups exit cleanly. Use an
> injectable clock so timing tests do not sleep five minutes.
>
> Change broker admission, `finishJob`, terminal-error handoff and applicable retry/
> recovery paths to consult authorized eligible intent. New observed generation
> alone must not create a successor. Respect future deadlines even when an older
> job finishes sooner; keep one active build/teardown per PR and do not lose close
> intent to uniqueness conflicts. Check request cancellation/access again at worker
> start. Preserve atomic generation/request fencing, durable cleanup and publication.
>
> Review River scheduled-job uniqueness carefully: repeated deadline resets must
> neither lose the latest wake-up nor monopolize the build/teardown PR uniqueness
> slot for the entire debounce window. Explain the chosen job-key and handoff scheme.
> Add only job kinds needed for this coordinator, with sensible per-kind timeout
> and maintenance registration. Keep scheduling distinct from real execution.
>
> Define a fail-closed production dispatch boundary while runtime/publication
> dependencies are absent. Never insert executable production build/teardown jobs
> with no registered capable worker or complete them using no-op adapters. Prove
> dispatch with test adapters; preserve live ping and worker startup without OAuth.
>
> Test Manual opened/synchronize no-build; Automatic deadline reset; old wake-up
> rejection; manual override; duplicate request; switch to Manual before start;
> terminal failure/success handoff; restart/due-work recovery; access revocation;
> and build in flight → PR closes → late result cannot publish. Add rollback and
> concurrent event/request/deadline tests, including both relevant lock orders.
>
> Stop with working coordination tests. Explain atomic eligibility/claim SQL and
> successor logic. Clearly distinguish tested adapters from production capability.

## Prompt 4 — Authenticated and durable GitHub webhook ingress

> Continue from Prompt 3. Read DESIGN.md Section 6 in full, Section 4's lifecycle
> contract, and existing installation/event handler interfaces. Implement the
> `POST /webhooks/github` HTTP adapter with feature-owned handlers/routes and thin
> app wiring. No image builds, router integration or PR comment publishing.
>
> Validate a size-bounded raw body using GitHub's signature verification facilities
> before parsing. Require the expected signature algorithm and reject missing/
> invalid signatures. Use server-only `GITHUB_WEBHOOK_SECRET`; document generation,
> GitHub settings and local delivery testing without exposing secret values.
> Preserve the existing optional App configuration startup behavior deliberately;
> workers must not acquire OAuth/session dependencies. Fail closed when webhook
> authentication is unconfigured rather than accepting unsigned deliveries.
>
> Implement durable delivery-ID deduplication and correct transaction/retry behavior.
> Duplicate successful deliveries must not increment generation or schedule again.
> A crash/error must not mark unprocessed work permanently successful. If direct
> transaction composition is unsafe, durably accept into an inbox and process via
> River; explain the chosen minimal design before adding schema. Return success
> only after durable acceptance, with no source/config fetching or builds inline.
>
> Route pull_request opened/reopened/synchronize/closed through Prompt 3. Map merged
> state correctly. Dispatch installation and installation_repositories payloads to
> existing parsed lifecycle functions, preserving revision/access/ownership fences.
> Acknowledge unsupported valid event types without side effects. Lifecycle cleanup
> remains durable pending work if the real cleanup adapter is absent; do not report
> nonexistent container teardown as completed.
>
> Handle stale/out-of-order deliveries explicitly: processing generation measures
> local acceptance order, not GitHub event chronology. Propose a source-state ordering
> or reconciliation policy for approval; do not silently let an older event revert
> a close or a newer head SHA. Do not run external state fetches under PR locks.
> Carry fork metadata so later execution enforces DESIGN's fork/secrets policy;
> do not expose secrets or invent a new trust policy here.
>
> Test signed fixture bodies, body limit, invalid JSON/signature, unsupported events,
> duplicate/concurrent delivery, transaction rollback/retry, closed versus stale
> synchronize, installation revocation and Manual no-build behavior through HTTP.
> Log safe delivery/event/PR identities, not full payloads, tokens or secrets.
>
> Stop after ingress and durable processing. Give exact local signed-fixture commands
> and real GitHub webhook setup/testing steps. A public HTTPS forwarding tool is
> optional developer tooling, not a new deployed component. Do not claim real
> delivery verification unless actually observed.

## Prompt 5 — Existing PR discovery and repository/PR workspace

> Continue from Prompts 1–4. Read the UI recommendations in
> `docs/BUILD_POLICY_AND_USER_FLOW.md`, DESIGN.md Section 17, the current presentation
> architecture and installation ownership queries. Implement repository/PR browsing
> and owner controls using the existing Templ/HTMX/Tailwind architecture.
>
> First confirm the proposed detail routes and initial-sync execution model. Build
> the smallest feature-owned PR boundary fitting Section 20; do not relocate working
> HTTP handlers globally or import backend feature types into page packages.
> Identify schema additions for PR title, author, branch, URL and source ordering.
> Use GitHub numeric repository identity rather than mutable names as ownership keys.
>
> Add bounded, paginated discovery of existing PRs through installation-authenticated
> GitHub API calls, plus explicit refresh. If using River for initial sync, add only
> the App ID/private key settings genuinely needed by that worker, with no OAuth
> requirement. Narrow the current GitHub App constructor's config dependency where
> needed. Give sync jobs their own timeout/concurrency and honor rate-limit retries.
> Do not fetch every PR inside page rendering or perform GitHub calls under DB locks.
>
> Synchronization is observational and does not bulk-authorize builds, even when a
> repo is Automatic. Fence stale API snapshots against newer webhook updates and
> installation changes; do not infer closure from a partial page or failed fetch.
> Show discovery progress/last refresh honestly. A bounded first page is not “all
> PRs synced”; continue via durable pagination if needed. Later periodic full
> reconciliation remains its own phase.
>
> Add repository detail with searchable, state-filtered PRs (ten per page), and PR
> detail with latest SHA, current request/build state and deployed SHA/preview URL.
> Reuse the table's internal scrolling, live search, ordinary GET fallbacks and
> selected navigation. Keep empty, unavailable, syncing and failed states distinct.
> New unbuilt PRs show Not requested and do not need an environment row.
>
> Add owner-only Manual/Automatic controls using Prompt 3 and CSRF-protected POSTs.
> Show the Automatic quiet-period deadline accurately. Add the per-PR manual build
> control/service boundary, but disable execution clearly while real runtime
> capability is absent; never claim a build has started or queue unsupported jobs.
> Before later enabling execution, verify the PR's current upstream state without
> holding DB locks, and fence that observation against concurrent updates so a
> delayed webhook cannot make the action silently build an obsolete commit.
>
> Populate Builds only with actual current/latest authorized work available from
> the new model; no permanent attempt history. Populate Environments only with real
> deployment state already supported; do not manufacture runtime health/URLs/logs.
> Current log streaming, live health and manual runtime teardown stay unbuilt if
> their adapters are absent. All links should lead to useful scoped pages or
> explicitly unavailable actions, not fake completed functionality.
>
> Test ownership on every query/action, unavailable installations, PR discovery and
> source races, pagination/search, CSRF, duplicate action handling and policy state.
> Run browser regressions for navigation, responsive tables, no-JS forms, modal
> confirmations and preserved theme/focus behavior. No infinite scroll or fake data.
>
> Stop after the PR control-plane UI. Provide click-by-click local checks and list
> what remains before Build can execute a real preview.

## After these five prompts

The next execution phase is still required: exact-SHA source fetching and safe
extraction (Section 8), Docker image building (9), isolated container runtime and
health checks (10), generation-fenced routing/TLS (11), and GitHub feedback (12).
Complete the needed secret/fork handling (16), logs (17) and production recovery/
expiry wiring (15) as those integrations are enabled. Do not enable public preview
execution merely because a manual button and webhook endpoint exist.

Break that execution phase into another reviewed prompt set when the control plane
is verified. Preserve the durable-publication decision; this plan does not replace
its route fencing, retry, cleanup or crash-recovery requirements.
