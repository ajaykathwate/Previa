# Guided onboarding (Feature 2) · 2026-10-16

## Delivered behavior

- A repository **Setup** tab (Owner, Admin, Developer) walks one open pull request
  from a connected repository to a preview opened from Previa: connect, check the
  configuration, review what the app needs, publish, open. Every step is derived
  from current records; a repository that already has a live preview shows them done.
- **Check setup** asks a worker to read `.previewrc.yaml`, the Dockerfile in its
  build context and the root `.gitmodules` at the exact head commit through a few
  size-bounded GitHub reads, and to validate them with the build's own parser and
  submodule rule against the services on offer. Problems name the key, line and fix
  (Starter kits, including the Own Dockerfile kit). Runtime facts are labelled
  *Checked during the build*.
- **Build preview** uses the existing Build action and comes back to the checklist,
  refusals included. **Open preview** records an open from Previa (not from other
  sites or prefetches) and finishes setup; Request review is labelled as not yet
  available (Feature 3).
- The Overview's next steps follow the role and point at the Setup tab where setup
  continues; accepted invitations land Reviewers on Previews and others on the
  Overview, in the joined workspace.

## Implementation and setup

Migration 0038 adds `repository_setup_checks`, `preview_opens` and
`setup_check_budgets` with realtime triggers; new binaries require Goose 38 (River
007). Run `make migrate-up`, then restart the server and the worker: setup checks
run on any worker with the GitHub App (queue `setup_checks`). No new configuration
or dependency. See [29](../docs/29-guided-onboarding.md) and
[setup](../docs/02-development-setup.md). The development database was not migrated
during this task.

## Verification

| Check | Result |
| --- | --- |
| `make build` (Tailwind, templ, sqlc, Go build) | Passed |
| `make lint` (`go vet`, pinned golangci-lint) | Passed, 0 issues |
| `go test ./...` against PostgreSQL (isolated schemas) | 44 of 45 packages passed on the first run; `internal/onboarding` failed one new test that listened for a database notification and accepted another package's concurrent event. Fixed to wait for its own repository's setup event; the package then passed (37 tests, the browser driver skipped as opt-in), and the test passed three repeated runs |
| `make test-setup-browser` (new, PostgreSQL-backed) | 5 of 5 scenarios passed, no JavaScript or CSP errors |
| `make test-workspace-browser` | 14 of 14 scenarios passed (the invitation scenario now expects a Reviewer on Previews with the *Welcome to …* toast) |
| `make test-browser` (in-memory UI suite) | 55 of 55 scenarios passed (Open preview actions now lead through Previa; the Overview fixture uses the role-aware journey) |
| Screenshots | Setup start, problem, ready to build, done (light, dark, 390 px phone), picker (desktop, phone), Developer view and Overview reviewed; fixes applied for spacing, one primary action per step, repeated facts, tab wrapping on phones and the Overview's repeated actions |

## Test cases

| ID | What is checked | Evidence |
| --- | --- | --- |
| ONB-01 | Only Owners, Admins and granted Developers see setup; Reviewers are sent to Previews; outsiders and ungranted Developers get 404 | `TestSetupIsForPeopleWhoCanBuildInTheRepository`, `TestSetupPageAnswersOnlyPeopleWhoCanBuild` |
| ONB-02 | Setup resumes on the explicit, last-checked or newest open pull request; closed or unknown ones are refused | `TestSetupResumesOnTheRightPullRequest` |
| ONB-03 | Secret names are shown only to managers; Developers see whom to ask | `TestSecretNamesAreShownOnlyToManagers`, browser › *a Developer sees who manages secrets* |
| ONB-04 | A check is requested once per commit, can be re-asked when stale, is refused for a changed commit, lost access, a closed pull request or a server without the App, and never requests a build or enables Automatic builds | `TestACheckIsRequestedOncePerCommit` |
| ONB-05 | A workspace's hourly allowance is enforced and resets | `TestAWorkspaceHasAnHourlyCheckAllowance` |
| ONB-06 | The worker records a result only while it applies: replaced requests, a push while checking, revoked access and closed pull requests record why instead | `TestTheWorkerRecordsAResultOnlyWhileItStillApplies`, `TestTheWorkerNoticesLostAccessAndClosedPullRequests` |
| ONB-07 | GitHub failures keep the last result without calling it current; unexpected errors retry, then are recorded | `TestTheWorkerKeepsTheLastResultWhenGitHubFails`, `TestAFailedAttemptIsNotASuccessButKeepsTheLastResult` |
| ONB-08 | The real queue: the server's producer inserts the job and a River worker runs it | `TestTheQueueRunsARequestedCheck` |
| ONB-09 | Checks read the exact commit by repository ID, call a configuration missing only for a readable commit, classify rate limits, outages and denials, and bound responses | `TestGitHubFiles*` (4) |
| ONB-10 | Missing, invalid and valid configurations; context and Dockerfile states; links; submodules; values never stored | `TestCheck*` (6) |
| ONB-11 | Every step state, one primary action, hosted wording, blocked builds, failures in the PR page's words, opens by the viewer or a teammate, a new commit restarting the path | `steps_test.go` (11) |
| ONB-12 | Open preview records same-origin opens only, not cross-site links or prefetches, sends no referrer and refuses numeric IDs | `TestOpenRecordsOnlyOpensFromPrevia`, `TestOpenPreviewRecordsOpensFromPreviaOnly` |
| ONB-13 | Check requests need CSRF and public IDs and return to the checklist with what happened | `TestCheckRequestsAreProtectedAndReturnToTheChecklist` |
| ONB-14 | Build and Sync from the Setup tab return to the checklist; refusals carry a notice | `TestSetupTabActionsReturnToTheChecklist`, `TestARefusedBuildReturnsToTheChecklistWithItsReason` |
| ONB-15 | Setup changes wake the repository's live pages | `TestSetupChangesWakeTheRepositoryPage`, browser › *the checklist takes an owner … live* |
| ONB-16 | The Overview's journey for each role; a working team sees no checklist | `TestTheOverviewJourneyFollowsTheWorkspace`, `TestSetupChecklistFollowsTheJourneyForEachRole`, browser › *the Overview points each role at its next step* |
| ONB-17 | Joined Reviewers land on Previews, Developers on the Overview, with a welcome toast | `TestInvitationRouteShowsExactScopeAndJoinsWithoutJavaScript`, member lifecycle test, workspace browser suite |
| ONB-18 | The picker is a menu of links that closes on Escape and before navigating, fits a phone, and a live refresh never closes it under the pointer | browser › *the pull request picker …* |
| ONB-19 | Checking works without JavaScript | browser › *checking works without JavaScript* |
| ONB-20 | Worker registration needs only the App; the composed server sends anonymous visitors of Setup to sign in | `TestPipelineRegistersOnlyWhatItsDependenciesAllow`, `TestServerRoutesRegression` |

## Limits

- GitHub was a fixture API in every automated test; a real repository check is part
  of the manual test.
- Request review and review instructions (Feature 3) and the review queue
  (Feature 4) are not built; the UI labels them.
- The PR page and the Overview still show operator build hints (for example
  `make run-worker`) when no worker can build in production; the Setup tab uses
  hosted wording. A hosted-wording pass over those pages is a follow-up.
