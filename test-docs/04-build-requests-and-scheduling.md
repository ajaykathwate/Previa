# 04 · Build requests and scheduling

**Module:** BLD · **Code:** `internal/environments`, `internal/pullrequests` ·
**Feature docs:** [16 · Build policy and intents](../docs/16-build-policy-and-intents.md),
[17 · Build scheduling](../docs/17-build-scheduling.md), [19 · Generation ordering](../docs/19-generation-ordering.md)

From an owner's click (or an Automatic push) to an admitted build: intents,
idempotency, deadlines, build slots, capacity waits and recovery after crashes.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Owner requests | TC-BLD-REQ | 6 |
| Automatic builds | TC-BLD-AUTO | 3 |
| Admission and capacity | TC-BLD-ADM | 5 |
| Ordering and fencing | TC-BLD-ORD | 4 |
| Recovery | TC-BLD-REC | 3 |

## 1. Owner requests (TC-BLD-REQ)

- [x] **TC-BLD-REQ-001 · Build latest commit records one request with a durable wake-up**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Press Build on a PR; inspect the request and its scheduling job.  
  Expected: One request for the newest commit, with a durable wake-up; the button is no longer offered while it is queued.  
  Evidence: `internal/pullrequests` › `TestBuildRetryRebuildAreIdempotentAndRevisionChecked`

- [x] **TC-BLD-REQ-002 · Double submits and replays record one request**  
  Type: Negative · Priority: P1 · Level: Integration + Browser  
  Steps: Submit the same form twice (same idempotency key); replay an older request; double-click Build while the response is slow.  
  Expected: One request; replays create no new work; the browser sends only one request because the button is disabled while it runs.  
  Evidence: `internal/pullrequests` › `TestBuildRetryRebuildAreIdempotentAndRevisionChecked`; `internal/environments` › `TestIntentManualIdempotencyExplicitRetryAndRebuild`; browser › *a slow build request disables its control so a double click sends one request*

- [x] **TC-BLD-REQ-003 · Retry follows a failure, Rebuild a success; stale revisions are refused**  
  Type: Positive / Negative · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Retry after a failure; Rebuild after a success; submit a retry with an outdated expected revision.  
  Expected: Each uses a new revision; Build never implicitly retries or rebuilds; the stale request is refused.  
  Evidence: `internal/pullrequests` › `TestBuildRetryRebuildAreIdempotentAndRevisionChecked`; `internal/environments` › `TestIntentManualIdempotencyExplicitRetryAndRebuild`

- [x] **TC-BLD-REQ-004 · A request builds the commit GitHub reports now**  
  Type: Positive / Negative · Priority: P1 · Level: Integration  
  Steps: Request a build after GitHub moved the head; during a GitHub outage; after the PR closed upstream.  
  Expected: The fresh head is built; an outage refuses without recording intent; a closed PR is refused.  
  Evidence: `internal/pullrequests` › `TestBuildUsesTheCommitGitHubReportsNow`

- [x] **TC-BLD-REQ-005 · Without a worker that can build, Build is shown disabled with the reason**  
  Type: Negative / Usability · Priority: P2 · Level: Integration + Browser  
  Steps: Open a PR with no worker, a worker without a preview domain, and a paused worker; post Build anyway.  
  Expected: The control is shown disabled with the reason and the fix; a forced post records nothing and reads nothing from GitHub; paused says "Builds paused".  
  Evidence: `internal/pullrequests` › `TestBuildIsUnavailableWithoutAWorkerThatCanBuild`; browser › *a blocked build stays visible, disabled, with the reason and the fix*

- [x] **TC-BLD-REQ-006 · Requests for another owner's pull request are refused**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: As a second user, open, build, retry, rebuild and tear down the first owner's PR, and list Builds and Environments.  
  Expected: 404 for each; no request or teardown is recorded; the other owner's preview is untouched and never listed.  
  Evidence: `internal/pullrequests` › `TestActionsRefuseAnotherOwnersPullRequest`; `internal/environments` › `TestIntentDefaultsOwnershipAndConstraints`

## 2. Automatic builds (TC-BLD-AUTO)

- [x] **TC-BLD-AUTO-001 · Automatic waits five quiet minutes; a manual build overrides it**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Push to an Automatic repository; push again; press Build now.  
  Expected: A deadline five minutes after the last push; a duplicate observation does not reset it; Build now starts at once with a new revision and fences the old wake-up.  
  Evidence: `internal/environments` › `TestIntentObservationManualAutomaticAndOverride`; `internal/environments` › `TestSchedulingAutomaticDeadlineResetOldWakeAndOverride`

- [x] **TC-BLD-AUTO-002 · Manual repositories build nothing on their own**  
  Type: Negative · Priority: P1 · Level: Integration + End-to-end  
  Steps: Push to a Manual PR; close a Manual PR.  
  Expected: No build without a request; a close still records a teardown.  
  Evidence: `internal/environments` › `TestSchedulingManualObservationAndClosedTeardown`; `internal/app` › `TestRealEndToEnd`

- [x] **TC-BLD-AUTO-003 · A burst of pushes builds only the newest commit**  
  Type: Edge · Priority: P1 · Level: End-to-end (real Docker)  
  Steps: Push several commits in quick succession to an Automatic PR.  
  Expected: Only the newest commit is downloaded and built; the superseded check run is closed; one living comment throughout.  
  Evidence: `internal/app` › `TestRealEndToEnd`

## 3. Admission and capacity (TC-BLD-ADM)

- [x] **TC-BLD-ADM-001 · Build slots cap concurrency; later requests wait and say so**  
  Type: Positive / Concurrency · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Admit more builds than slots, also concurrently.  
  Expected: Never more running builds than slots; the rest wait "for capacity", reported once; they run when a slot frees.  
  Evidence: `internal/environments` › `TestBuildSlotsKeepLaterRequestsQueuedAndWaiting`; `internal/environments` › `TestConcurrentAdmissionsNeverExceedTheSlots`

- [x] **TC-BLD-ADM-002 · Capacity, disk and headroom refusals wait instead of failing**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Have the runtime refuse for capacity and for disk, and headroom refuse before admission.  
  Expected: The request stays queued with its reason, is retried with backoff up to five minutes, and is never recorded as a failure.  
  Evidence: `internal/environments` › `TestRuntimeCapacityRefusalWaitsInsteadOfFailing`; `internal/environments` › `TestHeadroomRefusalKeepsTheRequestQueued`; `internal/environments` › `TestWaitDelayBacksOffToFiveMinutes`; `internal/environments` › `TestWaitingReasonOnlyForCapacityAndDisk`

- [x] **TC-BLD-ADM-003 · Admission re-reads the PR from GitHub**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Admit a build after access is lost, during an outage, after a new commit, and when everything is current.  
  Expected: Lost access ends the request; an outage retries; a superseded request never reaches the runtime; a confirmed one builds.  
  Evidence: `internal/environments` › `TestAdmissionReadsThePRFreshFromGitHub`

- [x] **TC-BLD-ADM-004 · The kill switch pauses builds without touching GitHub or Docker**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Run a build with `BUILDS_PAUSED`; then unpause.  
  Expected: The request waits with reason "paused"; GitHub and Docker are not called; it builds after resuming; worker capabilities say builds are paused.  
  Evidence: `internal/environments` › `TestPausedBuildsWaitWithoutTouchingGitHubOrDocker`; `internal/platform/workers` › `TestPausedWorkersSayWhyNothingBuilds`

- [x] **TC-BLD-ADM-005 · Permanent failures end at once; a worker shutdown is no failure**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Fail a build permanently, then retryably; stop the worker mid-build.  
  Expected: A permanent failure ends after one attempt with its category; a retryable one stays pending; an interrupted build runs again and is not a failure.  
  Evidence: `internal/environments` › `TestPermanentFailuresEndAfterOneAttempt`; `internal/environments` › `TestAWorkerShutdownMidBuildIsNoFailure`

## 4. Ordering and fencing (TC-BLD-ORD)

- [x] **TC-BLD-ORD-001 · Concurrent events bump the generation exactly once each**  
  Type: Concurrency · Priority: P1 · Level: Integration (PostgreSQL, River)  
  Steps: Deliver many PR events concurrently; hand off between jobs.  
  Expected: No lost increments; job uniqueness per PR holds; the successor captures the latest generation.  
  Evidence: `internal/environments` › `TestConcurrentEventsIncrementExactlyOnce`; `internal/environments` › `TestGenerationCaptureAndRiverHandoff`

- [x] **TC-BLD-ORD-002 · Closing or pushing fences running work**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Close a PR, or push a new commit, while its build runs.  
  Expected: The close wins: nothing is published and no resources leak; the obsolete result is refused; a closed or merged PR cannot be built.  
  Evidence: `internal/environments` › `TestCloseWinsOverInflightBuild`; `internal/environments` › `TestIntentCloseAndNewCommitFenceRunningWork`

- [x] **TC-BLD-ORD-003 · An older build finishing late never replaces a newer result**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Finish generation N after generation N+1 published; queue an old build that becomes stale.  
  Expected: No stale overwrite, publication or route activation; superseded resources are cleaned; the stale job skips execution.  
  Evidence: `internal/environments` › `TestOlderBuildFinishesAfterNewerResult`; `internal/environments` › `TestQueuedOldBuildSkipsExecutionAndCleanupRetries`; `internal/environments` › `TestAtomicGuardWaitsForConcurrentGenerationBump`

- [x] **TC-BLD-ORD-004 · The intent state machine and Automatic authorization**  
  Type: Unit · Priority: P2 · Level: Unit  
  Steps: Try every transition between the seven intent states; check Automatic intents against both modes.  
  Expected: Only pending→queued→running→succeeded/failed is allowed; an Automatic intent needs Automatic mode until it runs.  
  Evidence: `internal/environments` › `TestIntentStateMachine`; `internal/environments` › `TestAutomaticIntentNeedsAutomaticModeUntilItRuns`

## 5. Recovery (TC-BLD-REC)

- [x] **TC-BLD-REC-001 · Lost wake-ups, expired leases and exhausted retries are recovered**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL, River)  
  Steps: Delete a wake-up job; let an operation lease expire; exhaust a build's retries; run recovery.  
  Expected: Each is requeued exactly once; a stuck running build is recovered; recovery never queues duplicates.  
  Evidence: `internal/environments` › `TestOperationRecoveryRequeuesLostWakeupAndExpiredLease`; `internal/housekeeping` › `TestRecoveryResumesLostJobsLeasesAndExhaustedRetries`; `internal/environments` › `TestSchedulingDueRecoveryAndAccessCancellation`

- [x] **TC-BLD-REC-002 · The worker rechecks access before running**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Revoke access or cancel the request between scheduling and execution.  
  Expected: The job does not run the build.  
  Evidence: `internal/environments` › `TestSchedulingWorkerRechecksRevokedOrCancelledAccess`

- [x] **TC-BLD-REC-003 · Invalid commands never reach the database**  
  Type: Negative · Priority: P2 · Level: Unit  
  Steps: Submit commands with bad owners, keys, revisions or transitions.  
  Expected: Each is refused before persistence.  
  Evidence: `internal/environments` › `TestIntentCommandValidation`
