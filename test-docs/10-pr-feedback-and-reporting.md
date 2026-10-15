# 10 · PR feedback and reporting

**Module:** RPT · **Code:** `internal/reporting`, `internal/environments` (report
work) · **Feature docs:** [14 · PR feedback](../docs/14-pr-feedback.md)

The living PR comment and the check run per build revision: what they say, that
they always show the latest state, and that GitHub trouble never blocks a preview.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Comment and checks | TC-RPT-CMT | 4 |
| Resilience and access | TC-RPT-RES | 3 |
| Content | TC-RPT-TXT | 3 |
| Live GitHub proof | TC-RPT-LIVE | 1 |

## 1. Comment and checks (TC-RPT-CMT)

- [x] **TC-RPT-CMT-001 · One living comment, one check run per revision**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL, fake GitHub)  
  Steps: Report a PR repeatedly through build, publication and a rebuild.  
  Expected: One comment, edited in place; one check run per revision, never duplicated; unchanged state is not written again.  
  Evidence: `internal/reporting` › `TestReporterReportsOnceAndSkipsUnchangedState`; `internal/reporting` › `TestReporterGivesEachRevisionItsOwnCheck`

- [x] **TC-RPT-CMT-002 · Reports always show the latest state and never overwrite a newer one**  
  Type: Edge · Priority: P1 · Level: Integration  
  Steps: Report a stale version after a newer one; let two reporters race on one PR.  
  Expected: The latest state is rendered; an older report never overwrites a newer comment; reports are serialized per PR with a lease that is taken over and released.  
  Evidence: `internal/reporting` › `TestReporterAlwaysRendersTheLatestState`; `internal/reporting` › `TestReporterNeverOverwritesANewerComment`; `internal/reporting` › `TestReporterSerializesPerPR`

- [x] **TC-RPT-CMT-003 · New commits and removal update the report correctly**  
  Type: Positive · Priority: P2 · Level: Integration  
  Steps: Push an unbuilt commit; tear the preview down, also while a check is unfinished.  
  Expected: Checks are untouched for an unbuilt commit; removal rewrites the comment but not completed checks, and completes an unfinished check.  
  Evidence: `internal/reporting` › `TestReporterNewCommitAndRemoval`; `internal/reporting` › `TestReporterRemovalStopsAnUnfinishedCheck`

- [x] **TC-RPT-CMT-004 · Report work is recorded only for PRs that need it**  
  Type: Edge · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Open, build, publish and tear down a PR; observe an untouched Manual PR; run without reports enabled.  
  Expected: Each reportable change records report work once; untouched Manual PRs and disabled reporting record none.  
  Evidence: `internal/environments` › `TestReportableChangesRecordReportWork`; `internal/environments` › `TestUntouchedManualPRsRecordNoReportWork`; `internal/environments` › `TestReportsStayOffByDefault`; `internal/reporting` › `TestReporterExecutesOnlyReports`

## 2. Resilience and access (TC-RPT-RES)

- [x] **TC-RPT-RES-001 · Deleted or lost comments come back once**  
  Type: Resilience · Priority: P2 · Level: Integration  
  Steps: Delete the comment on GitHub; lose the stored ID after an ambiguous create.  
  Expected: The comment is recreated, never duplicated.  
  Evidence: `internal/reporting` › `TestReporterRecoversDeletedAndLostComments`

- [x] **TC-RPT-RES-002 · Rate limits and outages are retried; a preview never waits for GitHub**  
  Type: Resilience · Priority: P1 · Level: Integration + End-to-end  
  Steps: Rate-limit and fail report calls; run the real pipeline with GitHub's report API down.  
  Expected: Reports retry and converge; the preview publishes anyway; no check run is created during the outage; the report catches up afterwards.  
  Evidence: `internal/reporting` › `TestReporterRetriesRateLimitsAndOutages`; `internal/app` › `TestRealEndToEnd`

- [x] **TC-RPT-RES-003 · Revoked access and missing permissions are respected**  
  Type: Security · Priority: P1 · Level: Integration + Unit  
  Steps: Report after access is revoked and after the App loses permissions; find the App's comment among a human's.  
  Expected: Nothing is written without access; the outcome is recorded with a safe code; a human's comment is never adopted; failures are classified.  
  Evidence: `internal/reporting` › `TestReporterRespectsRevokedAccessAndPermissions`; `internal/reporting` › `TestGitHubAPIComments`; `internal/reporting` › `TestGitHubAPIChecksAndFailureClasses`

## 3. Content (TC-RPT-TXT)

- [x] **TC-RPT-TXT-001 · Every phase reads correctly**  
  Type: Positive · Priority: P2 · Level: Unit  
  Steps: Derive the report for all eleven phases: none, waiting (Automatic deadline), queued, blocked (checking or conflicts), capacity, building, ready, failed, removing, removed and expired.  
  Expected: The right phase, check status and notices for each; no queue position or URL is ever invented.  
  Evidence: `internal/reporting` › `TestDerivePhasesAndChecks`; `internal/reporting` › `TestDeriveBlockedAndClosed`; `internal/reporting` › `TestDeriveFailureAndNotices`; `internal/reporting` › `TestDeriveWaitingForCapacityAndDisk`

- [x] **TC-RPT-TXT-002 · The comment shows the preview's address, expiry, backend and docs**  
  Type: Positive · Priority: P2 · Level: Unit  
  Steps: Render a ready report with an expiry, managed services and a docs path; render without configured links.  
  Expected: Address, expiry, "Runs with …" and the docs link appear; without a dashboard URL or preview domain no link is rendered.  
  Evidence: `internal/reporting` › `TestCommentRendering`; `internal/reporting` › `TestCommentShowsExpiryOfALivePreview`; `internal/reporting` › `TestReadyCommentNamesTheBackendAndDocs`; `internal/reporting` › `TestDeriveWithoutLinksConfigured`

- [x] **TC-RPT-TXT-003 · The headline and actions on the PR page say what matters most**  
  Type: Usability · Priority: P2 · Level: Unit  
  Steps: Compute the headline and build label for every combination of request and preview state.  
  Expected: The most pressing fact first; the label names the action that would apply.  
  Evidence: `internal/pullrequests` › `TestHeadlineSaysWhatMattersMostFirst`; `internal/pullrequests` › `TestBuildLabelNamesTheActionThatWouldApply`; `web/pages/pullrequests` › `TestLabelsAndTeardownWording`

## 4. Live GitHub proof (TC-RPT-LIVE)

- [x] **TC-RPT-LIVE-001 · Reports on a real, disposable pull request**
  Type: Positive · Priority: P1 · Level: System (real GitHub)  
  Steps: `make test-reporting` with `TEST_GITHUB_*` naming an authorized disposable PR.  
  Expected: One living comment and one check run per revision on GitHub; a deleted comment is replaced; nothing duplicated.  
  Evidence: `internal/reporting` › `TestRealReportingOnADisposablePR`, `TestLiveProofRehearsal`  
  **Verified 2026-10-13:** owner-authorized `ajaykathwate/previa.live` PR #4; temporary proof comments removed, original preview comment preserved, three completed live-proof checks remain. The test does not close or merge the PR.
