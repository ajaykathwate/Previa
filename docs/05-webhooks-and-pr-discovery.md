# 05 · Webhooks and pull-request discovery

How Previa learns about pull requests: signed webhooks for changes as they happen,
bounded API discovery for PRs that already exist, and targeted mergeability checks.
All three only *observe* state; none of them authorizes a build. The screens are in
[03-user-guide.md](03-user-guide.md).

## Decisions and alternatives

| Decision | Chosen approach and reason | Alternative and tradeoff |
| --- | --- | --- |
| Webhook durability | Compose the receipt, lifecycle/PR update and scheduling intent in one short PostgreSQL transaction. Existing lifecycle parsing is shared and persistence now accepts a transaction. No external calls occur in this boundary. | A durable inbox plus River processor would add processing states and another queue. Use it if later ingress work cannot remain short and transactional. |
| PR ownership boundary | Flat `internal/pullrequests`; templates/view models in `web/pages/pullrequests`. Existing installation handlers keep connection/list ownership; dashboard keeps Overview/session/tools. | Global HTTP/delivery folders would relocate working features without a concrete benefit. |
| Routes | `/repositories/{repoID}` and `/repositories/{repoID}/pull-requests/{number}`, using local IDs scoped to installation ownership. Upstream resolution uses immutable numeric GitHub repository IDs. | Repository names in route ownership keys are mutable and can change after transfers/renames. |
| Discovery execution | Automatic open-only discovery on verified connection/repository addition; later open-only sync through the CSRF-protected Sync Pull Requests button. River fetches at most 100 PRs per job and durably schedules the next page. | Synchronous page rendering would block navigation and repeatedly spend API quota. |
| Event chronology | GitHub PR `updated_at` orders source snapshots; a separate local generation fences work. Equal-time conflicting heads require refresh; closed/merged wins an equal-time open observation. | Local delivery order alone can regress state. Timestamp-only last-writer-wins guesses within GitHub's timestamp resolution. |
| Installation ordering | Deletion is terminal. Lifecycle timestamps fence activation; per-repository access observations retain removal tombstones, with removal winning timestamp ties. Fresh verified linking can restore ambiguous access. | Installation timestamps are not a reliable per-repository selection sequence; treating one timestamp as an exact sequence could reopen revoked access. |
| Discovery race fence | Capture installation revision and repository observation version before API IO. Lock installation → repo → sync record → PRs and recheck before writes. Retry the page when either changed. | Holding database locks across GitHub calls blocks events; comparing only PR timestamps misses concurrent access/relink changes. |
| Worker credentials | Optional App ID/private-key pair; no OAuth, session, slug or App client secret needed. Without the pair, the worker processes ping and leaves discovery queued for a configured worker. | Requiring server config in workers unnecessarily distributes login credentials. |
| Queue capacity | Two discovery workers, 45-second per-page timeout, eight ordinary attempts. Rate limits snooze until reset/retry-after; source-race retries snooze briefly. | Sharing build capacity/timeouts makes short metadata operations compete with long image builds. |
| Build controls | Show a disabled per-PR action and reject direct authenticated POSTs without creating intent. | Accepting a request before real adapters exist would promise work the application cannot perform. |

## Durable event processing

- `POST /webhooks/github` sits outside browser session/CSRF middleware. It requires
  HMAC-SHA256 authentication over the exact raw body, using `go-github` validation
  before parsing. Browser mutations continue to require session authentication,
  ownership and CSRF.
- Maximum body: **2 MiB**; oversized requests get 413. This bounds parsing/transaction
  work; unusually large GitHub deliveries require revisiting the limit.
- Missing secret returns 503; absent/wrong/legacy SHA-1 signatures return 401.
  Invalid JSON/envelopes/supported payloads return 400. Database/transient failures
  return 503 so a delivery can be retried. Unsupported signed event types return 204.
- `webhook_deliveries.delivery_id` is unique. `INSERT ... ON CONFLICT DO NOTHING`
  serializes concurrent duplicates. An existing ID must match event type and body
  hash; otherwise return 409. The receipt commits only with its effects.
- A crash before commit leaves no receipt/effects. A crash after commit but before
  the response is safe: retry reads the existing receipt and applies nothing again.
- Receipt rows contain delivery identity, event type, hash and time, not raw bodies.
  They are kept without automatic pruning; housekeeping must choose an explicit
  replay window before deleting any, or old redeliveries could apply twice.
  Logs contain safe delivery/event identities, never tokens, signatures or payloads.
- Supported PR actions: opened, reopened, synchronize, closed; merged close records
  `merged`. Comments/reviews neither authorize builds nor reset quiet periods.
- Unlinked/inaccessible repositories are acknowledged without exposing or creating
  another user's PR state. Currently open PRs can later be discovered through sync.
- Installation revocation sets durable `cleanup_pending`. No absent runtime adapter
  is reported as successful teardown. A later cleanup reconciler must examine
  current access/resource ownership before clearing this marker.

## Source ordering and generations

- A unique receipt is transport acceptance; it is distinct from applying a fresh
  PR state. Older source observations are acknowledged and ignored.
- Fresh applicable webhook observations advance PR generation once and compose
  with the scheduling coordinator ([08](08-build-scheduling.md)). Duplicate
  receipts advance nothing.
- Equal-time conflicting heads retain the existing head, mark `needs_refresh`,
  advance generation and supersede active intent, preventing guessed state from
  authorizing/publishing a result. A later source observation or fenced API refresh
  resolves it. A close wins an equal-time open/head conflict.
- Discovery is observational: first sightings may have generation zero. A changed
  existing head/state advances generation and supersedes active obsolete intent;
  it never creates automatic build intent, even in Automatic mode.
- Discovery never lists or stores historical closed/merged PRs. A closed webhook
  only updates an existing tracked row; an unknown closure creates no row. A
  targeted mergeability GET may close an already tracked PR without adding history.
- Pre-migration PR rows with no source timestamp require authoritative discovery
  before a conflicting open observation can replace their known state; closure
  still invalidates work conservatively.
- Source timestamps have finite precision. Conservative conflict handling can require
  an explicit refresh after rapid same-second changes; this favors safety over guessing.
- Fork status and numeric head repository ID are retained. Execution must enforce
  the approved fork/secrets policy before any future build; no new trust policy is added.

## Discovery durability and visible status

- Verified linking/addition atomically queues initial open-only discovery once for
  accessible owned repositories. Later clicks sync only open PRs; additional
  clicks while queued/syncing coalesce.
- Each page's observations, progress and next River job commit together. A crash
  replay cannot double-count an already completed page or lose continuation.
- The GitHub client resolves repository metadata by numeric ID before using its
  current owner/name for the PR API. Pages use `state=open`, `sort=created`, ascending,
  with 100 records maximum. Browser pages remain ten records each.
- `pr_syncs` stores run revision, next page, fetched count, queued/syncing/complete/
  failed status and last successful refresh. Failed refreshes preserve existing data
  and never infer closure from an omitted PR or a partial page.
- Completion means that pagination finished; GitHub does not provide a transactional
  snapshot across list pages. Webhooks cover concurrent changes. Later periodic
  reconciliation remains a separate feature.
- Source/access races retry a fresh page. API failures save a safe visible failure
  message; a subsequent automatic attempt or explicit new refresh can recover.
- A newly requested refresh revision fences older jobs. Failure updates also match
  page identity and cannot downgrade a completed run.

## Enable GitHub deliveries locally

1. Generate `openssl rand -hex 32`; save the value as `GITHUB_WEBHOOK_SECRET` in `.env`.
   Restart the server. Keep `.env` out of Git.
2. In **Settings → Developer settings → GitHub Apps → your App**, enable webhooks.
   Put the same value in **Webhook secret**, preserving the existing App callbacks.
3. Set **Webhook URL** to a public HTTPS forwarding URL ending in
   `/webhooks/github`. Forward only that endpoint to local port 8080. A forwarding
   tool is optional development tooling; no deployed tunnel is added.
4. Subscribe to **Pull request** events. GitHub App installation and repository-
   selection lifecycle events are delivered for the App automatically.
5. Run PostgreSQL, `make dev` and a configured `make run-worker`. Open/commit/close a
   PR in an available linked repository. Inspect the App's **Recent deliveries**:
   expect 204, then check the repository/PR page. Redelivering the same delivery
   must not change generation or create another request.
6. In Manual mode, expect PR tracking with “Not requested.” In Automatic mode,
   expect durable pending intent with a five-minute deadline. Actual builds stay disabled.

These are reproducible instructions, not a claim that a real GitHub delivery was
observed in this task. Signature behavior follows
[GitHub's validation documentation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries).
Discovery follows the [GitHub PR list API](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests).

## Signed fixtures and automated checks

With the local server and secret configured:

```sh
make test-webhook
```

Expected: `HTTP 204: signed fixture accepted`. This sends a signed `ping` fixture,
which deliberately has no application state effects. No secret appears in arguments.

For a standalone server stateful fixture, use a separate development database
seeded with installation 99/GitHub repository 100; do not send synthetic PR state
to a database containing real integrations. The automated test below creates the
fixture scope, sends signed requests through the real handler and removes it.
If you have separately prepared that fixture database/server, the command is:

```sh
WEBHOOK_EVENT=pull_request WEBHOOK_DELIVERY_ID=fixture-pr-1 \
WEBHOOK_FIXTURE=internal/webhooks/testdata/pull_request.json make test-webhook
```

Export `TEST_DATABASE_URL` as described in [02](02-development-setup.md#5-build-and-check), then run:

```sh
go test -count=1 -race ./internal/pullrequests ./internal/webhooks
```

Tests create/drop isolated schemas and include real River
continuation, concurrent deduplication, transaction rollback/retry, stale/ambiguous
PR ordering, revocation, ownership, CSRF and observational sync without builds.

## Mergeability

GitHub computes whether a PR can merge asynchronously, and the answer changes when
the base branch moves, even without a push to the PR. Previa therefore treats the
saved value as a hint for the dashboard and re-checks it at every build boundary.

Migration 10 stores nullable `mergeable`, the raw `mergeable_state` and a derived
`build_status`, separate from the lifecycle (`open`, `closed`, `merged`):

| GitHub signal for an open PR | Status shown | Build eligibility |
| --- | --- | --- |
| `mergeable=false` or `mergeable_state=dirty` | Conflicts | Refused: resolve conflicts and push a commit |
| `mergeable=true` or `mergeable_state=clean` | Open | Eligible, subject to access, generation and policy gates |
| Null/unresolved without `clean` | Checking | Refused: wait for the check or sync again |

Contradictory signals favour Conflicts. Every opened/reopened/synchronize event
replaces the saved signal, so a push resets the status to Checking.

- An unresolved observation schedules a `pr_mergeability` job on the discovery queue
  two seconds later. It GETs only that already-tracked PR
  (`GET /repos/{owner}/{repo}/pulls/{number}`) and never discovers history.
- Still-unresolved results retry every two seconds, at most eight ordinary attempts;
  rate limits respect retry-after/reset. Exhaustion leaves Checking visible and
  builds blocked until another event or sync retries.
- API calls run outside transactions. Installation/repository revisions and PR
  generation/SHA fence stale responses, so an old check cannot overwrite a newer commit.
- A check triggered by a webhook resumes Automatic coordination when it resolves;
  checks from discovery never authorize builds. An existing event-authorized request
  survives an overlapping sync check that resolves first.
- Manual requests, scheduled dispatch and runtime admission all gate on the saved
  status, with runtime admission checking it under the same lock as the generation.
  The dashboard shows the reason; direct Build POSTs return 409.
- Committed changes reach open pages through database triggers and Server-Sent
  Events ([11](11-realtime-updates.md)); the browser refetches saved data and never
  calls GitHub.

## Verification status

Automated tests use signed fixtures, an HTTP fake of the GitHub API and real
PostgreSQL/River. One test creates a genuine local Git merge conflict and shows that
manual requests, scheduled dispatch and runtime admission all refuse it until a
resolving push restores Open. A **hosted** proof is still pending: delivering real
GitHub webhooks to a public URL, and watching a real PR move from Checking to
Conflicts to Open, needs a disposable repository with write access. Steps:

1. Configure the webhook secret and public URL (above), run server and worker.
2. Link a disposable repository with two open PRs plus some closed/merged ones;
   expect only the two open PRs after discovery.
3. Create a conflicting change on the base branch; expect Conflicts and the refusal
   reason. Push a resolution; expect Checking, then Open.
