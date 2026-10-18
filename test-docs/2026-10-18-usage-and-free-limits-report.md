# Workspace usage and finite free limits (F3) · 2026-10-18

## Delivered behavior

The owner delegated the free allowance amounts and confirmed there are no hosted
users. Every workspace receives **300 build minutes per UTC calendar month, one
concurrent build and two active preview slots**. Baselines share the minutes and
concurrency. One build per workspace permits fair sharing of the host; two
previews permit parallel PR review. No paid state, subscription or charge exists.

Migration 0042 starts a new execution ledger and records its cutover. It backfills
active workspaces and grants new workspaces finite entitlements. Historical
account/workspace elapsed-time counters remain labelled legacy technical data;
they are never imported or billed.

Admission reserves at most 60 seconds per physical attempt under the existing
global admission lock and the workspace entitlement lock. Starting execution
rechecks limits and removes any admission delay from accounting. Source, build,
startup, setup, seed and data fallbacks are metered; queue/capacity waits are not.
Unique attempt/segment keys and guarded settlement prevent duplicate consumption.
Execution splits at UTC day/month boundaries and stops at its last committed
reservation deadline even if a database renewal stalls.

Expiry recovery runs at admission and every minute through River. An unstarted
reservation consumes zero; a crashed execution consumes at most its final bounded
reservation and remains labelled provisional. Recovery does not charge the time
until the worker restarts. Late completion cannot add consumption again.

Preview slots count distinct PRs retaining active, sleeping or pending previews,
including newly admitted builds. A same-PR replacement reuses its slot; a baseline
uses no preview slot. Publication rechecks current entitlement, and quota waits
retry without exhausting River attempts. Existing serving previews, review reads,
feedback, account access and teardown remain available.

The **Usage** page shows authorized measurements, consumed/reserved minutes,
period/reset, limits, real blocked work, pending/provisional recovery and the
worker's reported expiry/idle/log-retention policy. Owners/Admins see workspace
totals; Developers see only granted repository technical usage; Reviewers are
denied. Refresh and exact timestamps work without JavaScript. Presentation uses
the existing signal theme.

## Setup and environment

- New binaries require **Goose 42**; River stays **007**. Stop old execution
  workers, run `make migrate-up`, then restart server and workers. No new
  initialization command or environment variable is required.
- Existing `BUILDS_PER_ACCOUNT` and `BUILD_MINUTES_PER_ACCOUNT_DAY` now add
  workspace safety ceilings; zero disables only the extra ceiling. Restart all
  workers together after changing them.
- Tests use the disposable PostgreSQL 17 database on port 55439 and isolated
  schemas. The development database was not migrated by this task.
- Chromium is `/snap/bin/chromium`, driven by the repository's Playwright setup.
  Real system/end-to-end tests use local Docker and fake GitHub services.

## Verification

| Check | Result |
| --- | --- |
| `make build` | Passed (Tailwind, templ, sqlc and `go build ./...`) |
| `make lint` | Passed: `go vet ./...`, golangci-lint **0 issues** |
| `go test -p 4 -count=1 ./...` against PostgreSQL | All 49 test packages passed; packages without test files also compiled |
| `go test -race -count=1 ./internal/usage` | Passed on the final ledger code (88.952 s), no data races |
| Migration 0042 up/down/up in a scratch schema | Covered by `TestMigrationUpDownUpPreservesLegacyHistory`; preserves legacy history and reinstalls the 300-minute entitlement |
| Compute-independent access | Passed: authenticated review read/feedback/account access and teardown with restricted/exhausted compute |
| Final lifecycle/assembled-server regressions | Passed: environments 26.637 s, reviews 5.732 s and app 6.241 s after the replay/attribution fixes |
| `make test-usage-browser` | 4/4 passed (46.322 s) after final wrapping changes, including no JavaScript, role scope and CSP |
| Existing browser suites | Broad UI 55/55 passed (262.527 s); workspace 14/14, setup 5/5, review 6/6 and Test data 3/3 passed; no JavaScript/CSP violations |
| `make test-runtime` | 11/11 passed (350.631 s), including PostgreSQL/MySQL data copying and cancellation |
| `make test-routing` | 8/8 passed (254.549 s), including stable HTTPS, failed swaps, restart and teardown |
| `make test-e2e` | All four journeys passed (945.591 s), with nonzero metering and reconciled period/segment totals |
| Screenshots | Reviewed all eight final Owner/Developer captures at 1440/390 px, light/dark; readable timestamps and repository values, no page overflow |

The real end-to-end harness requires nonzero physical attempts and executed time,
and checks every allowance period against the sum of its execution segments.
The Test data journey accounted 11 physical attempts, including two baseline
attempts, and 250.307945 seconds. Backend/reference/general journeys accounted
4/5/4 attempts and 103.413478/100.440835/30.780901 seconds respectively.
Its large test-only allowance permits the full journeys without changing the
production plan.

## Focused coverage

| Contract | Evidence |
| --- | --- |
| Queue time excluded; failed/retried execution counted once | `TestExecutionCountsOnceExcludesWaitingAndSeparatesRetries` |
| Concurrent reservation cannot overspend; held balances retry before reset | `TestReservationsPreventConcurrentOverspend` |
| Shared PR/baseline concurrency; baseline takes no preview slot | `TestConcurrentBuildsAndPreviewSlotsAreSharedWithBaselines` |
| Bounded crash recovery, zero unstarted time, idempotent late completion | `TestCrashRecoveryIsBoundedIdempotentAndProvisional`, `TestExpiredAdmissionNeverExecutesAndNewAttemptCanProceed` |
| UTC splits and daily/monthly resets | `TestSegmentsSplitAtUTCMonthAndDayBoundaries`, `TestAllowanceExhaustionDailySafetyAndReset` |
| Rechecks before execution/publication | `TestExecutionRechecksEntitlementsAfterAdmissionDelay`, `TestPublicationRechecksLoweredEntitlementAndPreservesExistingPreview` |
| Hard deadline, stalled database, cancellation classification | `TestRunCancelsAtReservedDeadline`, `TestDatabaseStallCannotExtendExecutionReservation`, `TestParentCancellationCannotBecomeSuccessfulExecution` |
| Physical key cannot identify different work | `TestPhysicalKeyCannotBeReusedForDifferentWork` |
| Concurrent replay cannot release the original execution's hold | `TestConcurrentReplayCannotFinishTheRunningAttempt` |
| Sleeping preview retains slot; teardown releases it | `TestPreviewSlotSurvivesSleepAndIsReleasedAfterTeardown` |
| Permission changes and repository scope | `TestOverviewEnforcesWorkspaceAndRepositoryPermissions`, Usage browser suite |
| Repository binding cannot reattribute another workspace's usage | `TestRepositoryRebindingCannotMoveCapturedWorkspaceUsage` |
| Legacy cutover and new-workspace/restart behavior | `TestLegacyUsageIsNotImportedAndBlockedWorkHasReset`, `TestMigrationUpDownUpPreservesLegacyHistory`, `TestWorkerRestartsKeepUsageAndNewWorkspacesReceiveFinitePlan` |
| Complete lifecycle integration | Five `TestWorkspace…` tests in `internal/environments/usage_integration_test.go` |
| Feedback/reads/account access at a limit | `TestExhaustedComputeKeepsReviewReadsFeedbackAndAccountAccess` |

## Corrections found during verification

- Updated an existing intent test fixture's explicit migration list to include
  0042, matching the generated query's new waiting timestamp column. Its historical
  rollback test removes 0042's dependent ledger before rolling back 0030/0006.
- Made an existing baseline checksum-corruption test deterministic: overwriting
  a checksum character with `0` sometimes left it unchanged; it now toggles it.
- Corrected an existing encrypted-stream test that searched random ciphertext
  for a single plaintext byte. That byte can occur by chance; the leakage check
  uses a 32-byte prefix, while round-trip/tamper checks still cover every size.
- Preserved the quota reason ahead of a runtime's generic cancellation code, so
  a reservation stop becomes a retryable wait instead of a terminal build error.
- Restricted failed-start cleanup to unstarted holds. A concurrent duplicate
  invocation cannot settle the original invocation's running reservation.
- Scoped repository breakdowns to the workspace captured on each attempt, so a
  changed installation binding cannot expose or reattribute historical usage.
- Corrected inline templ component text, mobile repository columns and wrapping
  around exact measurement/cutover timestamps during screenshot review.
- Two broad browser runs reached the five-minute process deadline under test
  load; the FAQ scenario passed when rerun alone. The 55-scenario process now has
  ten minutes (Go cleanup twelve), retaining each browser-action deadline and
  every assertion. Final broad-browser evidence is recorded above.

## Manual rows and remaining boundaries

User-guide rows **U1–U10** cover the free plan, role scope, concurrency, replay,
baseline/fallback accounting, preview slots, exhausted allowance, UTC resets,
crash recovery and responsive/no-JavaScript usage. Automated equivalents are in
the focused tests, browser suite and real Docker journeys above. The owner's
hands-on acceptance remains a release step; no owner acceptance is claimed.

Crash instant is unknown after process loss: recovered consumption uses the last
committed upper bound and stays provisional. The crash and clock-boundary tests
are deterministic PostgreSQL tests; this task did not kill a real worker process
or change the host clock. No live GitHub reporting, production deployment,
purchase flow, F4 account deletion/support work, commit or push was performed.
No architecture or visual-theme deviation from README/DESIGN was introduced.
