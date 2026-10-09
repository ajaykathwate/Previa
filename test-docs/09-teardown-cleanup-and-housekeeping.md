# 09 · Teardown, cleanup and housekeeping

**Module:** TDN · **Code:** `internal/environments` (teardown, cleanup),
`internal/housekeeping`, `internal/runtime` (cleanup executor) · **Feature docs:**
[13 · Teardown and cleanup](../docs/13-teardown-and-cleanup.md),
[15 · Housekeeping and capacity](../docs/15-housekeeping-and-capacity.md)

Removing previews when an owner asks, a PR closes, access is revoked or a preview
expires, and finding whatever a crash left behind, without ever touching the
serving preview or anything that is not this instance's.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Teardown | TC-TDN-TD | 5 |
| Cleanup | TC-TDN-CLN | 3 |
| Expiry | TC-TDN-EXP | 2 |
| Orphans and disk | TC-TDN-ORP | 4 |

## 1. Teardown (TC-TDN-TD)

- [x] **TC-TDN-TD-001 · An owner's teardown is confirmed, owner-only and idempotent**  
  Type: Positive / Security · Priority: P1 · Level: Integration + Browser  
  Steps: Open the teardown modal and cancel; confirm; post without CSRF and for another owner's PR; repeat; return to Environments with a crafted return value.  
  Expected: Cancel changes nothing and returns focus; the confirmed teardown records a durable route removal and says "Removing"; CSRF-less and foreign requests are refused; a repeat removes nothing twice; the redirect never leaves the site.  
  Evidence: `internal/pullrequests` › `TestTeardownIsConfirmedOwnerOnlyAndIdempotent`; browser › *owner builds once and tears a preview down only after confirming in a modal*

- [x] **TC-TDN-TD-002 · Closing a PR removes everything it owned**  
  Type: Positive · Priority: P1 · Level: System + End-to-end  
  Steps: Close a PR with a live preview; also tear one down while the router is unavailable.  
  Expected: The route stops serving; containers, images and networks are removed; unrelated containers and other PRs' objects survive; during a router outage the preview stays until the removal converges, then goes; the lifetime environment row is kept.  
  Evidence: `internal/app` › `TestRealEndToEnd`; `internal/routing` › `TestRealRoutingTeardownLeavesNothingButUnrelatedObjects`

- [x] **TC-TDN-TD-003 · Revoked access tears down without GitHub**  
  Type: Security · Priority: P1 · Level: System + Integration  
  Steps: Uninstall, suspend or remove the repository via webhook; run the teardown sweep.  
  Expected: Previews are torn down without any GitHub call; the sweep clears the installation's cleanup marker only when Docker holds nothing more.  
  Evidence: `internal/routing` › `TestRealRoutingRevocationTearsDownWithoutGitHubAccess`; `internal/environments` › `TestRevocationWebhookRequestsTeardownAndSweepClearsTheMarker`; `internal/environments` › `TestSweepKeepsTheMarkerWhileDockerStillHoldsResources`; `internal/environments` › `TestLocalTeardownNeedsNoAccessAndCleansEverything`

- [x] **TC-TDN-TD-004 · Teardowns are idempotent and never hit the wrong generation**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Tear down a PR with nothing live; retry a teardown; let an old teardown finish after a reopen.  
  Expected: Nothing live is a no-op; a retry is idempotent; the replacement preview after a reopen is kept.  
  Evidence: `internal/environments` › `TestRequestTeardownOfAPRWithNothingLiveIsANoOp`; `internal/environments` › `TestRetriedTeardownIsIdempotent`; `internal/environments` › `TestOldTeardownAfterReopenKeepsTheReplacement`

- [x] **TC-TDN-TD-005 · A close while building cleans the candidate**  
  Type: Edge · Priority: P1 · Level: System  
  Steps: Close the PR while its build is running.  
  Expected: The superseded build's candidate is removed; the environment ends torn down.  
  Evidence: `internal/routing` › `TestRealRoutingCloseWhileBuildingCleansTheCandidate`

## 2. Cleanup (TC-TDN-CLN)

- [x] **TC-TDN-CLN-001 · Cleanup keeps whatever the router serves**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL) + Unit  
  Steps: Schedule cleanup of the routed candidate before and after a newer route converges; check a resource against the cleanup fence's rules.  
  Expected: The routed candidate is kept until the newer route is served; the fence refuses other PRs', newer, referenced or still-building resources (11 cases).  
  Evidence: `internal/environments` › `TestCleanupKeepsWhatTheRouterServes`; `internal/environments` › `TestCleanupFenceAllowsOnlyUnreferencedSettledResources`

- [x] **TC-TDN-CLN-002 · Failed cleanup is retried and never given up**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Make cleanup fail; schedule the same resource again; exhaust River attempts.  
  Expected: The same operation is rearmed (no duplicate); idempotent cleanup stays retryable until it succeeds.  
  Evidence: `internal/environments` › `TestFailedCleanupIsRetriedWhenScheduledAgain`; `internal/environments` › `TestIdempotentCleanupIsNeverGivenUp`; `internal/environments` › `TestRescheduledCleanupIsTheSameOperation`

- [x] **TC-TDN-CLN-003 · Operations and their outbox commit or roll back together**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Fail the queue during a state change; fail an external operation transiently.  
  Expected: No state without its wake-up and no orphan outbox row; transient failures retry with persisted backoff and finalize; external publication never holds the PR lock; each operation kind has its own timeout and a stuck one is rescued.  
  Evidence: `internal/environments` › `TestOperationOutboxAndStateRollBackTogether`; `internal/environments` › `TestOperationOutboxRetriesAndFinalizes`; `internal/environments` › `TestExternalPublicationDoesNotHoldGenerationLock`; `internal/environments` › `TestPerKindTimeoutsAndRescue`

## 3. Expiry (TC-TDN-EXP)

- [x] **TC-TDN-EXP-001 · Expired previews are torn down without rebuild loops**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Let a preview pass its lifetime; run expiry twice; run recovery; push a new commit.  
  Expected: Only due previews expire (not ones being rebuilt); nothing rebuilds by itself; a new commit brings it back.  
  Evidence: `internal/housekeeping` › `TestExpiryTearsDownDuePreviewsWithoutRebuildLoops`

- [x] **TC-TDN-EXP-002 · The lifetime starts at publication; dry runs only log**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Publish, fail a rebuild, republish, tear down; run expiry as a dry run; run without a lifetime.  
  Expected: Expiry is set at publication, unchanged by a failed rebuild, restarted by a publication and cleared by teardown; a dry run expires nothing; without a lifetime nothing expires.  
  Evidence: `internal/environments` › `TestPublicationSetsExpiryAndTeardownClearsIt`; `internal/housekeeping` › `TestExpiryDryRunOnlyLogs`; `internal/environments` › `TestWithoutLifetimePreviewsNeverExpire`

## 4. Orphans and disk (TC-TDN-ORP)

- [x] **TC-TDN-ORP-001 · Orphans wait out a grace period; dry runs only log**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Leave an abandoned candidate and objects of an unknown PR; run housekeeping as a dry run, then for real, before and after the grace period.  
  Expected: Young objects are never touched; the dry run schedules nothing; the abandoned candidate and unknown PR's objects are removed after the grace period; the serving preview is kept.  
  Evidence: `internal/housekeeping` › `TestOrphansWaitForTheGracePeriodAndDryRunOnlyLogs`; `internal/housekeeping` › `TestObjectsOfUnknownPRsAreRemovedAfterTheGracePeriod`

- [x] **TC-TDN-ORP-002 · Housekeeping removes only this instance's orphans**  
  Type: Security · Priority: P1 · Level: System (real Docker)  
  Steps: Run housekeeping beside the serving preview and another instance's container.  
  Expected: Orphans go; the serving preview and other instances' objects stay.  
  Evidence: `internal/housekeeping` › `TestRealHousekeepingRemovesOnlyThisInstancesOrphans`; `internal/platform/dockerx` › `TestOwnedByRequiresThisInstance`

- [x] **TC-TDN-ORP-003 · Caches are pruned under disk pressure, never while in use**  
  Type: Resilience · Priority: P1 · Level: System + Integration  
  Steps: Fill the disk with caches and an untagged image; let a build wait for disk.  
  Expected: No pruning without pressure; under pressure the caches of unavailable repositories and untagged images are removed, while caches in use or within budget stay; a build waiting for disk triggers pruning.  
  Evidence: `internal/housekeeping` › `TestRealHousekeepingPrunesUnderDiskPressure`; `internal/housekeeping` › `TestPruneKeepsCachesWithinBudgetAndNeverRemovesInUse`; `internal/housekeeping` › `TestHousekeeperPrunesWhenABuildWaitsForDiskAndRegistersItsJobs`

- [x] **TC-TDN-ORP-004 · Missed closes and vanished previews are reconciled**  
  Type: Resilience · Priority: P1 · Level: Integration + End-to-end  
  Steps: Miss a close webhook; delete the active container outside Previa; leave a crashed candidate's service container behind.  
  Expected: Reconciliation finds the close and tears down once; the vanished preview is recorded as failed without a rebuild; housekeeping removes the crash leftover without touching the live preview; PR reads stay within their interval and outcomes are safe categories.  
  Evidence: `internal/housekeeping` › `TestMissedCloseFoundByReconciliationTearsDown`; `internal/housekeeping` › `TestAVanishedActivePreviewIsRecordedAsFailed`; `internal/housekeeping` › `TestPullRequestsConfirmOnlyLiveWorkWithinTheInterval`; `internal/housekeeping` › `TestOutcomesAreSafeCategories`; `internal/app` › `TestRealBackendEndToEnd`
