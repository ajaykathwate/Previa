# Phase 1: Prompts 1–5 audit

Verified 2026-10-03 against the working tree, README, DESIGN and
[FEATURE_IMPLEMENTATION_PROMPTS.md](FEATURE_IMPLEMENTATION_PROMPTS.md).
Existing uncommitted work was preserved. Prompts were reviewed and reported in
order. Phase 2 (master–detail UI) and Phase 3 (open-only PR tracking) have not started;
each requires the user's confirmation after the preceding phase report.

## Status and changes

| Prompt | Initial status | Final status | Findings and changes |
| --- | --- | --- | --- |
| 1 — Repository configuration | Fully implemented | Fully implemented | Standalone exact-SHA installation-authenticated fetch; strict YAML, approved defaults, tier clamping, safe errors and effective hashing. No code fix. |
| 2 — Repository policy and intent | Fully implemented | Fully implemented | Manual default, owner/access checks, relink preservation, one current intent, duplicate coalescing, explicit same-SHA Retry/Rebuild revision and transactional mode cancellation. No code fix. |
| 3 — Scheduling and worker handoff | Partially implemented | Fully implemented within the specified interface boundary | Debounce, admission/handoff/recovery and revision fencing existed. Fixed missing log clearing and lifetime environment uniqueness, including the common requirements and user's explicit audit checks. |
| 4 — Webhook ingress | Fully implemented | Fully implemented | Bounded SHA-256 verification, atomic delivery receipts/effects, source ordering, fork metadata and durable revocation cleanup. No code fix. |
| 5 — PR discovery/workspace | Fully implemented | Fully implemented against its original contract | Explicit initial Refresh, durable paginated discovery, owner-scoped repository/PR pages, CSRF policy controls and disabled build action. No code fix. The requested subsequent layout/discovery changes remain separate phases. |

### Prompt 1 evidence

`go test -count=1 -json ./internal/repoconfig ./internal/platform/githubapp`
passed: 14 config tests and 2 local GitHub-client tests. The same packages also
passed with `-race`. Real GitHub access is opt-in and was skipped.

Fixtures cover valid/minimal YAML, required missing file, unknown/duplicate keys,
traversal/absolute paths, invalid port/env/health path, malformed/unknown resources,
over-ceiling clamping, exact API SHA/authentication, response/input bounds, safe
access/rate/transient errors, cancellation and stable effective hashes.
Defaults are `Dockerfile`, port 3000, empty env, no assumed HTTP health endpoint,
and standard (1 vCPU / 1 GiB). Small is 0.5 vCPU / 512 MiB; large and default
platform ceilings are 2 vCPU / 2 GiB. Lower platform ceilings clamp with notices.

### Prompt 2 evidence

`go test -count=1 -race -json -run '^TestIntent' ./internal/environments`
passed all 12 top-level tests with PostgreSQL enabled. They cover migration
defaults/constraints and rollback/reapply; foreign/revoked access; policy retention
on rename/reinstall; duplicate Build and explicit Retry/Rebuild; same-SHA stale
revision rejection; concurrent requests/mode changes; atomic failure rollback;
Manual observation without environments; and generation-zero compatibility.

Intent is one current row keyed by PR, with separate observed generation and request
revision. Mutations lock installation → repository → PR → intent. State/intent and
River wake-up composition is the Prompt 3 coordinator's responsibility; low-level
persistence APIs do not execute work or bypass the production capability gate.

### Prompt 3 fixes and evidence

- `0009_environment_identity.sql` replaces active-only uniqueness with a lifetime
  unique constraint on `environments.pull_request_id`. Rebuild/reopen reuses the
  existing row and subdomain. The migration preserves data and fails atomically if
  legacy duplicate rows require explicit reconciliation; it never silently deletes
  resource references or logs. Applied successfully to the disposable test database.
- `Repository.claimBuild` clears previous logs in the admission transaction after
  the conditional claim succeeds, before calling the runtime. Failed deletion rolls
  back the claim. Rejected/early/stale/revoked requests preserve logs. An admitted
  runtime retry starts a fresh log too; already-published/pending-candidate replays
  return before claiming and keep their logs.
- sqlc output was regenerated. New `build_logs_test.go` tests the runtime seeing
  empty logs before sequence 0, multiple builds/close/reopen retaining row ID and
  subdomain, generation/revision/cancellation/access/deadline rejection preserving
  logs, and deletion failure leaving queued intent and skipping runtime execution.
- The migration regression now rejects duplicate environment rows in every status,
  including `torn_down`; it previously expected a new row after teardown.

`go test -count=1 -race -json ./internal/environments ./internal/platform/queue
./internal/platform/database` passed: 40 environment, 4 queue and 5 database tests.
The strengthened rebuild/close/reopen test was also rerun independently and passed.

| Required behavior | Executed regression evidence |
| --- | --- |
| Manual opened/synchronize creates no build/environment | `TestSchedulingManualObservationAndClosedTeardown` |
| Trailing five-minute deadline and manual override | `TestSchedulingAutomaticDeadlineResetOldWakeAndOverride` |
| Superseded/early scheduled jobs do no build work | Same test asserts zero build jobs after dispatching old/early wake-ups; `TestSchedulingRiverWakeWorkerAndPRSlot` exercises real River processing |
| Worker generation check before runtime | `TestQueuedOldBuildSkipsExecutionAndCleanupRetries`, `TestRejectedBuildPreservesLogs` |
| Access/mode recheck on start | `TestSchedulingWorkerRechecksRevokedOrCancelledAccess` |
| One executable build/teardown per PR | `TestTransactionAndCrossKindUniquenessIntegration`, `TestSchedulingRiverWakeWorkerAndPRSlot` |
| Success/terminal failure preserves future deadline; Manual has no implicit successor | `TestSchedulingHandoffRespectsFutureDeadlineAndManualNoSuccessor`, `TestGenerationCaptureAndRiverHandoff` |
| Restart/due-work recovery and timeout rescue | `TestSchedulingDueRecoveryAndAccessCancellation`, `TestPerKindTimeoutsAndRescue` |
| Close wins over a late build | `TestCloseWinsOverInflightBuild` |
| Same-SHA revision fences results/publication/cleanup | `TestSchedulingSameGenerationRebuildFencesOperationsAndCleanup`, `TestSchedulingRevisionChangesDuringExternalPublication`, `TestSchedulingAtomicRevisionCheckAfterLockWait` |
| Atomic rollback and concurrent updates | `TestSchedulingManualWakeRollbackAndConcurrentObservation`, intent/ordering concurrency tests |
| One lifetime environment and fresh current logs | `TestMigrationConstraintsIntegration`, all three new log tests |

Wake-up keys include PR/generation/revision and are separate from the executable
PR-only uniqueness key. Dispatch and worker claim use locked current intent,
generation/SHA/revision, policy/access and deadline checks. `finishJob` releases the
old River key and admits only current eligible intent atomically; future work keeps
its durable wake-up. A compiled worker started and drained successfully with only
database and ping concurrency settings, with all GitHub/OAuth/session settings absent.

### Prompt 4 evidence and local commands

Seven top-level signature/lifecycle/signed-delivery/source-ordering tests passed
with `-race` across webhooks, pullrequests and installations. Coverage includes
12 concurrent identical HTTP deliveries producing one receipt/generation, Manual
no-build, changed-payload delivery conflict, rollback/retry, stale close/head events,
ambiguous timestamps, legacy baselines and durable revocation cleanup.

The compiled server rejected an unsigned webhook with 401. Against that server,
`make test-webhook` returned `HTTP 204: signed fixture accepted`, using disposable
credentials and an isolated DB. No live GitHub delivery was claimed.

For local repetition, configure `.env` as described in
[DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md), then run:

```sh
make migrate-up
make dev
# In a second terminal; this target loads .env automatically:
make test-webhook
```

Real GitHub testing requires the same webhook secret on the App, a public HTTPS
receiver URL ending in `/webhooks/github`, and Pull request subscriptions. Open,
push and close a test PR, inspect Recent deliveries, then redeliver and check that
generation/intent does not duplicate. Full steps and stateful fixture commands:
[WEBHOOKS_AND_PR_DISCOVERY.md](WEBHOOKS_AND_PR_DISCOVERY.md).

### Prompt 5 evidence and browser walkthrough

`go test -count=1 -race -json ./internal/pullrequests ./internal/installations`
passed 13 PR-workspace and 7 installation tests. They verify numeric-ID authenticated
API pagination and rate limits, 101 records across durable pages, concurrent refresh
coalescing, partial failure/retry, stale snapshot/access fences, real River continuation,
ownership on reads/actions, ten-row search/pagination, CSRF and policy state.

All 12 existing Chromium scenarios passed, including responsive navigation/tables,
live search with preserved focus, modal focus/dismissal, disabled Build and no-JS
forms/pagination. The opt-in test used `UI_BROWSER_EXECUTABLE` with an existing local
Chromium and its unpacked shared libraries; normal setup is `make browser-install`
then `make test-browser`. Browser fixtures use simulated providers, not real GitHub.

1. Run migrated PostgreSQL, `make dev` and `make run-worker` with App credentials.
2. Sign in → Repositories → connect/select a repository.
3. Click **Refresh pull requests**; reload to inspect progress/completion. Search,
   change state tabs and use Next/Previous; each page has at most ten rows.
4. Open **Build mode**, change/save Manual/Automatic in the modal. Enabling Automatic
   does not build existing PRs; switching to Manual cancels unstarted automatic work.
5. Open a PR; compare latest, requested and deployed SHA. Unrequested PRs say
   **Not requested**. Build remains disabled; direct POSTs cannot queue execution.
6. Open Builds/Environments; they show actual saved state or truthful empty states.

These are the current routes. The extra repository-page navigation hop is exactly
what Phase 2 will replace. Discovery currently uses `state=all`; explicit initial
Refresh and historical closed/merged imports will be changed only in Phase 3.

## Final verification and boundaries

- `make build lint`: generation, `go build ./...`, `go vet ./...` and pinned
  `golangci-lint run` passed, zero lint issues.
- `TEST_DATABASE_URL=... go test -count=1 -json ./...`: 111 top-level tests passed.
  Two opt-in tests skipped: real GitHub access and browser tests; the browser suite
  was separately enabled and passed all 12 scenarios. No database tests skipped.
- `git diff --check`: passed. New behavior has database-backed regressions.
- All DB verification used a disposable database (Goose 9 / River 007), without
  resetting or migrating application data. The disposable DB and its connection file
  were removed after verification. Run `make migrate-up` on the development
  application DB before using the new schema. `.env` loading rules are unchanged:
  run/migration targets load it; direct `go test` needs exported `TEST_DATABASE_URL`.
- No library, architecture, visual-theme or README/DESIGN requirement deviation.
  Lifetime uniqueness/log clearing correct code that contradicted the existing scope.
  README's older implementation-status prose remains stale and is flagged here;
  the user's README PR-scoping edit is reserved for Phase 3.
- Real source extraction, Docker/build/runtime, fork/secrets enforcement, routing,
  GitHub publication, live logs, cleanup/expiry and production recovery scheduling
  remain later execution work. Current tests use runtime/publication adapters;
  they do not prove a real preview deployment. No fake production workers added.
