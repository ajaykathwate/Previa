# Native review and feedback (Feature 3) · 2026-10-16

## Delivered behavior

- Every pull request has a **review page** (`…/pull-requests/{n}/review`). Owners,
  Admins and granted Developers reach it through a **Preview | Review** switch on the
  pull request page (with the open-findings count); Reviewers through **Review** on
  Previews, a shared review link, or a pull request link that redirects them.
- **How to test:** a summary, numbered steps, an expected result and optional start
  and docs pages that open inside the preview, written in place by people who can
  build, revisioned, never overwritten by the GitHub description (shown separately,
  escaped, as imported context).
- **Findings:** record (text, page path, reproduction details) against the
  deployment the page shows; reply; edit own in place; remove own (erased, with a
  tombstone); moderate anyone's message or a whole finding with a reason;
  resolve/reopen with revision checks; filters, literal search, 50 per page; a
  finding's header opens its own page with every reply.
- **Deployments:** each route activation records a numbered publication; a new commit
  or a same-commit Rebuild is a new deployment, a failed update none. A deployment
  published while a finding is drafted triggers a choice (newest or the tested one),
  never a silent relabel. Live previews at migration are *Present at migration*.
- **Live and no-JS:** review changes refresh open pages; a refresh waits while
  someone types and keeps unsent drafts with their deployment and revision. Every
  action works without JavaScript (forms redirect back; edit and remove use
  `?edit=`/`?remove=`).
- **Access:** Reviewers use only review pages, their forms, the repository's change
  stream and Open preview; logs, builds, secrets and settings stay closed. Closed or
  merged pull requests are read-only except removal. Removed members' words stay,
  attributed as former members.
- **Activity (step B):** publications, instruction saves and every finding change
  are recorded in the same transaction, deduplicated, kept 90 days and 5,000 per pull
  request by an hourly job. The timeline view is Feature 4.
- **GitHub:** the living comment links *Review in Previa*; nothing of the review is
  projected.

## Implementation and setup

Migration 0039 adds `preview_publications` (with backfill), `preview_components.config_hash`,
`activity_events`, `activity_pruned`, `pr_review_instructions`,
`preview_feedback_threads`, `preview_feedback_messages`, `pr_review_counters` and
`review_write_budgets`, with realtime triggers; new binaries require Goose 39 (River
007). Run `make migrate-up`, then restart the server and the worker. No new
configuration or dependency. The migration was also applied, rolled back and
re-applied in a scratch schema of the test database. See [30](../docs/30-reviews.md)
and [setup](../docs/02-development-setup.md).

## Verification

| Check | Result |
| --- | --- |
| `make build` (Tailwind, templ, sqlc, Go build) | Passed |
| `make lint` (`go vet`, pinned golangci-lint) | Passed, 0 issues (after adding the `reviews` package comment and escaping a direction-override character in a test) |
| `go test ./...` against PostgreSQL (isolated schemas) | 47 of 47 packages passed. Two existing expectations were updated for intended changes: the worker pipeline now also registers `activity_prune`, and Setup's next links lead to the review page |
| `make test-review-browser` (new, PostgreSQL-backed) | 5 of 5 scenarios passed with no JavaScript or CSP errors |
| `make test-setup-browser` | 5 of 5 passed (the next step is now the review page) |
| `make test-workspace-browser` | 14 of 14 passed (a Reviewer's Previews now link review pages only) |
| `make test-browser` (in-memory UI suite) | 55 of 55 passed |
| Screenshots | Review page empty, with instructions, editing, a conversation, resolved list, version conflict, removal and moderation dialogs, light and dark at 1440 px and phone at 390 px, reviewed. Fixed: step numbers, search icon spacing, a one-line composer, the reproduction-details toggle, stretched radio buttons, phone hero and filter rows |

Found and fixed during verification:
- htmx copied `style` attributes while settling a live swap. A tool or extension that leaves one on a field caused CSP refusals, so `attributesToSettle` now excludes `style` for every page.
- Open preview ignored the review's page paths. It now accepts a validated `path` inside the preview.

## Test cases

| ID | What is checked | Evidence |
| --- | --- | --- |
| REV-01 | Owners, Admins and granted Developers and Reviewers read the review; outsiders, ungranted members and other workspaces get 404; who may operate | `TestReviewIsForPeopleWithPreviewAccess` |
| REV-02 | A conversation: create (idempotent), reply (idempotent), edit own with revision, no editing others, deployment kept on edit, older-deployment label, resolve/reopen with revisions, the verified deployment, timeline events | `TestAFindingsConversation` |
| REV-03 | Removing an unanswered finding, a tombstone when replies depend on it, erasure in the database, Reviewers cannot moderate, moderation needs a reason, finding removal, replies to removed findings refused, counts, the reason on the timeline | `TestRemovalAndModeration` |
| REV-04 | A newer deployment while drafting: conflict before any deployment and after one, keeping the tested deployment on purpose, the newest, unknown deployments refused, the deployment filter | `TestANewerDeploymentNeverRelabelsAFinding` |
| REV-05 | Instructions: Reviewers cannot write them, saving, steps, concurrent saves with the saved version, saving over on purpose, timeline events, unsafe paths and text refused, ordinary parameters accepted | `TestTestingInstructions` |
| REV-06 | Closed pull requests are read-only except removal | `TestAClosedPullRequestIsReadOnly` |
| REV-07 | A removed member loses access immediately and stays attributed | `TestARemovedMemberLosesAccessButStaysAttributed` |
| REV-08 | Size limits, empty text, forged request keys, the finding limit (and the page saying so), the hourly allowance with its reset time and no partial write | `TestReviewLimits` |
| REV-09 | Pages, the last page, author filter, literal case-insensitive search (`%` is not a wildcard), a finding's own page, a missing finding | `TestFiltersSearchAndPages` |
| REV-10 | Lists show the opening message and the latest replies; the finding's page shows all | `TestLongConversationsShowTheLatestReplies` |
| REV-11 | Deleting a retired pull request removes its publications, instructions, findings, messages, counters and activity in one statement | `TestDeletingAPullRequestTakesItsReviewWithIt` |
| REV-12 | A review change notifies the repository's live topic | `TestReviewChangesWakeTheRepositoryPage` |
| REV-13 | Reviewers reach the review, are redirected from pull request links, cannot open logs or post to build, teardown, policy, secrets or instructions; the change stream and Open preview are open to them | `TestReviewersUseTheReviewPageAndNothingElse`, `TestTheReviewStreamIsOpenToReviewers`, `TestOpenPreviewRecordsOpensFromPreviaOnly` |
| REV-14 | Every form without JavaScript: redirects with anchors and toasts, escaping, double submission, validation and conflicts with drafts, list kept, stale state notices, no-JS removal, CSRF, numeric IDs refused | `TestFindingFormsWorkWithoutJavaScript` |
| REV-15 | Moderation asks again without a reason; tombstones show the reason; the pull request page links its review | `TestModerationNeedsAReasonAndAsksAgain` |
| REV-16 | Instructions save, open through Open preview, keep conflicts, refuse outside addresses | `TestInstructionsSaveInPlaceAndKeepConflicts` |
| REV-17 | Publications at activation only, once per candidate; none for a failed update; same-commit Rebuild is new; passed to the timeline | `TestPublicationsRecordEachServedCandidateOnce` |
| REV-18 | Activity dedup, derived scope, no timeline outside workspaces, invalid events refused; pruning by age and cap with markers | `TestEventsAreRecordedOnceAndStaySmall`, `TestPruningKeepsNinetyDaysAndACapPerPullRequest` |
| REV-19 | The GitHub comment links the review and carries no review state | `TestCommentLinksTheReviewOnly` |
| REV-20 | Open preview goes to validated page paths only | `TestOpenPreviewRecordsOpensFromPreviaOnly` |
| REV-21 | Browser: instructions in place; a Reviewer's whole conversation; the header opening a finding; live reply and deployment with an unsent draft; moderation; no JavaScript | `make test-review-browser` (5 scenarios) |

## Remaining limits

- Review requests, assignments, decisions, the review queue and the activity
  timeline view are Feature 4; the page labels them as not available yet.
- Projecting review status to GitHub needs the repository sharing policy of delivery
  step E; today the comment carries a link only.
- Verification used fixtures for GitHub and the worker's activation inside the
  PostgreSQL suites; no hosted multi-user or live GitHub acceptance was run.
