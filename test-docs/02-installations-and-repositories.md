# 02 · Installations and repositories

**Module:** INST · **Code:** `internal/installations`, `web/pages/repositories` ·
**Feature docs:** [04 · GitHub sign-in and App](../docs/04-github-sign-in-and-app.md),
[05 · Webhooks and PR discovery](../docs/05-webhooks-and-pr-discovery.md)

Connecting a GitHub App installation, keeping it in step with GitHub, listing an
owner's repositories, and the per-repository build policy.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Linking an installation | TC-INST-LINK | 3 |
| Installation lifecycle | TC-INST-LIFE | 2 |
| Repository list | TC-INST-LIST | 5 |
| Build policy | TC-INST-POL | 3 |

## 1. Linking an installation (TC-INST-LINK)

- [x] **TC-INST-LINK-001 · Installing the App links it to the signed-in owner**  
  Type: Positive · Priority: P1 · Level: Integration  
  Steps: Start the installation (with CSRF), return through setup and the user authorization callback.  
  Expected: The installation links only after GitHub approves it and the user authorizes; the flow finishes on the repositories page.  
  Evidence: `internal/installations` › `TestInstallRedirectCallbackStateAndSession`

- [x] **TC-INST-LINK-002 · Forged, replayed or unapproved link callbacks are refused**  
  Type: Security · Priority: P1 · Level: Integration  
  Steps: Start without CSRF; send an unsafe setup request; call back before approval; replay a finished callback.  
  Expected: Each is refused; nothing is linked twice.  
  Evidence: `internal/installations` › `TestInstallRedirectCallbackStateAndSession`

- [x] **TC-INST-LINK-003 · Linking requires the user's own access, verified with GitHub**  
  Type: Security · Priority: P1 · Level: Integration  
  Steps: Link with a user whose GitHub token cannot see the installation; link from a stale snapshot; page through the user's installations.  
  Expected: Unauthorized installations are refused; stale snapshots are discarded; pagination and identity checks use the user's token.  
  Evidence: `internal/installations` › `TestAppUserAuthorizationIdentityAccessAndPagination`; `internal/installations` › `TestLinkRequiresIdentityAndAccessAndDiscardsStaleSnapshot`

## 2. Installation lifecycle (TC-INST-LIFE)

- [x] **TC-INST-LIFE-001 · Suspend, unsuspend, removal and deletion follow GitHub, in order**  
  Type: Positive / Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Apply created, suspend, unsuspend, repository removal, deletion and late or stale events, and reinstall by another user.  
  Expected: Ownership never changes; suspended and removed repositories become unavailable; unsuspend restores the selection; a late "created" never revives a deleted installation; a reinstall never takes another owner's repository; PR history survives.  
  Evidence: `internal/installations` › `TestPostgresInstallationOwnershipLifecycleAndRevisionFence`

- [x] **TC-INST-LIFE-002 · Real lifecycle payloads from GitHub parse**  
  Type: Positive · Priority: P3 · Level: Unit  
  Steps: Parse recorded installation and repository-selection webhook payloads.  
  Expected: Every fixture yields the right change.  
  Evidence: `internal/installations` › `TestLifecycleFixturePayloads`

## 3. Repository list (TC-INST-LIST)

- [x] **TC-INST-LIST-001 · The list pages, filters and stays scoped to the owner**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Page through repositories with search and the available/unavailable/all filter; ask for a page past the end; read another user's view.  
  Expected: Stable ordering and counts; filters limit rows and totals; out-of-range pages are clamped; no other owner's repository appears.  
  Evidence: `internal/installations` › `TestPostgresRepositoryPaginationFiltersAndOwnership`

- [x] **TC-INST-LIST-002 · Invalid list input is refused; failures are handled**  
  Type: Negative · Priority: P2 · Level: Integration  
  Steps: Request the list signed out, with a bad page or filter, past the last page, and with the database failing.  
  Expected: Anonymous access refused; bad input 400; past the end redirects to the last page keeping filters; a database failure shows a safe 503 message. In-page requests return only results, with `Vary` set.  
  Evidence: `internal/installations` › `TestRepositoryHTTPValidationScopeAndOptionalConfiguration`

- [x] **TC-INST-LIST-003 · Repository names are escaped**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Render the list with a repository name containing markup.  
  Expected: The name appears as text; the connect action is disabled without App configuration.  
  Evidence: `web/pages/repositories` › `TestRepositoryPageEscapesDataAndPreservesFilters`

- [x] **TC-INST-LIST-004 · Pagination and live search keep the person's place**  
  Type: Usability · Priority: P2 · Level: Browser  
  Steps: Page with filters; search as you type, then clear the search.  
  Expected: Filters survive paging; clearing restores all records; the search field keeps focus.  
  Evidence: browser › *repository pagination preserves filters and connection action is in the header*; browser › *repository live search restores all records and keeps the input focused*

- [x] **TC-INST-LIST-005 · Small collections show compact cards without pagination**  
  Type: Usability · Priority: P3 · Level: Browser  
  Steps: Open the list with up to five repositories.  
  Expected: Compact cards at natural height, no pager.  
  Evidence: browser › *small repository collections use compact cards with natural height and no pagination*

## 4. Build policy (TC-INST-POL)

- [x] **TC-INST-POL-001 · Automatic builds need a confirmation; cancel and failure change nothing**  
  Type: Positive / Negative · Priority: P1 · Level: Browser  
  Steps: Flip the Automatic switch; cancel; flip again and confirm while the server fails (503); confirm again.  
  Expected: No request until confirmed; cancel leaves Manual; the failure keeps the modal open with an error and the switch off; the retry saves in place.  
  Evidence: browser › *Automatic confirms before enabling saves in place and live updates respect modal focus*

- [x] **TC-INST-POL-002 · Switching to Manual cancels only unstarted automatic work**  
  Type: Edge · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: With queued automatic, running automatic and manual requests, switch the repository to Manual.  
  Expected: Unstarted automatic requests are cancelled; running work and manual requests are kept.  
  Evidence: `internal/environments` › `TestIntentModeChangeCancelsQueuedButPreservesRunningAndManual`

- [x] **TC-INST-POL-003 · A mode change racing an observation leaves no stray automatic work**  
  Type: Concurrency · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Switch modes while pushes are observed concurrently; make the cancellation fail.  
  Expected: No live automatic intent survives a switch to Manual; a failed cancellation rolls the mode change back.  
  Evidence: `internal/environments` › `TestIntentConcurrentModeChangeAndAuthorization`; `internal/environments` › `TestIntentAtomicObservationAndModeRollback`
