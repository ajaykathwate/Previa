# 03 · Webhooks and pull request discovery

**Module:** HOOK · **Code:** `internal/webhooks`, `internal/pullrequests` · **Feature
docs:** [05 · Webhooks and PR discovery](../docs/05-webhooks-and-pr-discovery.md)

How Previa learns about pull requests: signed GitHub webhooks, deduplicated and
ordered, and paged discovery that fills in what webhooks missed. This is the start
of the PR-to-preview pipeline, so authenticity and ordering are tested hard.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Signature and envelope | TC-HOOK-SIG | 5 |
| Deduplication | TC-HOOK-DUP | 3 |
| Ordering | TC-HOOK-ORD | 3 |
| Discovery | TC-HOOK-DISC | 5 |
| Mergeability | TC-HOOK-MRG | 3 |

## 1. Signature and envelope (TC-HOOK-SIG)

- [x] **TC-HOOK-SIG-001 · Unsigned or wrongly signed deliveries are refused**  
  Type: Security · Priority: P1 · Level: Integration (assembled server)  
  Steps: Post a delivery with no signature, one signed with another key, and one with an HMAC lacking the `sha256=` scheme.  
  Expected: 401 for each; nothing is recorded.  
  Evidence: `internal/app` › `TestServerRoutesRegression/webhooks_need_GitHub's_signature_and_a_sound_envelope`; `internal/webhooks` › `TestSignatureAndEnvelope`

- [x] **TC-HOOK-SIG-002 · A malformed envelope is refused**  
  Type: Negative · Priority: P2 · Level: Integration (assembled server)  
  Steps: Send correctly signed deliveries with a path-like delivery ID (`../etc/passwd`), no event kind, a non-JSON body, and an incomplete `pull_request` payload.  
  Expected: 400 for each.  
  Evidence: `internal/app` › `TestServerRoutesRegression/webhooks_need_GitHub's_signature_and_a_sound_envelope`

- [x] **TC-HOOK-SIG-003 · Events Previa does not use are acknowledged and ignored**  
  Type: Edge · Priority: P3 · Level: Integration  
  Steps: Send a signed `ping` and other unsupported event kinds.  
  Expected: 204; nothing is processed.  
  Evidence: `internal/app` › `TestServerRoutesRegression/webhooks_need_GitHub's_signature_and_a_sound_envelope`; `internal/webhooks` › `TestSignatureAndEnvelope`

- [x] **TC-HOOK-SIG-004 · Oversized deliveries are refused; tunnels are allowed**  
  Type: Negative / Edge · Priority: P2 · Level: Integration (assembled server)  
  Steps: Send a signed body over 2 MiB; send a valid delivery with another `Host` (a development tunnel).  
  Expected: 413 for the oversized body; the tunnelled delivery is accepted (the host check exempts only the webhook path).  
  Evidence: `internal/app` › `TestServerRoutesRegression/webhooks_need_GitHub's_signature_and_a_sound_envelope`

- [x] **TC-HOOK-SIG-005 · Without a webhook secret the receiver refuses everything**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Run the handler with an empty secret.  
  Expected: 503; no delivery is processed.  
  Evidence: `internal/webhooks` › `TestSignatureAndEnvelope`

## 2. Deduplication (TC-HOOK-DUP)

- [x] **TC-HOOK-DUP-001 · A redelivery is idempotent; a reused ID with other contents is a conflict**  
  Type: Positive / Negative · Priority: P1 · Level: Integration (assembled server, PostgreSQL)  
  Steps: Deliver a `pull_request` event; deliver it again; deliver another payload under the same delivery ID.  
  Expected: 204, 204, then 409; exactly one delivery row.  
  Evidence: `internal/app` › `TestServerRoutesRegression/a_pull_request_delivery_is_recorded_once_and_a_reused_ID_is_a_conflict`

- [x] **TC-HOOK-DUP-002 · The same delivery sent 20 times at once is recorded once**  
  Type: Concurrency · Priority: P1 · Level: Integration (assembled server, PostgreSQL)  
  Steps: Send one delivery from 20 goroutines simultaneously.  
  Expected: Every copy is answered 204 (the losers as redeliveries); exactly one delivery row; no data race (run with `-race`).  
  Evidence: `internal/app` › `TestServerRoutesRegression/the_same_delivery_sent_concurrently_is_recorded_exactly_once`

- [x] **TC-HOOK-DUP-003 · A failed delivery rolls back and can be retried**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Make processing (or the job enqueue) fail mid-delivery, then retry the same delivery.  
  Expected: Nothing from the failed attempt persists, the PR generation is unchanged, and the retry succeeds.  
  Evidence: `internal/pullrequests` › `TestWebhookRollbackCanRetrySameDelivery`; `internal/environments` › `TestEventEnqueueFailureRollsBackGeneration`

## 3. Ordering (TC-HOOK-ORD)

- [x] **TC-HOOK-ORD-001 · Older observations never overwrite newer ones**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Deliver pushes out of order and duplicates; record a Manual PR.  
  Expected: Ordering follows GitHub's source time; duplicates do not bump the generation; a Manual PR authorizes nothing.  
  Evidence: `internal/pullrequests` › `TestWebhookAtomicDedupSourceOrderingAndManual`

- [x] **TC-HOOK-ORD-002 · Ambiguous same-time conflicts are fenced until GitHub settles them**  
  Type: Edge · Priority: P1 · Level: Unit + Integration  
  Steps: Judge new, older, newer, same-time conflicting, closing and legacy observations (12 cases); deliver a signed fixture at an ambiguous timestamp.  
  Expected: Older ones ignored; same-time conflicting webhooks fence the PR for an authoritative read, except a close of an open PR; snapshots apply.  
  Evidence: `internal/pullrequests` › `TestJudgeObservationOrdersObservations`; `internal/pullrequests` › `TestSignedFixtureAndAmbiguousTimestamp`

- [x] **TC-HOOK-ORD-003 · A legacy row needs an authoritative baseline before a reopen**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Reopen a PR stored before source timestamps existed.  
  Expected: The reopen is fenced until a fresh GitHub read provides a baseline.  
  Evidence: `internal/pullrequests` › `TestLegacyPRNeedsSourceBaselineBeforeReopen`

## 4. Discovery (TC-HOOK-DISC)

- [x] **TC-HOOK-DISC-001 · Connecting stores only open PRs, never history**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Connect a repository with two open, one closed and one merged PR; sync later; relink.  
  Expected: Exactly the two open PRs are stored; no historical closures are backfilled; relinking does not duplicate discovery.  
  Evidence: `internal/pullrequests` › `TestConnectionStoresOnlyTwoOpenPRsAndNeverHistoricalClosures`; `internal/pullrequests` › `TestDiscoveryPersistenceRejectsHistoricalSnapshots`

- [x] **TC-HOOK-DISC-002 · Discovery pages, coalesces and continues through River**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL, River)  
  Steps: Sync several pages; request syncs concurrently; let real River jobs continue page by page.  
  Expected: Pages are bounded and ordered; concurrent requests coalesce; only the owner's repository is synced; continuation completes.  
  Evidence: `internal/pullrequests` › `TestDiscoveryPaginationCoalescingAndOwnership`; `internal/pullrequests` › `TestDiscoveryRealRiverContinuation`

- [x] **TC-HOOK-DISC-003 · Discovery uses numeric identities and authenticated, bounded pages**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Run discovery against a fake GitHub that checks requests.  
  Expected: Installation authentication on every call; repository ID, never the mutable name; bounded page sizes.  
  Evidence: `internal/pullrequests` › `TestGitHubDiscoveryUsesNumericIdentityAndBoundedAuthenticatedPages`

- [x] **TC-HOOK-DISC-004 · Rate limits and failures are safe and recoverable**  
  Type: Resilience · Priority: P1 · Level: Unit + Integration  
  Steps: Return a rate limit and provider errors; fail mid-sync, retry; let an old failure arrive after a completed page.  
  Expected: Rate limits are honoured; errors never surface provider text; partial data is kept and the retry completes; an old failure cannot downgrade a finished page; a snapshot is fenced against webhooks and installation changes.  
  Evidence: `internal/pullrequests` › `TestDiscoveryRateLimitsAndSafeErrors`; `internal/pullrequests` › `TestDiscoveryFailurePreservesPartialDataAndRetryCompletes`; `internal/pullrequests` › `TestOldDiscoveryFailureCannotDowngradeCompletedPage`; `internal/pullrequests` › `TestDiscoverySnapshotFencesWebhookAndInstallation`

- [x] **TC-HOOK-DISC-005 · A sync never spins forever**  
  Type: Edge / Usability · Priority: P2 · Level: Integration  
  Steps: Request a sync with no worker running; let a sync stall past five minutes.  
  Expected: The page says it waits for a worker; a stalled sync says so and a new request replaces it, and the stalled job's late writes are ignored.  
  Evidence: `internal/pullrequests` › `TestStalledSyncIsReplacedInsteadOfWaitingForever`; `internal/pullrequests` › `TestRepositoryPageSaysWhenSyncsWaitStallOrNeverResolve`

## 5. Mergeability (TC-HOOK-MRG)

- [x] **TC-HOOK-MRG-001 · Unknown mergeability is never treated as mergeable**  
  Type: Security / Edge · Priority: P1 · Level: Unit + Integration  
  Steps: Observe a PR whose mergeability GitHub reports as null, then dirty, then clean after a push.  
  Expected: Null means "Checking" and is polled; conflicts are stored and refuse builds; the resolving push makes it buildable again and keeps event-authorized Automatic work.  
  Evidence: `internal/pullrequests` › `TestMergeabilityMappingNeverAssumesNullIsClean`; `internal/pullrequests` › `TestNullMergeabilityPollsAndFencesConflictResolution`

- [x] **TC-HOOK-MRG-002 · A real Git conflict blocks manual and scheduled builds until resolved**  
  Type: Negative · Priority: P1 · Level: Integration (PostgreSQL, real Git)  
  Steps: Create a genuine merge conflict; request a build manually and through scheduling; resolve and push.  
  Expected: Admission, dispatch and the runtime all refuse with "resolve conflicts" guidance; nothing reaches the runtime; after the fix the build runs.  
  Evidence: `internal/environments` › `TestRealGitConflictBlocksManualAndScheduledBuildsUntilResolution`

- [x] **TC-HOOK-MRG-003 · A check GitHub never answers is recorded as Unknown and cleared later**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Exhaust the mergeability retries; then observe a new push.  
  Expected: Early attempts retry; the last records that GitHub never answered ("Unknown"); a new observation clears it; a stale give-up cannot mark the newer one.  
  Evidence: `internal/pullrequests` › `TestMergeabilityThatNeverResolvesIsRecordedAndCleared`
