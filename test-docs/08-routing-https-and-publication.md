# 08 · Routing, HTTPS and publication

**Module:** RTE · **Code:** `internal/routing`, `internal/environments` (route fence) ·
**Feature docs:** [12 · Routing and HTTPS](../docs/12-routing-and-https.md),
[19 · Generation ordering and fencing](../docs/19-generation-ordering.md)

Turning a healthy candidate into the preview URL: the route fence that only moves
forward, the router's file and certificates, and what visitors see for previews
that do not exist.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Serving over HTTPS | TC-RTE-HTTPS | 3 |
| Route fence | TC-RTE-FEN | 5 |
| Router file and controller | TC-RTE-CTL | 3 |
| Hostnames and unknown hosts | TC-RTE-HOST | 3 |

## 1. Serving over HTTPS (TC-RTE-HTTPS)

- [x] **TC-RTE-HTTPS-001 · A published preview is served over HTTPS at a stable address**  
  Type: Positive · Priority: P1 · Level: System (real Docker, Traefik, Pebble ACME)  
  Steps: Publish a preview; fetch it over HTTPS and plain HTTP; rebuild it.  
  Expected: Served with a wildcard certificate issued by the ACME server; HTTP redirects to HTTPS; the hostname never changes across rebuilds.  
  Evidence: `internal/routing` › `TestRealRoutingServesStableHTTPSAndKeepsThePreviewThroughAFailedSwap`

- [x] **TC-RTE-HTTPS-002 · A failed swap keeps the serving preview**  
  Type: Resilience · Priority: P1 · Level: System + Integration  
  Steps: Publish a new candidate that fails verification just before activation.  
  Expected: The route stays on the old preview, which keeps serving; the failure is reported; the candidate is cleaned up.  
  Evidence: `internal/routing` › `TestRealRoutingServesStableHTTPSAndKeepsThePreviewThroughAFailedSwap`; `internal/environments` › `TestFailedActivationKeepsTheServingPreview`

- [x] **TC-RTE-HTTPS-003 · Router and controller restarts recover without a new certificate**  
  Type: Resilience · Priority: P1 · Level: System  
  Steps: Restart the route controller mid-activation, then restart Traefik.  
  Expected: The activation resumes and converges; Traefik serves again with the stored certificate (no new issuance).  
  Evidence: `internal/routing` › `TestRealRoutingRecoversAfterControllerAndRouterRestarts`

## 2. Route fence (TC-RTE-FEN)

- [x] **TC-RTE-FEN-001 · Routes only move forward**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL) + System  
  Steps: Accept a route; retry the same acceptance; try a target that is not the operation's candidate or not pending; accept a newer version; replay an older one, also through the worker.  
  Expected: The retry is idempotent; wrong targets refused; the newer version wins; stale activations neither rewrite the table nor count as served.  
  Evidence: `internal/environments` › `TestRouteFenceAcceptsOnlyNewerVersions`; `internal/routing` › `TestRealRoutingFencesDelayedStaleOperations`

- [x] **TC-RTE-FEN-002 · A removed route cannot be resurrected by a late activation**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Tear down while an activation is pending; deliver the activation late; reopen the PR and activate.  
  Expected: The tombstone refuses the late activation; after a reopen a new activation is accepted and the old removal cannot undo it.  
  Evidence: `internal/environments` › `TestRouteRemovalTombstoneBlocksLateActivationUntilReopen`

- [x] **TC-RTE-FEN-003 · A close during publication wins**  
  Type: Edge · Priority: P1 · Level: System  
  Steps: Close the PR while its activation is reaching the router.  
  Expected: The activation does not complete; the environment is torn down.  
  Evidence: `internal/routing` › `TestRealRoutingCloseDuringPublicationWins`

- [x] **TC-RTE-FEN-004 · Rebuild revisions are ordered**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Rebuild the same generation and deliver the older revision's activation after the rebuild's.  
  Expected: The rebuild's route stands; the older revision is refused.  
  Evidence: `internal/environments` › `TestSameGenerationRebuildOrdersRevisions`

- [x] **TC-RTE-FEN-005 · The earlier candidate is kept until its replacement is routed**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Publish A; fail B; publish B; inspect cleanup.  
  Expected: A stays pending until B's route activates, then A is cleaned up; a superseded A never reaches the executor.  
  Evidence: `internal/environments` › `TestPendingCandidateIsPreservedUntilReplacementRouteActivates`; `internal/environments` › `TestCleanupKeepsWhatTheRouterServes`

## 3. Router file and controller (TC-RTE-CTL)

- [x] **TC-RTE-CTL-001 · The controller verifies a candidate before routing it**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Activate verified, superseded and failing candidates; remove a route; sync behind and ahead.  
  Expected: Only a verified candidate is attached and written; a superseded activation writes nothing; failures are classified; removal writes a new table; sync writes only when behind and never moves backwards; other operation kinds are refused.  
  Evidence: `internal/routing` › `TestControllerActivatesVerifiedCandidate`; `internal/routing` › `TestControllerSupersededActivationWritesNothing`; `internal/routing` › `TestControllerClassifiesFailures`; `internal/routing` › `TestControllerRemovesRoute`; `internal/routing` › `TestControllerSyncWritesOnlyWhenBehind`; `internal/routing` › `TestControllerStopsWaitingWhenANewerVersionIsAccepted`; `internal/routing` › `TestControllerRejectsOtherOperations`

- [x] **TC-RTE-CTL-002 · The route file only moves forward, even with concurrent writers**  
  Type: Concurrency · Priority: P1 · Level: Unit  
  Steps: Write versions out of order and concurrently; find an unrecognised file; use a relative directory.  
  Expected: The newest version wins, no temporary files remain, an unrecognised file is replaced, and the directory must exist and be absolute.  
  Evidence: `internal/routing` › `TestFileOnlyMovesForward`; `internal/routing` › `TestFileConcurrentWritersKeepTheNewest`; `internal/routing` › `TestFileReplacesAnUnrecognisedFile`; `internal/routing` › `TestNewFileRequiresAnExistingAbsoluteDirectory`

- [x] **TC-RTE-CTL-003 · The router deployment keeps its security properties**  
  Type: Security · Priority: P1 · Level: Static  
  Steps: Read the router's Compose file.  
  Expected: No Docker socket or Docker provider, dashboard off, API on loopback only, file provider read-only, Let's Encrypt staging by default, the DNS token read from a file secret.  
  Evidence: `internal/routing` › `TestRouterDeploymentInvariants`

## 4. Hostnames and unknown hosts (TC-RTE-HOST)

- [x] **TC-RTE-HOST-001 · Preview hostnames are stable, DNS-safe and unguessable**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Generate labels for many PRs; validate good and bad labels; generate without identity.  
  Expected: A fixed DNS-safe format with a random token that does not repeat; invalid labels and missing identity refused.  
  Evidence: `internal/routing` › `TestNewLabelIsAStableDNSSafeFormat`; `internal/routing` › `TestNewLabelTokensDiffer`; `internal/routing` › `TestNewLabelRejectsMissingIdentity`; `internal/routing` › `TestValidLabel`

- [x] **TC-RTE-HOST-002 · Rendered routes are deterministic and send unknown hosts to a fallback**  
  Type: Positive / Negative · Priority: P2 · Level: Unit  
  Steps: Render active routes, an empty table, and invalid routes or settings.  
  Expected: Active versions are routed; everything else redirects to the unavailable page; output is deterministic; invalid input is refused.  
  Evidence: `internal/routing` › `TestRenderRoutesActiveVersionsAndRedirectsTheRest`; `internal/routing` › `TestRenderIsDeterministicAndEmptyTableStillRedirects`; `internal/routing` › `TestRenderRejectsInvalidInput`

- [x] **TC-RTE-HOST-003 · The "preview unavailable" page is a cookie-free, uncached 404**  
  Type: Security / Usability · Priority: P2 · Level: Unit + Integration + Browser  
  Steps: Visit the unavailable page in light and dark themes, through the assembled server.  
  Expected: 404, `no-store`, no cookies set, readable in both themes.  
  Evidence: `internal/routing` › `TestUnavailablePageIsAnUncachedCookieFree404`; `internal/app` › `TestServerRoutesRegression/the_unavailable-preview_page_is_public,_uncached_and_sets_no_cookie`; browser › *preview unavailable page is an uncached cookie-free 404 in both themes*
