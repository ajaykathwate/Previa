# Repository policy and durable build intent

## Status and boundaries

Prompt 2 implements the persistence/domain foundation, tested with PostgreSQL.
Migration `0006_build_intents.sql` adds repository build mode and a current intent
row per PR. The live server now constructs the policy/observation services. Persistence APIs alone do
not enqueue jobs, create environments, change deployed previews or perform external
calls. Production workers execute ping and App-configured PR discovery; executable previews remain disabled.

Prompt 3 now integrates parsed events, wake-ups, worker admission/handoff and
request-revision guards through results/publication/cleanup. Details are in
[BUILD_SCHEDULING.md](BUILD_SCHEDULING.md). Production remains fail-closed until
real execution adapters are configured; repository/PR policy UI and signed HTTP observation are now implemented by Prompts 4–5.

## Approved behavior

- New and existing repositories default to Manual. Only the linked installation
  owner with current repository access can read/change policy or request work.
- Automatic is opt-in. Changing mode does not create intent for existing PRs.
- Switching to Manual atomically cancels automatic `pending`/`queued` intent.
  Manual intent, running authorized work and deployed previews are unchanged.
- A running automatic build may finish after switching to Manual, provided no
  newer PR state or request has superseded it. Close/new commit still wins.
- Ordinary Build requests coalesce with current pending/running work. After
  success or failure, Build returns the existing result rather than executing again.
- Explicit **Retry** is valid after failure; explicit **Rebuild** after success.
  Each deliberate request needs a fresh idempotency key and the current revision.
- A newer PR generation makes Retry/Rebuild of the old snapshot conflict. Use
  Build latest to authorize the new state instead of silently retrying old code.
- Manual override of pending/queued automatic work creates a new immediate manual
  revision. Already-running equivalent work coalesces.
- Closed/merged PRs cannot accept builds. The observation API invalidates active
  intent and returns teardown-required, in either mode. Teardown scheduling remains
  the Prompt 3 coordinator; actual removal still requires external adapters.

## Schema and identities

`repos.build_mode` is `manual` or `automatic`, default `manual`. Installation
upserts/renames/relinks do not overwrite it. Same-owner reinstall preserves policy
through stable repository identity. Revoked/suspended/inactive access blocks APIs.

`build_intents` has a primary key on `pull_request_id`, giving one current row:

| Fields | Purpose |
| --- | --- |
| `revision`, `base_revision` | Current authorized request version and the version the command expected; new revision = base + 1. |
| `generation`, `head_sha` | Captured observed PR state, distinct from deployed environment state. |
| `source`, `request_action`, `requested_by` | Manual/automatic origin; build/retry/rebuild/automatic action; manual owner identity. |
| `idempotency_key` | Opaque token for replaying the current command; scoped to that PR. Never log or expose it in presentation. |
| `state`, `available_at` | Current lifecycle and eligibility deadline. |
| `requested_at`, `started_at`, `completed_at`, `updated_at` | Current coordination timestamps, not historical attempt rows. |

Constraints enforce mode/state values, positive revisions, source/actor consistency,
completion/start timestamps and nonempty captured SHA. A partial due index covers
pending intent. There is no builds archive, historical idempotency ledger or new
preview row for merely observed PRs.

Captured generation may be zero for migration/bootstrap state. Manual requests
must not fabricate a webhook by incrementing generation. Prompt 3 migration 7
permits nonnegative generations in pending/operation records and job validation.
Integration tests prove authorized generation-zero dispatch and finalization with
a positive request revision. No production bootstrap build is claimed.

## Idempotency and same-commit fencing

Manual commands include owner/PR, action, expected revision and a 16–128 character
URL-safe opaque key (`A–Z`, `a–z`, digits, `_`, `-`). Expected revision is zero when
no intent exists. Do not treat it as the expected PR generation.

Example:

1. PR generation 7, no intent. Build with expected revision 0 creates revision 1.
2. Replaying that key/action/base returns revision 1, even if it has since completed.
3. A separate current Build command coalesces with revision 1's current work/result.
4. After failure, explicit Retry with a fresh key and expected revision 1 creates
   revision 2, still capturing PR generation 7.
5. An old command expecting revision 0 or old transition for revision 1 cannot
   overwrite revision 2. A replay using the current key with a different action/base
   is rejected as a key conflict.

Only the current key is retained. Older HTTP replays are protected by their stale
expected revision; they do not need an unbounded token/history table. A new key
alone does not bypass the expected-revision or allowed-action checks.

New request writes use an UPSERT guarded by current PR generation/SHA/open state
and the expected intent revision. Lifecycle writes use this SQL pattern:

```sql
UPDATE build_intents b SET state = $next_state, ...
FROM pull_requests pr
WHERE b.pull_request_id = pr.id
  AND pr.id = $pr_id AND pr.state = 'open'
  AND pr.generation = $expected_generation
  AND b.generation = pr.generation AND b.head_sha = pr.head_sha
  AND b.revision = $expected_revision AND b.state = $previous_state
  AND ($next_state <> 'running' OR b.available_at <= clock_timestamp())
RETURNING b.*;
```

The PR row is already locked in the same transaction, so a concurrent observation
cannot pass between the check and the write. A request revision distinguishes
attempts even when generation/SHA do not change. Returning zero rows never grants
authority to overwrite a newer request.

**Coordination integration:** Prompt 3 carries revision through immutable job args,
result/finalization guards, operation payloads/keys and reporting/cleanup ownership.
Real adapters must fence external effects as well. See [BUILD_SCHEDULING.md](BUILD_SCHEDULING.md)
for atomic eligibility/claim SQL and external-system limits.

## Lifecycle and transaction rules

Transitions are pending → queued → running → succeeded/failed. Cancelled/superseded
are terminal for that revision. A new authorized request replaces the current row
with revision + 1 and pending state. Repeating the same current transition is
idempotent. Running admission checks due time; terminal transitions cannot skip
execution. Success here is a domain transition API, not proof of actual deployment.

Every mutation locks **installation → repository → PR → intent**. Mode cancellation
locks only PRs with unstarted automatic intent, in ascending PR ID order. Ownership,
installation status, access and repository membership are rechecked after locks.
The final guarded SQL runs inside the same transaction. External work never runs
inside it. Lock acquisition honors the caller's context.

`ObserveTx` must be composed with a trusted lifecycle state update in one transaction,
with installation/repo locks acquired **before** PR mutation. It reads the actual
locked PR state, not a caller-supplied SHA/generation. In Manual it creates no
automatic work; stale active intent is superseded. In Automatic, a new observed
generation can record an explicit deadline. Duplicate observation of the same
snapshot does not reset it. Initial discovery/refresh must not call this API to
bulk-authorize builds; scheduling intent is for accepted lifecycle events.

Prompt 3 now provides five-minute deadlines, delayed River wake-ups, intent-aware
retry/recovery dispatch and atomic teardown wake-ups. Delivery deduplication remains
Prompt 4. Caller
transactions must roll back on any API error; do not commit partially processed
observations. Production app construction remains unchanged until coordination is
integrated safely.

## API ownership

- `installations.BuildPolicyService`: owner-scoped Get/Set through a small store
  interface; repository mode + automatic cancellation commit together.
- `environments.IntentService`: Get, Request, Advance and ObserveTx through a small
  store interface, independent of runtime adapters.
- `IntentRepository`: database persistence and lock/CAS boundaries. It does not
  import the installations feature's concrete Go types.
- Feature-owned sqlc queries produce `dbgen` code. Shared schema model changes in
  other generated packages are expected and must not be edited by hand.

## Local verification

```sh
make migrate-up
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 -race -run '^TestIntent' ./internal/environments
```

For the test command, export DATABASE_URL first as described in
[DEVELOPMENT_SETUP.md](DEVELOPMENT_SETUP.md), or export TEST_DATABASE_URL directly.
Do not pass credentials as CLI flags. Tests create/drop isolated schemas; they do
not reset the application's schema/data. Migration rollback/reapply is tested only
inside a disposable test schema.

Tests cover migration defaults/constraints, ownership/revocation, mode preservation
on rename/reinstall, duplicate keys/actions, explicit Retry/Rebuild, stale same-SHA
transitions, Manual observation, Automatic deadline persistence/replay, manual
override, due checks, mode switch during pending/queued/running work, close/new
commit supersession, concurrent requests/retries/mode changes and rollback after
intent cancellation failure. Build/test/vet/lint and the full database-backed
regression suite must also pass before this task is complete.
