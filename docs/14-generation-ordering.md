# 14 · Generation ordering and fencing (implementation)

How the code guarantees that a newer commit, a close or a deliberate rebuild always
beats older work, even when jobs retry, overlap or finish late. This is the
implementation of the decision in [13](13-durable-publication-design.md); the
request revision it carries alongside generation comes from
[11](11-build-policy-and-intents.md), and admission/handoff from
[12](12-build-scheduling.md).

**Status:** generation and revision fencing, guarded results and the database/River
publication outbox are implemented in `internal/environments` and tested with real
PostgreSQL and River plus simulated runtime/publication adapters. Docker, routing,
GitHub reporting and a production reconciliation timer are not connected, so these
tests prove database behaviour, not real deployments. Migration 9 keeps one
environment row per PR for its whole life; admission clears the previous build's
logs only after its checks pass.

## Recording an event and capturing its generation

Migration `0003_pr_generation.sql` adds:

- `pull_requests.generation BIGINT NOT NULL DEFAULT 0` — desired-state sequence.
- `environments.applied_generation` — last applied result's sequence.
- `environments.head_sha` — SHA of the deployed artifact, distinct from desired SHA.

The legacy test/domain `webhooks.Service.ProcessPR` accepts already validated `opened`, `reopened`,
`synchronize` or `closed` events; closed with merged=true records merged. `internal/webhooks/queries.sql` performs:

```sql
INSERT INTO pull_requests (..., generation)
VALUES (..., 1)
ON CONFLICT (repo_id, number) DO UPDATE
SET head_sha = EXCLUDED.head_sha,
    state = EXCLUDED.state,
    generation = pull_requests.generation + 1
RETURNING id, head_sha, state, generation;
```

The counter increments once per successfully processed invocation, including close
events. PostgreSQL serializes concurrent increments on the same PR. The same
transaction persists policy-controlled intent and its delayed/immediate River
wake-up. Manual open observations insert neither intent nor wake-up; close always
records a wake-up. An insertion failure rolls back event, generation and intent. Generation records
processing order, not GitHub delivery timestamps. Production ingress composes unique receipts and source-timestamp guards with the same coordinator; stale sources are ignored and ambiguous changes require refresh. The legacy parsed API remains a trusted integration-test boundary; production
uses the authenticated DeliveryService and source-aware observations.

## Atomic completion guard — review this SQL

The complete query is `internal/environments/queries.sql`, named `ApplyResult`.
Its essential structure is:

```sql
WITH current_request AS MATERIALIZED (
    SELECT pr.id
    FROM pull_requests pr
    WHERE pr.id = $pr_id AND pr.generation = $expected_generation
    FOR UPDATE
), previous AS MATERIALIZED (
    SELECT e.* FROM environments e
    JOIN current_request p ON p.id = e.pull_request_id
    WHERE e.status <> 'torn_down'
    FOR UPDATE OF e
)
UPDATE environments e
SET status = CASE $result_status WHEN 'running' THEN 'publishing' ... END,
    pending_generation = $expected_generation,
    ...
FROM previous
WHERE e.id = previous.id
RETURNING previous.container_id, previous.image_tag;
```

The named placeholders above are illustrative; sqlc generates positional bindings.
The SQL sketch isolates generation fencing; the current query additionally checks
SHA/state and current intent revision. `Repository.Apply` takes the ownership scope/PR
lock before this statement, ensuring a fresh statement snapshot after a concurrent
same-generation revision change. All checks and writes remain in one transaction. A separate optimistic read is used only to skip obsolete work
early; it never authorizes a write.

Locking the **PR row** is essential. If a webhook already holds that lock, the
completion waits, then PostgreSQL re-evaluates its generation predicate against
the updated row. If the completion gets the lock first, the webhook waits until
completion commits. A simple `UPDATE environments ... FROM pull_requests` without
locking the PR would not protect against a concurrently updated desired state.

Zero returned rows for an outdated generation means superseded: no environment
write and no publication intent. A missing active environment for a still-current
request is an error; provisioning is a prerequisite, not silently created here.
For successful builds, the transaction stores a pending candidate and inserts the
route operation and River wake-up atomically. It does not set that candidate active
until an external operation succeeds and a second short guarded transaction
finalizes it. `OperationWorker` invokes its executor between those transactions,
without a PR row lock. The executor contract requires idempotency and route
generation fencing; those external guarantees await real adapters.

## Why close and newer commits win

- A captures generation 1. B records generation 2 and finishes first. A's later
  success **or failure** cannot match generation 2, so B's fields remain intact.
- A captures generation 1. A close event records generation 2. A later finishes
  building a container. The identical generation predicate fails; A does not
  register it or report a final state, and removes its resources.
- A still-queued older build checks the generation before calling the runtime and
  skips building. Cleanup can still discover artifacts left by a prior attempt.
- A failed current rebuild records failure while preserving the previous container,
  image, and deployed SHA. A durable report operation is created separately.

## Uniqueness without dropping the latest request

Build and teardown use separate queues and the same PR-only uniqueness policy.
Generation is deliberately excluded from that key. A newer event while an older
job is active updates the PR's durable desired state instead of creating a second
active job or mutating the running job's captured arguments.

On completion, `finishJob` locks that PR row, completes the old River job using
`JobCompleteTx`, and admits only current authorized eligible intent. A new observed
generation alone is insufficient; future deadlines retain their separate wake-up.
This is one transaction: the unique key is released and acquired by the successor
together. A racing event is processed either before this handoff or after it, when
it can enqueue against the completed row. Terminal worker failures use the pinned
River driver's conditional discarded-state transition with the same handoff;
ordinary retryable errors retain River's backoff. Exhausted crash recovery still
uses the bounded `Broker.RecoverScheduling` contract to recreate wake-ups; a
production periodic runner remains future integration.

The tests deliberately run A and B concurrently through the service to verify the
guard independently of River uniqueness. Production admission ordinarily serializes
the PR's jobs; the guard remains necessary for retries, cancellation, or overlap
after a worker loses its claim.

## Cleanup ownership

`Service.cleanup` inventories resources outside a transaction and schedules cleanup;
the durable cleanup operation worker invokes its executor outside transactions.
The intended adapter behavior is:

1. Locks the PR briefly to read the currently referenced active and pending artifacts.
2. Finds resources labeled with this PR and a generation at or below the obsolete
   current request version, including partial builds/images left by previous attempts.
   Resource references and version bounds now include request revision.
3. Preserves the referenced preview and all resources from newer generations.
4. Persists cleanup work transactionally. The operation worker removes each
   obsolete container, then its owned image, outside the transaction. Missing
   resources must be treated as already removed by the adapter. Images need
   generation-specific tags.
5. Retryable cleanup errors remain durably represented with bounded exponential
   backoff; terminal errors are stored as safe codes for operator visibility.

The durable operation row survives River job retention. The bounded reconciler can
re-enqueue due operations and inventory PR-owned resources, but it is not yet wired
to a production timer or real runtime adapter.

## Timeouts and rescue

| Kind | Execution budget | River worker timeout / rescue eligibility |
| --- | --- | --- |
| Ping | 5 seconds | 5-second timeout; eligible after the 30-second rescue scan floor. |
| Build | `BUILD_TIMEOUT`, default `12m` | Execution budget plus 1 minute for completion/cleanup; default 13 minutes. |
| Teardown | 2 minutes | 3 minutes including completion/cleanup grace. |

`BUILD_TIMEOUT` is worker-only, accepts Go durations such as `15m`, and must be
positive and at most 24h. Pass `cfg.BuildTimeout` to `environments.NewService` when
constructing real adapters. Register both environment workers with River and their
separate queues; use `MAX_CONCURRENT_BUILDS` for the build queue's concurrency.

River v0.47.0 has one client-wide rescue scan floor, but its rescuer also checks each
registered worker's `Timeout`. The effective eligibility age is the larger of the
floor and that timeout, plus normal polling/leader scheduling delay. All worker
processes capable of becoming maintenance leader must register the same kinds and
use consistent timeout configuration. Tests run the real rescuer to prove a slow
build stays running while expired ping/build/teardown attempts are reclaimed.

## Run the checks

```sh
make migrate-up
set -a
. ./.env
set +a
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 -v ./internal/environments
make build lint
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 ./...
```

Ordering integration tests create and remove isolated schemas. Coverage includes
late success/failure, close during a build, stale queued work, cleanup retries,
failed rebuild preservation, both directions of the SQL/event lock race,
concurrent generation increments, enqueue rollback, captured arguments, real River
success/close/terminal-error handoffs, per-kind rescue behavior, durable operation
retry/finalization/recovery, pending-artifact protection, and proof that blocked
external publication does not block a new PR event. Recovery coverage deletes an
operation wake-up and expires a lease before verifying that recovery recreates work.
