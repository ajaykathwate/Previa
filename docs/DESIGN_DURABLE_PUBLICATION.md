# Design: Durable, generation-fenced publication

This document records the architecture decision for safely publishing preview
environment changes when PostgreSQL, River, Docker, the routing layer, and GitHub
can fail independently. It covers only that consistency problem. Product
requirements remain in [README.md](../README.md), feature implementation details
remain in [DESIGN.md](../DESIGN.md), and the current generation implementation and
tests remain in [GENERATION_ORDERING.md](GENERATION_ORDERING.md).

## Decision

Use PostgreSQL as the durable source of desired PR state and environment state.
Use River for at-least-once delivery of durable work. Keep generation checks and
database mutations inside short transactions. Perform builds and external
publication calls outside those transactions. The transactional River job is the
durable work record for build/teardown requests; route, reporting, and cleanup
side effects each get a durable operation/outbox record plus a River wake-up job
in the transaction that makes that action necessary. Make adapters idempotent and
generation-aware, and run reconciliation to bring Docker and routing back to the
committed database state after partial failure or a crash.

The generation number is the fencing token. No result from generation `g` may
replace state accepted for a later generation. The PR row remains the authority
for the latest requested state. A completed database transaction is the durable
boundary; River jobs and external systems are mechanisms for converging toward it.

This is a transactional-outbox/saga design, not a distributed transaction.
PostgreSQL cannot atomically commit with Docker, Traefik, or GitHub. The design
guarantees durable intent, stale-generation rejection, safe retries, and eventual
repair. A temporary mismatch can remain visible during an external-system outage;
no design can promise simultaneous commits across these independent systems.

## The current implementation

The repository now has the database-only foundation for the target lifecycle:

1. A validated PR event increments `pull_requests.generation` and enqueues a River
   build/teardown job in one PostgreSQL transaction.
2. A worker captures the PR ID, generation, SHA, and state. It skips work that is
   already stale and builds outside a database transaction.
3. `ApplyResult` locks the PR row and atomically changes environment state only if
   the captured generation remains current. A successful build becomes a pending
   candidate; the previous active artifact remains recorded until activation is
   finalized.
4. `Repository.Apply` inserts the durable external-operation row and its River
   wake-up job in the same transaction as that state transition. It performs no
   external calls. Operations have idempotency keys, retry/terminal states, attempt
   counts, availability time, and expiring leases.
5. `OperationWorker` claims an operation in a short transaction, calls an
   `OperationExecutor` with no database transaction open, then finalizes the result
   in another short generation-checked transaction. External adapters are still
   absent; in particular route executors must implement the generation-fencing
   contract before they can be connected.
6. `Reconciler` recovers due operation wake-ups and schedules cleanup for
   unreferenced resources returned by a resource inventory interface. No production
   inventory, route inspection, or periodic runner is wired yet.

This removes the long-held PR lock and makes accepted external intent durable. It
does not claim exactly-once execution or an atomic PostgreSQL/external commit. Those
properties come from idempotent adapters, external fencing, and reconciliation;
the interfaces and database state machine are ready, but those real-system guarantees
are not validated until the adapters exist.

## Required guarantees

The target architecture must provide all of these properties:

- **Atomic desired-state recording:** the new PR generation and its durable work
  request either both commit or both roll back.
- **Stale-result fencing:** older generations cannot replace the latest desired or
  active database state, report a final success, or become the final route target.
- **Short database transactions:** no Docker, router, or GitHub network call occurs
  while a PR row lock or application transaction is held.
- **At-least-once safety:** duplicate jobs and retries are expected; repeating an
  operation converges to the same result and does not create duplicate routes,
  comments, containers, or cleanup records.
- **Crash recovery:** after a crash at any boundary, durable state lets another
  worker determine what remains to be done without guessing from process memory.
- **Blue/green preservation:** a previously healthy preview stays available while a
  candidate is built and checked. It is retired only after the replacement is
  accepted and traffic has switched.
- **Durable cleanup:** obsolete containers, images, and routes remain represented
  as pending cleanup until removal succeeds or an operator-visible terminal state
  is reached.
- **Reconciliation:** PostgreSQL intent and observed Docker/router state are
  compared periodically and repaired after missed jobs, ambiguous timeouts, or
  exhausted retries.

## Proposed components and ownership

### PostgreSQL desired state

`pull_requests.generation`, `head_sha`, and `state` describe what is currently
desired. The existing generation column remains the monotonic fence. Each accepted
event increments it exactly once, including close/reopen events. Webhook delivery
deduplication must happen before processing so a retried GitHub delivery does not
create a second logical generation.

Environment rows describe the last accepted/active deployment and separately store
pending deployment state. A candidate is not the active deployment merely because
Docker produced a container. Persist the candidate's generation, SHA,
container/image identity, and lifecycle state before asking the router to use it.

The foundation uses these environment states: `queued` means a PR exists but has
no active candidate; `publishing` m eans a healthy candidate is pending route
activation; `running` means the candidate's activation operation committed; `failed`
means the current build attempt failed while prior active and pending artifacts
remain protected;
`tearing_down` means a close has committed and route removal is pending; and
`torn_down` means route removal was accepted and teardown state was committed.
Pending candidate identity is stored separately from active artifact identity.
The status and pending columns are implemented now; production transitions still
depend on real build and route adapters.

### River jobs

River remains the worker scheduler and retry mechanism. A job carries stable IDs
(for example, an outbox operation ID or PR ID plus generation), not an ephemeral
process pointer. The database transaction that creates a durable operation also
inserts its River wake-up job with `InsertTx`. River delivery is at least once; job
completion alone is not the audit record for an external action.

Keep the existing per-PR uniqueness/handoff behavior for desired build and teardown
work. Build, teardown, publication, reporting, and cleanup use separate queues and
per-kind execution/rescue budgets as their work is implemented. A publication
operation must not be silently lost when a River job reaches its retry limit; the
durable operation row stays pending/failed for retry or reconciliation.

### Durable publication operations (outbox)

Add a table for external operations, for example `environment_operations` (the
final name can follow the schema conventions when implemented). It is a durable
state machine and audit trail, separate from River's expiring job rows. At minimum
it should record:

| Field | Purpose |
| --- | --- |
| operation ID | Stable identity carried by River jobs and logs. |
| PR ID and generation | Ownership and stale-work fence. |
| operation kind | For example activate route, report status, remove container, or remove image. |
| idempotency key | Stable key derived from PR, generation, and operation kind. |
| payload/reference | Candidate artifact and inputs needed to repeat the operation. Avoid secrets. |
| state | Pending, running/leased, succeeded, superseded, retryable failure, or terminal failure. |
| attempts and next-attempt time | Backoff and operational visibility. |
| lease owner and lease expiry | Recovery if a worker dies while holding an operation. |
| last error and timestamps | Diagnosis and cleanup/audit retention. |

Enforce uniqueness for the logical operation, such as `(pr_id, generation, kind)`.
Use short transactions to claim an operation (`FOR UPDATE SKIP LOCKED` or an
equivalent conditional lease update), record progress, and finish it. A lease is
not itself a fencing token: an expired worker can still have an in-flight network
request. External adapters must reject stale generations or be driven through a
single ordered controller as described below.

River and the outbox serve different purposes: River delivers/retries execution;
the operation row preserves durable intent and outcome after River retention or
exhausted attempts. Create/update the outbox row and its River job in the same
transaction so neither can commit alone.

### Build worker

The build worker fetches and builds the exact requested SHA without a PR-row lock.
It creates a candidate container that is not yet publicly routed, waits for health
checks, and tags/lables all resources with stable ownership data: PR ID, generation,
SHA, and operation/build ID. Tags must be generation-specific; a later build must
not overwrite an image another generation still uses.

After the candidate is ready, a short transaction locks the PR row and checks the
captured generation. If it is stale or the PR is closed, the worker does not create
a publication operation; it records/schedules idempotent cleanup for that
candidate. If still current, it records the pending candidate and creates the route
activation operation plus its River job in the same transaction. It commits before
any routing call. The old active artifact remains available during candidate build
and activation.

### Publication worker and route fencing

The publication worker processes the durable route operation outside a database
transaction. It rechecks that the operation is still relevant before making the
external call. After the call, a short transaction records the outcome only if the
generation is still acceptable. A late worker may record its own operation result,
but it must never overwrite a newer PR/environment state.

The route update needs a **real fencing point**. Checking PostgreSQL and then
issuing an ordinary router update is not sufficient: a newer event can commit
between that read and the router call. The routing boundary must accept the PR ID
and generation and enforce monotonic updates, using one of these equivalent
properties:

1. The routing API supports compare-and-set/versioned updates and rejects a
   generation older than the latest accepted generation for that PR; or
2. A single logical route controller owns all route mutations for each PR, stores
   the accepted generation durably, serializes operations, and will not apply an
   older generation after a newer one has been accepted. Its state transition is
   conditional on the current PR generation in PostgreSQL.

For the project's Traefik direction, treat Traefik's dynamic configuration as a
**materialized view**, not the source of truth. A route controller applies a
generation-fenced route target and reconciles Traefik to that target. If strict
per-request freshness during controller lag is required, traffic must pass through
a routing layer that checks the authoritative generation-fenced target (or the
chosen router must offer equivalent conditional update semantics). A plain
fire-and-forget Traefik config write cannot provide that strict guarantee by
itself. This constraint must be validated against the selected routing adapter
before claiming that old route updates can never briefly reappear.

Once a route is confirmed at generation `g`, a short DB transaction marks `g`
active only if the PR still desires `g`. If the guard fails, the old operation is
superseded and the latest generation remains queued for activation. Do not retire
the prior container until the replacement's active state is committed and route
convergence is confirmed.

### GitHub reporting worker

GitHub comments/checks are separate durable operations, never part of the route
transaction. Use a stable external identity per PR (for example, update the known
comment/check run) and an idempotency key per report intent. Before publishing a
final result, confirm its generation is still current. If GitHub is unavailable,
retry from the operation record. A crash after GitHub accepts the request but
before PostgreSQL records success is safe only if the retry updates the same
logical report rather than creating another one.

Reporting is eventually consistent. A GitHub outage can leave the PR page behind
the database temporarily; the durable outbox makes the desired report recoverable.
Do not claim atomicity between a GitHub API call and PostgreSQL.

### Cleanup worker and reconciler

Cleanup is also durable work. It removes only resources tagged to the PR and an
obsolete generation, and it removes a container before its exclusively owned
image. It preserves both the active artifact and a candidate still used by a
pending operation. Not-found is success. Failures remain retryable and visible.

A reconciler periodically compares desired PR state, active/pending environment
rows, operation rows, container/image labels, and the route controller's accepted
target. It repairs missing operations and mismatches, retries cleanup, and marks
resources safe to remove only when no active or pending generation references
them. Reconciliation is required even with River because a worker can crash after
an external action and before recording its completion.

## Target lifecycle

### New commit / rebuild

1. The webhook handler authenticates and deduplicates the delivery.
2. One transaction increments PR generation to `g`, stores the new SHA/state, and
   inserts the River build job. It commits and returns quickly.
3. The build worker checks desired generation, fetches the exact SHA, builds an
   unregistered candidate, starts it, and verifies health. No long DB transaction
   or PR lock is held during this work.
4. A short transaction locks the PR. If desired generation is no longer `g`, it
   schedules cleanup and commits without publication. Otherwise it persists the
   candidate as pending and inserts a route-activation outbox operation and River
   job atomically.
5. The route controller applies `(PR, g, candidate)` idempotently with generation
   fencing. It confirms route convergence.
6. A short transaction rechecks PR generation and records `g` as active. It creates
   report and old-artifact cleanup operations. If a newer generation won meanwhile,
   it does not set the old generation active and leaves the newer work to converge.
7. Reporting and cleanup workers retry independently. The reconciler repairs any
   incomplete steps.

The previous preview continues serving until the candidate is healthy and the
route switch is accepted. A failed build leaves the old active artifact intact and
creates a current-generation failure report. A stale candidate never becomes the
database's final active generation.

### PR close

1. The close event increments generation and stores the closed state in the same
   transaction that records teardown work.
2. Older build/publication operations fail their generation check or are rejected
   by the route controller's fence. In-flight builds clean their generation-owned
   resources rather than registering them.
3. Teardown creates a generation-fenced route removal operation. Only after the
   route controller accepts the close generation does cleanup remove the active
   container/image.
4. The environment is recorded as torn down with that generation; GitHub reporting
   is retried independently. The reconciler repairs missed teardown work.

## Failure and retry behavior

| Failure point | Durable state after failure | Recovery |
| --- | --- | --- |
| Before event transaction commits | No generation increment and no job | GitHub delivery retry may process it. |
| After event transaction commits, before build starts | Desired generation and River job exist | River retries/worker restart resumes it. |
| During image/container build | Desired generation exists; partial resources carry ownership labels | Retry is idempotent; cleanup/reconciler finds partial resources. |
| New event arrives during build | PR desired generation is newer | Completion fence rejects old candidate and schedules cleanup. |
| Worker dies after candidate is ready but before DB commit | Candidate is labeled but not active | Reconciler removes or safely resumes it; it cannot be considered deployed from process memory alone. |
| Worker dies after candidate/outbox commit, before route call | Pending operation and River job exist | River retries the same operation ID. |
| Route call times out with unknown result | Operation remains retryable; route generation is queryable | Retry same idempotency/fencing key, inspect actual route, then converge. |
| Route succeeds, DB completion commit fails | Route controller records accepted generation; operation remains retryable | Retry is idempotent; reconciler records/converges committed state. |
| GitHub accepts report, worker dies before recording success | Report operation remains retryable | Update the same comment/check identity, then record success. |
| Cleanup fails or worker is killed | Cleanup operation remains pending/retryable | Retry safely; do not report cleanup complete prematurely. |
| River exhausts retries | Durable operation remains visible and not silently forgotten | Reconciler/operator retry or repair; alert on age/attempt threshold. |

Retryable failures include temporary network/provider errors and timeouts. Invalid
configuration and other deterministic failures should be marked terminal for that
generation, reported once through durable reporting work, and not retried as if a
provider outage. A new PR generation may create fresh work.

## Example: A finishes after B

Suppose commit A is generation 10 and commit B is generation 11.

- If A is still building when B commits, A's candidate-completion transaction sees
  generation 11 and refuses to create A's publication intent. Its candidate is
  cleaned. B continues independently.
- If A's route operation was already accepted while generation 10 was current, B's
  commit makes 11 desired. The route controller processes/fences route operations
  in generation order; it will not accept a late generation-10 update after
  generation 11 has been accepted. B's operation eventually becomes the route.
- A's database completion uses a generation predicate and cannot overwrite B's
  environment record. GitHub reporting for A is skipped/superseded if 11 is
  current.
- If an external provider cannot fence versions, the adapter must serialize and
  reconcile its full desired state. In that case, eventual convergence is the
  guarantee; a brief external view of an older route during failure/recovery cannot
  honestly be ruled out. The routing implementation must choose and document the
  required guarantee before production use.

For a close event, the close generation is newer than any in-flight build. The same
fence rejects the candidate; teardown's generation-fenced route removal is the
latest desired route state.

## Foundation implemented before product integrations

These database/queue pieces are implemented and tested against PostgreSQL, River,
and deterministic fakes:

- Environment rows distinguish pending candidates from active artifacts and use
  `publishing`/`tearing_down` while external transitions are pending.
- Migration `0004_durable_operations.sql` adds the operation state machine,
  idempotency key, payload references, attempts, availability, lease, and safe
  error-code fields. sqlc queries support claim, completion, retry, supersession,
  recovery, activation, and teardown.
- `OperationBroker` inserts an operation and River wake-up in the caller's
  transaction. `Repository.Apply` performs only the generation-fenced state change
  and durable intent insertion; it makes no external call.
- `OperationWorker` leases work, applies per-kind timeouts and capped exponential
  retry delay, calls the executor outside a DB transaction, and finalizes with a
  short guarded transaction. Terminal failures remain in PostgreSQL with safe
  error codes. Build failure reports are emitted only on the terminal River attempt;
  intermediate attempts do not publish a final failure.
- Executor contracts define idempotency, route generation fencing, and resource
  cleanup. No concrete Docker, router, or GitHub executor is selected.
- `Reconciler` can recover due operation wake-ups and schedule removal of
  unreferenced resources from an inventory interface. It is bounded and restart
  safe at the durable-operation boundary, but is not wired to a production timer.
- Integration coverage proves generation/outbox behavior, latest generation wins,
  PR close defeats an in-flight build, cleanup retries, operation retry/finalize,
  and a blocked external call does not block a concurrent PR event.
- Structured logs include operation identity, kind, PR, generation, and safe error
  code. Metrics, alerting, exhaustive crash-point injection, and operator controls
  remain before production use.

The existing event-generation transaction, captured River arguments, job handoff,
atomic SQL fence, and generation-order tests remain. `Repository.Apply` no longer
calls a publisher inside the environment-result transaction.

## What must be implemented with the build/runtime/routing features

These parts depend on real adapter capabilities and cannot be honestly completed
with database-only fakes:

- **Docker/build integration:** deterministic PR/generation labels; resumable or
  discoverable partial builds; candidate startup and health checks; safe cleanup;
  cancellation and resource limits.
- **Routing integration:** a generation-fenced route controller or provider with
  specified compare-and-set/serialization behavior, route convergence checks, and
  a clear definition of when the old container can be removed. Validate whether
  the chosen Traefik integration can provide the desired strictness; otherwise use
  an authoritative routing layer whose target table supports generation fencing.
- **GitHub reporting:** stable comment/check identity, per-generation status rules,
  API rate-limit/backoff handling, and safe retries after an ambiguous response.
- **Crash recovery against actual systems:** prove reconciliation can list real
  Docker resources and inspect the actual route target after process restart.
- **End-to-end cutover and close tests:** build A, request B or close during each
  publication step, inject process/provider failures, and verify that the latest
  generation wins and obsolete resources are eventually removed.

Do not connect a real runtime until the selected adapters implement these
contracts. The short-transaction outbox boundary exists; route fencing semantics
still need to be selected and validated with the actual routing design before
production use.

## Guarantees and explicit limits

The design does not claim exactly-once external execution. It uses at-least-once
delivery plus idempotent operations, durable intent, generation fencing, and
reconciliation. It guarantees that an obsolete generation cannot become the
committed active database state and that accepted work is not forgotten after
process crashes. With a generation-aware route controller, it also prevents older
route commands from replacing a newer accepted route generation.

During an external outage, database desired state and a materialized external view
may temporarily differ. Reporting may lag, and a route provider may take time to
converge. The system should keep the last known healthy preview where safe, expose
the pending/degraded state, retry durably, and alert if convergence exceeds its
operational limit. If the product requires stronger guarantees than eventual
external convergence, the routing layer must enforce generation fencing on the
traffic path; an ordinary asynchronous config push is not enough.

## Related code and documents

- Current implementation and its SQL/test evidence: `internal/environments/`,
  `internal/webhooks/`, and [GENERATION_ORDERING.md](GENERATION_ORDERING.md).
- Product workflow and original queue/rebuild/teardown requirements:
  [DESIGN.md Sections 7, 13, 14, and 15](../DESIGN.md).
- Process/configuration, build timeouts, and local database setup:
  [Development setup](DEVELOPMENT_SETUP.md).
