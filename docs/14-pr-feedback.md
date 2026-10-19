# 14 · PR feedback: the living comment and check runs

Previa tells reviewers what is happening directly on the pull request: one
**living comment** that is edited in place, and one **check run per build request**
on the exact commit it describes. Both are rendered from PostgreSQL's persisted
truth at the moment they are written, so a late or repeated report can never put an
older state back. This document covers the decisions, what GitHub shows in each
state, how a report gets from a database change to GitHub, how comments and check
runs keep a stable identity, ordering and its limits, failures, permissions and what
was verified. It builds on the durable outbox
and generation fences ([19](19-generation-ordering.md)). The worker registers the
reporter as the report executor whenever it can build ([22](22-mvp-operations.md)),
and both processes record report work whenever the GitHub App is configured.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| When the first report appears (owner decision) | Once the PR has preview work: a build request, an environment, or an existing living comment | A Manual-mode PR nobody asked to build gets no comment and no check run. |
| Check run when a preview is removed (owner decision) | Keep the last result; an unfinished run is completed as *cancelled* ("Stopped") | A finished result is history and stays true for its commit; nothing is left spinning. |
| Report model | **Latest-state sync**: every reportable change bumps the PR's `wanted_version` and enqueues a report operation; the reporter renders the PR as it is *now* | Reports cannot arrive out of order in any way that matters: the newest render always wins, and duplicate or late operations become no-ops. |
| One reporter per PR | A lease in `pull_request_reports`, in PostgreSQL | Two reporters can never both create a first comment or check run for the same PR. |
| Check run identity | One run per (head commit, request revision), external ID `previa:pr:<pr id>:<sha>:r<revision>` | A same-commit Retry or Rebuild gets its own run instead of rewriting a finished one; a retried create finds its run again instead of duplicating it. |
| Reporting and deployments | Separate durable operations with their own retries | A GitHub outage never blocks, delays or rolls back a publication or teardown. |
| Off without the App | `Repository.WithReports()` / `Broker.WithReports(repo)` / `installations.Repository.WithReports(broker)`, enabled by the server and worker when the App is configured | Nothing accumulates where no worker could ever report it. Work recorded while no worker can build waits durably and is reported when one starts. |

## What GitHub shows

The comment title is `Previa preview: <state>`; the check run is named
**Previa preview**.

| State | Comment says | Check run |
| --- | --- | --- |
| Waiting | The commit builds after the five-minute quiet period, at a stated time | queued, "Waiting for the quiet period" |
| Queued | Queued; builds when a worker is free (never a queue position) | queued |
| Waiting to build: Checking | GitHub is still checking mergeability | queued, "Waiting for GitHub" |
| Waiting to build: Conflicts | Resolve the merge conflicts to build | completed, **neutral**, "Not built: merge conflicts" |
| Waiting for capacity / disk space | Builds or previews use the host's capacity, or disk is short; it starts automatically (no position is promised) | queued, "Waiting for capacity" or "Waiting for disk space" |
| Building | Building commit `abc1234` (also after the build, until the route is confirmed) | in progress |
| Ready | The preview URL, the deployed commit and when it expires | completed, **success**, with the URL |
| Failed | A short, safe sentence for the failure category | completed, **failure** |
| Removing / Preview removed | The preview is being removed / no longer exists; no links | open run stopped; a finished run is kept |
| Preview expired | How to bring it back at the same address | as removed |

Rules that hold in every state:

- **Ready requires accepted routing.** A build that succeeded but whose route has not
  been confirmed still shows *Building*; a failed activation shows *Failed*.
- **Current vs deployed commit.** The comment always names the commit it describes.
  While a newer commit or a rebuild is waiting, building or has failed, an older
  healthy preview is shown as *Current preview (commit …)* with its URL and the note
  that it keeps serving until a newer one is ready; a Ready preview of an earlier
  commit says newer commits have not been built.
- **Links only to things that work.** The preview URL appears only for a published
  preview (`https://<hostname>.<PREVIEW_BASE_DOMAIN>`); the build-log link
  (`<APP_BASE_URL>/repositories/<id>/pull-requests/<n>/logs`, Previa sign-in required)
  only when the PR has an environment and therefore a log page. With either setting
  empty, that link is omitted. Removed and expired previews link nowhere except to
  the review.
- **Review in Previa.** With `APP_BASE_URL` set, every comment links the pull
  request's review page (`…/pull-requests/<n>/review`, public IDs), beside the build
  log link: *[Review in Previa] · [Build log] (sign in to Previa)*. It is a link
  only: no findings, instructions, review status, counts or reviewer names are
  projected ([30](30-reviews.md)), so review changes do not bump report versions.
- **Safe failure text.** Failures are stable categories (`unhealthy`, `crashed`,
  `build_failed`, …) stored in `environments.last_error_code` and mapped to fixed
  sentences; unknown categories read "The preview could not be built." Raw errors,
  logs and provider responses never reach GitHub.
- **Fork secrets.** A fork PR whose repository has secrets gets a notice that secrets
  are not given to fork previews; names and values are never mentioned.
- **No invented data.** No queue positions, durations or metrics.

## From a database change to GitHub

```
lifecycle transaction (observe, request, admit, finish, activate, fail, teardown, removal)
   └─ reportTx: wanted_version += 1; report_status operation "report:pr:<id>:v:<n>"   (same transaction)
OperationWorker ──► Reporter.Execute ──► Sync(pr)
   lease the PR ─► read snapshot ─► reported ≥ wanted? done
                 ─► Derive(view) ─► repository by numeric ID ─► comment ─► check run
                 ─► RecordReport(version = wanted read at the start, under the lease)
                 ─► newer wanted meanwhile? render again (at most 3 passes)
```

Report work is recorded in the same transaction as the change it reports, so it is
never lost and never precedes its cause. These changes record it: PR observations
(webhooks, and discovery, mergeability or verification refreshes that changed the
PR), build requests that were not coalesced, queueing, admission, build results,
route activation, failed activation, teardown start and confirmed removal. Every
report operation is relevant whatever its generation, because it renders the
current state rather than the one that queued it.

An unchanged rendering costs no write: the comment's digest (its content without the
version line) and the check run's digest are stored, and an equal digest skips the
call. Several queued operations for one PR collapse into one GitHub update; the rest
see `reported_version ≥ wanted_version` and finish without calling GitHub.

## Stable identity

**Comment.** Its ID is stored on the PR (`pull_requests.github_comment_id`), and its
body starts with `<!-- previa:living-comment -->` and `<!-- previa:version N -->`.

- Normal case: read the stored comment; if it is this App's, edit it.
- Deleted by someone, or not this App's: look for this App's marker comment (at most
  5 pages of 100, newest pages not assumed), adopt it, or create a new one.
- **Create succeeded but saving its ID failed** (crash, database error): the ID is
  saved immediately after the create; if that save is lost, the next report finds
  the comment through the same bounded discovery and adopts it instead of creating a
  second one.
- Ownership is checked by `performed_via_github_app.id` (or the `<slug>[bot]`
  login), so a human comment that quotes the marker is never edited.
- A comment showing a newer version than the reporter's is left alone.

**Check run.** One per (commit, revision), found again by its external ID through
GitHub's check-run list for that commit, filtered by name and App ID (at most 5
pages). When the identity moves on (new commit, Retry/Rebuild, close or removal), an
**open** earlier run is completed as cancelled ("Stopped"); completed runs are never
changed. A run deleted on GitHub's side is recreated.

## Ordering and its limits

GitHub has no compare-and-set on comments or check runs, so Previa does not claim
atomic or exactly-once writes. What it guarantees and how:

- **Serialization:** one reporter per PR through the lease. Each run stops after
  three quarters of the lease (2 minutes by default), so a slow reporter gives up
  before another can start; configure the report operation timeout below the lease.
- **Recording is fenced:** a reporter records progress only while it still holds the
  lease; otherwise it fails with `report_lease_lost` and is retried, which renders the
  current state again.
- **Latest state wins in the end:** a stale write can only come from a reporter that
  was paused past its lease in the middle of a single GitHub call. It can be visible
  briefly, but it cannot remain final: the change that made it stale has its own
  report operation, which rewrites the comment and check run from current truth.
- **Version check before edits:** the reporter skips an edit if the comment already
  shows a newer version. Reading and editing are two calls, so this narrows the
  window rather than closing it.
- Check runs never move backwards in identity: an old revision's run can be completed
  but never reopened, and a new revision always gets its own run.

## Failures, rate limits and permissions

| Category | Meaning | What happens |
| --- | --- | --- |
| `github_rate_limited` | Primary or secondary rate limit | Retried after GitHub's reset time or `Retry-After` (capped at an hour), never sooner than the normal backoff |
| `github_unavailable` | 5xx, timeouts, network errors | Retried with backoff (5 s doubling to 5 min); report operations retry until done |
| `github_permission_denied` | 401/403 on a comment or check call | Permanent for that part; the other part is still written; recorded as `comment_…` or `check_…` in `pull_request_reports.last_error_code` |
| `github_rejected` | 400/422 (invalid input) | Permanent for that part, as above |
| `inaccessible` | Installation inactive, repository removed or no longer visible (404/403 on the repository) | Nothing is written and nothing is retried; the next change after access returns reports again |
| `report_in_progress`, `report_lease_lost`, `report_store_unavailable` | Another reporter holds the PR, the lease was lost, or PostgreSQL failed | Retried |

If both parts are permanently refused (for example, the owner has not accepted the
required permissions), the operation fails permanently with that category and later
changes try again. Progress made before a retryable failure (such as a created
comment) is recorded, so a retry does not repeat it. Error categories are stable,
lowercase strings without response text.

### Required GitHub App permissions

| Permission | Used for |
| --- | --- |
| **Pull requests: write** | Reading, creating, editing (and in the live proof deleting) the PR's issue comment |
| **Checks: write** | Creating, listing and updating check runs |
| **Metadata: read** (automatic) | Resolving the repository by its numeric ID |

These are the permissions registered in [04](04-github-sign-in-and-app.md) (Part B, Register).
If they are reduced, owners must accept the change on GitHub; until then, reports
record `github_permission_denied` and deployments continue unaffected. The App's ID
and slug, used to recognise its own comments, come from the App itself
(`GET /app` with the App JWT), so workers still need only `GITHUB_APP_ID` and
`GITHUB_APP_PRIVATE_KEY_PATH`.

## Schema and code

- Migration `0015_pr_reports.sql`: `pull_request_reports` (wanted/reported versions,
  digests, current check identity, lease, last error category) and
  `environments.last_error_code`.
- `internal/reporting`: `state.go` (Derive: persisted truth to view; safe failure
  sentences), `render.go` (comment body, marker, version, digests, external IDs),
  `github.go` (the `GitHub` interface and its go-github adapter with error
  classification), `reporter.go` (lease, passes, comment and check run sync).
- `internal/environments/reports.go`: `reportTx`, `WithReports`; hooks in the
  lifecycle transactions listed above. `pullrequests` records reports for refreshed
  observations through the broker (`RecordReportTx`).
- Wiring (`internal/app/execution.go`): the App's ID and slug are read from GitHub
  (`GET /app`) at start, then `reporting.NewGitHubAPI(githubapp client, app ID, slug)`
  and `reporting.NewReporter(pool, api, Settings{AppBaseURL, PreviewBaseDomain}, logger)`
  become the `report_status` executor of the operation worker. The server
  (`internal/app/server.go`) enables `WithReports` on its repository, request broker
  and build-policy store, so owner requests, webhooks and mode switches record reports.
- Waiting and failure wording lives in `internal/environments/messages.go`, shared by
  the reporter and the dashboard so both say the same thing.

## What was verified

- `go test ./internal/reporting` against PostgreSQL and a fake GitHub: duplicate
  report operations, stale success and failure (latest state always rendered), a
  deleted comment, an ambiguous create, a comment showing a newer version, same-commit
  retries with one run per revision, a new commit and removal (open run stopped,
  finished run kept), primary and secondary rate limits with their delays, outages,
  revoked access and permissions, the per-PR lease, and the adapter's pagination and
  error classes.
- `internal/environments` tests: report work is recorded through queued, building,
  ready and teardown, not for an untouched Manual PR, not twice for a duplicate
  teardown, and not at all with reporting off; failed activation stores its category.
  `internal/pullrequests`: refreshed observations report only when they change the PR.
- `TestLiveProofRehearsal` runs the live proof's script against the fake GitHub.
- `make test-e2e` (`TestRealEndToEnd`) drives the comment and check runs through the
  worker's whole pipeline against a fake GitHub over TLS: one comment throughout,
  Ready with the preview address, a failed rebuild reported while the old preview
  keeps serving, removal, and no check run left open on a superseded commit.
- **Live proof** (`make test-reporting`, opt-in): on an authorized disposable PR it
  drives queued, building, ready, a failed same-commit rebuild, a deleted comment, a
  lost comment ID and removal, logs only the resulting comment and check run IDs,
  then deletes the comment and completes any open run. Check runs named
  "Previa preview (live proof)" remain on that commit as completed history because
  GitHub has no API to delete check runs. Setup and expected output:
  [02](02-development-setup.md#live-pr-feedback-proof-optional).

## Not done yet

- The live proof on a real PR (`make test-reporting`) has not run: it needs an
  authorized disposable PR in the `TEST_GITHUB_*` settings
  ([22](22-mvp-operations.md#live-proofs-still-required-before-a-public-rollout)).
- Expiry, PR reconciliation (which finds a close missed while webhooks were down)
  and the waiting states come from housekeeping and capacity
  ([15](15-housekeeping-and-capacity.md)).

## Missed installation deliveries (F5)

App-authenticated reconciliation can discover a lost installation or repository
and use the normal local revocation/teardown path even when GitHub is unreachable
for reporting. It does not rewrite an inaccessible PR comment or invent a final
check result. Token-mint 403/404 failures in the shared client only signal a
durable reconciliation job; reporting does not revoke access itself. Reconnects
do not authorize builds. See [35](35-installation-recovery-and-early-access.md).
