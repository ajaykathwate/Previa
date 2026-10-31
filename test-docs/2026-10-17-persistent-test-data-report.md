# Persistent test data (Feature 5B) and F1 · 2026-10-17

## Delivered behavior

### Feature 5B: persistent test data

- **The Test data tab** sits next to Setup for Owners, Admins and Developers with a
  grant.
  - Owners and Admins change its settings in place, each saved with a toast and a
    settings revision: on/off, keep data, use a baseline, baseline branch,
    automatic refresh.
  - Turning on, turning off (which deletes baselines), **Refresh baseline** and
    **Delete** confirm in modals over the page.
  - It shows current baselines per database, whether the branch moved, the five
    latest refreshes with safe failure categories, and each refresh's build log
    (filters, search, download).
- **The `seed` key** runs after `setup` only on empty databases and in baseline
  builds, and needs PostgreSQL or MySQL. Starter kits now split migrations
  (`setup`) from seed data (`seed`) for every stack.
- **Repository baselines.**
  - **Build:** a refresh resolves the default (or chosen) branch head and builds it
    without repository secrets under its own cache scope. It starts the services on
    an internal network, runs setup and seed, and captures each SQL database into a
    gzip-compressed, AES-GCM-encrypted, checksummed file (`PREVIEW_BASELINE_DIR`).
  - **Ordering:** refreshes coalesce, a sequence fence keeps the newest, and
    automatic triggers are spaced.
  - **Admission:** refreshes take build slots, count running builds and build
    minutes, and wait for capacity or the daily allowance.
  - **Triggers:** turning on, a merge into the baseline branch (in the webhook
    delivery's transaction), the daily check, an unusable baseline or a version no
    longer offered, and the button.
  - **Background jobs:** a dispatcher, freshness and cleanup jobs keep jobs, files
    and Docker leftovers in order.
- **Starting data.** Each SQL service tries the pull request's kept data, then the
  baseline, then empty.
  - Copies stream between service containers, into the same or a newer version
    only.
  - A failed setup or copy restarts the databases empty and tries the next source;
    the empty attempt decides.
  - The candidate records the source, the fallbacks and the Setup/Seed timings.
- **Visible outcomes.**
  - The PR page shows *Started from: …* with fallback reasons and says whether data
    is kept between builds.
  - The review page shows a one-line source, and Reset offers *Repository baseline*
    or *Empty database*. A Reset without a choice starts empty and never keeps data.
  - The GitHub comment adds `Test data: …`.
  - A seed failure has its own diagnosis, and an app failing on kept data is pointed
    to Reset.

### F1: spec corrections and the PR page fix

- **Spec.** Feature 4 references were removed from spec §8B.9, §8B.13 and §11.2
  step G.
- **Serving deployment.** The PR page (*Runs with*, *Docs*), the GitHub comment's
  snapshot and component retention now follow the newest publication. After a
  failed update they describe the deployment that still serves, as the review page
  does.

## Implementation and setup

Migration 0041 adds the Test data tables, the starting-data columns,
`build_intents.start_source` and `worker_capabilities.baselines`. New binaries
require Goose 41 (River stays at 007): run `make migrate-up`, then restart the
server and the worker.

- **New settings.** Four optional worker settings, all with defaults:
  `PREVIEW_BASELINE_DIR`, `PREVIEW_BASELINE_MAX_SIZE`,
  `PREVIEW_DATA_COPY_TIMEOUT`, `PREVIEW_BASELINE_MIN_INTERVAL`.
- **Keys.** Baselines use the existing `SECRETS_ENCRYPTION_KEYS`.
- **Production.** The worker image and compose file add a `baselines` volume, which
  is not backed up.
- **Rollback check.** The migration was applied, rolled back (no table left) and
  re-applied in a scratch schema of the test database.

See [32](../docs/32-persistent-test-data.md) and
[setup](../docs/02-development-setup.md).

## Verification

| Check | Result |
| --- | --- |
| `make build` (Tailwind, templ, sqlc, Go build) | Passed |
| `make lint` (`go vet`, pinned golangci-lint) | Passed, 0 issues (after splitting three over-complex functions) |
| `go test ./...` against PostgreSQL (isolated schemas) | 48 of 48 packages passed |
| `make test-test-data` (`TestRealTestDataJourneys`) on Docker Engine 29.8.1 | Passed in 228 s, all ten steps |
| `make test-e2e` (all four journeys) | `TestRealBackendEndToEnd`, `TestRealReferenceJourneys` (Test data off), `TestRealEndToEnd` and `TestRealTestDataJourneys` passed (686 s in total) |
| `make test-runtime` | 11 of 11 real runtime tests passed (264 s), including `TestRealRuntimeCopiesTestData` and managed MySQL |
| After the decided defaults and the Test-data-off rule | `make test-reference` (now asserting no starting source and no comment line without Test data) passed in 103 s; `make test-test-data` passed in 225 s; `make lint` 0 issues |
| `make test-test-data-browser` (new) | 3 of 3 scenarios, five consecutive runs, with no JavaScript or CSP errors |
| `make test-review-browser`, `test-setup-browser`, `test-workspace-browser`, `test-browser` | 6 of 6, 5 of 5, 14 of 14, 55 of 55 (re-run after the shared `ui.js` change) |
| Screenshots | The Test data tab off, ready and for a Developer, and a refresh's build log, at 1440 and 390 px in light and dark; the Turn on and Turn off confirmations |

Found and fixed during verification:

- **The realtime trigger failed for settings rows.** A combined condition read
  `OLD.state`, which only refreshes have. It is now nested; the first database test
  caught it.
- **A live refresh could erase what someone was typing.** A refresh that started
  before the baseline branch field had focus was swapped in while it was being
  edited, dropping the text or the error. In-place forms swapped in were also left
  unbound. Now a live refresh is never swapped in over a focused `data-live-pause`
  form, and in-place forms re-bind after a swap. The browser suite caught it (two
  in three runs failed before the fix).
- **The refresh log page showed *Replaced* for the current baseline.** It now reads
  the current baseline.
- **In-place branch saves expected JSON.** The settings handler now answers them as
  other in-place edits do.
- **Spacing would have delayed a person's change.** Turning Test data on or
  changing the branch is no longer spaced like automatic refreshes, and a refresh's
  own retries are not spaced either.
- **A Reset without a source would have kept data.** It now starts empty.
- **The review test for realtime signals was flaky in full runs.** All test schemas
  share one notification channel, so it now ignores other packages' signals.

## Test cases

| ID | What is checked | Evidence |
| --- | --- | --- |
| PTD-01 | Turning Test data on builds the first baseline from the default branch; it becomes current; the branch head is recorded; activity and build time are recorded | `TestTurningTestDataOnBuildsTheFirstBaseline` |
| PTD-02 | Requests coalesce (one waiting); a request during a run is the single follow-up; the newest refresh is current; a late older result is discarded with its file; replaced baselines are retired | `TestRefreshRequestsCoalesceAndTheNewestResultWins` |
| PTD-03 | A failed refresh keeps the previous baseline with its category and activity; a missing chosen branch fails before building, naming the branch; a chosen branch that exists is built | `TestAFailedRefreshKeepsThePreviousBaseline`, `TestABaselineIsBuiltFromAChosenBranch` |
| PTD-04 | Without a build slot a refresh waits with its reason; the daily allowance waits until it resets; work that could not run retries, then fails after ten tries | `TestRefreshesWaitForCapacityAndRetryWorkThatCouldNotRun` |
| PTD-05 | Automatic refreshes are spaced; a person's request joins and lifts the spacing and moves the waiting job forward | `TestAutomaticRefreshesAreSpacedButRefreshBaselineIsNot` |
| PTD-06 | A refresh whose worker died is taken over; turning Test data off discards a running refresh and its file and cancels a waiting one; paused builds leave refreshes waiting | `TestARefreshWhoseWorkerDiedIsTakenOver`, `TestTurningTestDataOffDiscardsARunningRefresh`, `TestPausedBuildsLeaveRefreshesWaiting` |
| PTD-07 | Only Owners and Admins change settings; Reviewers do not see the tab; stale forms and turning off through Save are refused; invalid branches are refused; consequences of each change; Delete and Turn off retire baselines | `TestOnlyOwnersAndAdminsChangeTestDataSettings`, `TestSettingsChangesFollowTheirConsequences`, `TestTurningTestDataOffAndDeletingRetireBaselines` |
| PTD-08 | Only merges into this repository's baseline branch (default or chosen, with automatic refresh) request a refresh; unusable baselines are marked missing and rebuilt once | `TestMergesIntoTheBaselineBranchRequestARefresh`, `TestUnusableBaselinesAreRebuilt` |
| PTD-09 | The daily check: no repeat within a day, a moved head refreshes, a commit already tried is not retried, a version no longer offered is rebuilt, a missing branch is recorded; cleanup removes retired and orphaned files and old leftovers of refreshes that are not running; the dispatcher replaces lost jobs | `TestFreshnessAndCleanup`, `TestDispatchReplacesLostJobs` |
| PTD-10 | The tab over HTTP: permissions, the turn-on and turn-off forms, conflicts, field errors, refresh, delete, the log page, filters and download | `TestTheTestDataTab` |
| PTD-11 | Baseline files are encrypted, bound and checked; streamed encryption round-trips and refuses tampering | `TestBaselineFilesAreEncryptedBoundAndChecked`, `internal/secrets/stream_test.go` |
| PTD-12 | Starting data on real PostgreSQL 16 → 17 and MySQL 8.4: copy, restart emptying, capture then restore, refused input | `TestRealRuntimeCopiesTestData` |
| PTD-13 | Source order, version rules, recorded detail without data, restore problem classification | `internal/runtime/data_test.go` |
| PTD-14 | Baseline logs continue across writers, redact, filter, and fence late writers | `TestBaselineRefreshLogs` |
| PTD-15 | PR page *Test data* row and the kept-data note; Reset records `empty` by default and `baseline` when chosen and refuses others; the review page's choice and line; the comment line | `TestThePullRequestPageNamesWhereTestDataStarted`, `TestResetFromTheReviewPageReturnsThere`, `TestTheReviewShowsSetupAndOffersReset`, `TestReadyCommentNamesTheTestDataSource` |
| PTD-16 | Test data paths need their ability (manage, operate) | `TestTestDataPathsNeedTheirAbility` |
| PTD-17 | Every kit's seed choices pass Previa's rules next to a database and are refused without one; Rails splits `db:migrate`/`db:seed` | `TestSeedDataPassesConfigRulesWithADatabase` |
| PTD-18 | On real Docker: a baseline built from the default branch, encrypted, nothing left running; a first build restoring it and migrating existing rows without seeding; data kept across a commit and a Rebuild, reported by the comment; a parallel pull request isolated; Reset to the baseline and to empty and seed; an incompatible history falling back with both reasons; a broken migration failing as `setup_failed` while the earlier deployment serves; a fork starting from the baseline; a merge webhook refreshing the baseline; teardown leaving nothing | `TestRealTestDataJourneys` |
| PTD-19 | Browser: turning on in a modal, a refresh followed live, switches and the branch saved in place, Refresh baseline confirmed, the build log, Developer and Reviewer views, turning off in a modal | `make test-test-data-browser` |
| F1-01 | After a failed update the PR page, the report snapshot and component retention describe the serving deployment | `TestThePullRequestPageDescribesTheServingDeploymentAfterAFailedUpdate`, `TestReportSnapshotDescribesTheServingDeploymentAfterAFailedUpdate`, `TestRecordComponentsKeepsTheServingDeploymentAfterAFailedUpdate` |

## Decided defaults

Decided on 17 October 2026 (the owner delegated the choice; spec §8B.8 and §13):

| Setting | Default | Reason |
| --- | --- | --- |
| `PREVIEW_BASELINE_MAX_SIZE` | `64MiB` (the spec proposed 128) | A preview's database lives in a 192 MiB (PostgreSQL) or 384 MiB (MySQL) tmpfs, so a database that fits dumps to well under 64 MiB compressed; synthetic seeds are a few MB. It bounds disk use per repository on a small host. |
| `PREVIEW_DATA_COPY_TIMEOUT` | `2m` | Copies of seed-sized data took seconds in the journeys; two fallbacks still fit the 12-minute build deadline. |
| `PREVIEW_BASELINE_MIN_INTERVAL` | `1h` (the spec proposed 10 minutes) | Refreshes spend the workspace's daily build minutes, which pull request builds share. One hour caps automatic refreshes at 24 a day per repository; the first merge after a quiet hour still refreshes at once, and Refresh baseline is never spaced. |
| `PREVIEW_BASELINE_DIR` | the worker's `baselines` volume in production | Derived data, not backed up. |

With Test data off, a build now records no starting source. Its PR page, review
page and GitHub comment stay exactly as Feature 5 left them; the reference journeys
assert this.

## Manual checklist walkthrough

Claude walked through the user guide's manual rows for Feature 5 (section F2) and
Feature 5B (section F3) through automated equivalents: real Docker journeys and
Chromium against the real handlers. The owner's own hands-on pass is part of the
release acceptance (MVP-TODO.md Part 2).

| Row | What it checks | Evidence |
| --- | --- | --- |
| R12 | The Preview card's setup facts and Test data line | `TestTheReviewShowsSetupAndOffersReset`; `TestRealReferenceJourneys` step 1; `make test-review-browser` |
| R13 | Reset test data: modal in place, new deployment with only seeded data, findings stay | `make test-review-browser`; `TestRealReferenceJourneys` step 2; `TestResetFromTheReviewPageReturnsThere` |
| R14 | No Reset for a Reviewer | `TestTheReviewShowsSetupAndOffersReset` |
| R15 | An Owner adds test access in place | `TestTestAccessSavesAndRevealsWithAndWithoutJavaScript`; `make test-review-browser` |
| R16 | A Reviewer reveals the password and signs in | `TestRealReferenceJourneys` steps 4–5; `make test-review-browser` |
| R17 | A failing seed fails the update; the earlier deployment serves | `TestRealReferenceJourneys` step 3 |
| R18 | Reveal without JavaScript | `make test-review-browser` (no-JavaScript scenario) |
| T1 | A Developer's read-only tab | `make test-test-data-browser`; `TestTheTestDataTab` |
| T2 | A Reviewer is sent to Previews | `make test-test-data-browser` |
| T3 | Turn on: Cancel changes nothing, confirming starts a refresh live | `make test-test-data-browser` |
| T4 | A ready baseline row and its build log | `TestRealTestDataJourneys` step 1 (real refresh, log content); `make test-test-data-browser` |
| T5 | A first build starts from the baseline, without seed | `TestRealTestDataJourneys` step 2; `TestThePullRequestPageNamesWhereTestDataStarted` |
| T6 | A record survives a new commit; the comment names kept data | `TestRealTestDataJourneys` step 3 |
| T7 | A record survives a Rebuild | `TestRealTestDataJourneys` step 3 |
| T8 | Reset to the baseline | `TestRealTestDataJourneys` step 5; `TestTheReviewShowsSetupAndOffersReset` |
| T9 | Reset to empty and seed | `TestRealTestDataJourneys` step 5 |
| T10 | A missing branch fails and keeps the baseline | `TestAFailedRefreshKeepsThePreviousBaseline` |
| T11 | A bad branch name is explained beside the field | `make test-test-data-browser`; `TestTheTestDataTab` |
| T12 | A Developer refreshes the baseline | `TestTheTestDataTab`; `TestTurningTestDataOffAndDeletingRetireBaselines` |
| T13 | A merge into the default branch refreshes it | `TestRealTestDataJourneys` step 9 (through the webhook delivery); `TestMergesIntoTheBaselineBranchRequestARefresh` |
| T14 | Turn off: baselines deleted, builds as before | `make test-test-data-browser`; `TestTurningTestDataOffAndDeletingRetireBaselines`; `TestRealReferenceJourneys` (no source without Test data) |

Nothing needed fixing beyond what is listed under Verification.

## Remaining limits

- **MySQL** is proven at the runtime level (copy, capture and restore on real
  Docker); the web journey runs PostgreSQL.
- **Crash recovery** (takeover, leftovers removed at the next attempt and by
  cleanup) is covered by unit tests and code paths. No process was killed during a
  real refresh or copy.
- **Disconnecting a repository** deletes its Test data records by cascade, and
  cleanup removes the files within hours. Feature 6's disconnect flow (F4) will add
  the visible confirmation.
- **Storage.** Baselines live on one worker's disk; several workers would need
  shared storage.
- **The owner's hands-on pass** of rows R12–R18 and T1–T14 is part of the release
  acceptance (MVP-TODO.md Part 2).
- **Environment.** The journeys ran on a local Docker Engine with a fake GitHub; no
  live GitHub acceptance was run.
