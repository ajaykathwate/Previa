# Phase 3 — open-only PR tracking and mergeability

Implemented on 2026-10-04. Production preview execution remains disabled pending
the existing runtime/publication work; this phase does not enable later features.

## Tracking changes

- Verified repository connection and repository additions atomically queue initial
  discovery once. Unowned/inaccessible repositories do not queue discovery.
- `internal/pullrequests/github.go`, `GitHubSource.Page`, now calls
  `client.PullRequests.List` with `State: "open"` instead of `State: "all"`:

  ```http
  GET /repos/{owner}/{repo}/pulls?state=open&sort=created&direction=asc&per_page=100&page=N
  ```

- The API adapter and discovery persistence boundary both reject closed/merged
  snapshots. This applies to initial discovery and every later sync.
- Both authenticated delivery processing and the older parsed-event service ignore
  unknown closed/merged PRs. Known PRs close/merge in place, preserving their row ID.
- No PR-delete operation was introduced. Retained tracked closures remain visible
  using Closed/Merged/All filters. Open means all active PRs, including Checking
  and Conflicts. Omission from an open-only page never infers closure.
- Existing records are retained: legacy rows lack reliable provenance distinguishing
  tracked closures from earlier backfill, so this phase does not purge them.

## Mergeability and build admission

Goose migration 10 adds nullable `mergeable`, raw `mergeable_state`, and derived
`build_status`, separately from lifecycle (`open`, `closed`, `merged`).

| GitHub signal for an active PR | Visible status | Build eligibility |
| --- | --- | --- |
| `mergeable=false` or `mergeable_state=dirty` | Conflicts | Refused; resolve conflicts and push a commit |
| `mergeable=true` or `mergeable_state=clean` | Open | Eligible, subject to existing access/generation/policy gates |
| Unresolved/null without `clean` | Checking | Refused; wait for the check or sync again |

Contradictory signals favor Conflicts. Every opened/reopened/synchronize event
replaces the saved signal; null on a push resets the status to Checking.

Unresolved observations schedule a `pr_mergeability` River job after two seconds.
It uses `GET /repos/{owner}/{repo}/pulls/{number}` only for an already tracked PR;
it never discovers historical closures. Pending results retry every two seconds,
up to eight ordinary attempts. Rate limits respect retry-after/reset. Exhaustion
leaves Checking visible and builds blocked until another event or sync retries.
IO runs outside transactions; installation/repository revisions and PR generation/
SHA fence stale responses. Resolving an event-driven check resumes Automatic
coordination. Discovery alone does not authorize builds; an overlapping sync check
preserves existing event authorization.

Manual requests and scheduled dispatch reject Conflicts/Checking. The shared runtime
admission gate repeats the saved-status check under the generation lock immediately
before `Runtime.Build`, including for a queued request whose generation still matches.
SQL completion/publication guards also require verified buildability. Conflict
refusal gives the contributor a resolve-conflicts reason in the PR dashboard;
authenticated CSRF-protected direct Build POSTs return HTTP 409 with that reason.
Open/all lists and active PR details poll saved status every three seconds, pausing
for focused filters and open confirmation modals. Dashboard polling does not call
GitHub. The presentation theme is preserved.

## Test evidence

- `TestConnectionStoresOnlyTwoOpenPRsAndNeverHistoricalClosures`: actual connection
  persistence, River enqueue and the GitHub adapter against an HTTP fixture with
  **2 open + 1 closed + 1 merged → exactly 2 stored**. Asserts `state=open`; repeated
  linking does not duplicate initial discovery. Later syncs and unknown close/merge
  deliveries add no history. A tracked merge retains its original row ID.
- `TestDiscoveryPersistenceRejectsHistoricalSnapshots`: an alternate source returning
  mixed states still stores/counts only the two open records.
- `TestNullMergeabilityPollsAndFencesConflictResolution`: signed webhook null →
  Checking → resolved dirty/false → Conflicts; synchronize → Checking → clean/true
  → Open. Obsolete checks cannot overwrite the resolving commit; Automatic
  authorization survives a sync check resolving first.
- `TestRealGitConflictBlocksManualAndScheduledBuildsUntilResolution`: creates two
  branches changing the same line, verifies an actual Git merge conflict, rejects
  manual authorization, scheduled dispatch and runtime admission with a clear
  reason and zero runtime calls. Resolves the conflict and pushes the merge commit
  to a local bare remote; a simulated clean synchronize payload restores Open and
  admits the test runtime. Runs in both Manual and Automatic modes.
- GitHub adapter tests cover the exact list query and targeted GET/raw nullable
  signal. Mapping tests cover null, dirty, blocked, unstable and contradictory
  signals. HTTP tests verify visible Conflicts/Checking feedback and HTTP 409 while
  retaining ownership/CSRF protection.

`make build`, the full PostgreSQL-backed `go test -count=1 ./...`, `go vet ./...`
and pinned `golangci-lint run` pass. Final race checks pass for pullrequests,
environments and webhooks. All 20 browser regressions also pass. Chromium and
three missing libraries were downloaded/extracted under `/tmp`; no system packages
were installed. The browser check used:

```sh
UI_BROWSER_EXECUTABLE=/tmp/previa-phase3-browser-manual/chrome-linux64/chrome \
LD_LIBRARY_PATH=/tmp/previa-phase3-browser-libs/extracted/usr/lib/x86_64-linux-gnu \
make test-browser
```

Packages without tests are reported by Go;
generated packages and unbuilt runtime/reporting packages have no independent
tests. Initial implementation verified migration 10 on a disposable test database;
that database and credential env file were removed afterward. On 2026-10-04, at
the user's request, `make migrate-up` applied pending migrations 9 and 10 to the
configured application database. Goose 10, River 007 and all three PR mergeability
columns were verified; rollback-protected database constraint tests pass. Restart
server/worker to run the updated code.

## Live GitHub proof still required

The tests above use simulated GitHub payloads/API responses and runtime adapters.
The merge conflict and resolving push are real local Git operations; they are not
a hosted GitHub PR. GitHub CLI has no authenticated account, and no disposable
repository/installation has been supplied for creating PRs. The live test requested
by the user therefore remains unperformed. It needs a chosen disposable repository
and authenticated write access; a repository choice was requested during the task.
Rollout checks confirmed that App ID/key are configured and the key file is
accessible; `.env` has no webhook secret or test repository/installation IDs.
Configure a matching App/server webhook secret and public HTTPS receiver before
the live delivery proof. No live comments or Docker builds are claimed. Steps are in
[DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md#verify-phase-3-pr-tracking-and-mergeability).

README Section 4.5 and DESIGN Section 1 now explicitly document the authorized
Phase 3 scope. No architecture/library/folder/theme deviation was introduced.
GitHub semantics follow the official
[PR list and get documentation](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests).
