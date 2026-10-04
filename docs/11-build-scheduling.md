# 11 · Build scheduling and worker handoff

How an authorized intent ([10](10-build-policy-and-intents.md)) becomes executable
work: durable deadlines, River wake-ups, atomic admission, one build per PR, handoff
to the next request and crash recovery. Everything here is implemented and tested
with real PostgreSQL and River; what is missing is the real build/runtime adapters.

## What runs in production today

The server constructs the **observation coordinator**: webhooks and policy changes
persist intent and wake-ups. Workers execute `ping`, PR discovery and mergeability
checks. Executable build, teardown, route and reporting workers are **not
registered**, so durable intent and wake-ups simply wait until capable adapters exist.

`NewBroker` can persist coordination wake-ups but **cannot dispatch executable
build/teardown work**. Its `RequestBuild`, `EnqueueTx` and `Dispatch` reject execution
with `ErrExecutionUnavailable`. `NewExecutionBroker` requires an environment service
with runtime/outbox dependencies and an operation worker with an executor. It is
used with test adapters in integration tests. Future production construction must
register capable build, teardown, wake and operation workers and configure all
queues on every consuming/maintenance process. Supplying no-op adapters to enable
production execution is not supported.

## State and scheduling

- `opened`, `reopened`, `synchronize`, and `closed` are supported parsed actions.
  `closed` with `Merged: true` stores `merged`.
- Every committed supported invocation increments PR generation once. Delivery
  authentication, deduplication and source ordering happen in webhook ingestion;
  stale deliveries are acknowledged without regressing state ([05](05-webhooks-and-pr-discovery.md)).
- Unsupported actions return an error without mutation, and the HTTP adapter
  acknowledges unrelated event types without processing them. Comments and reviews
  never reset a deadline.
- Manual observations track PR state and supersede stale active intent without
  authorizing a new build or creating an environment.
- Automatic observations record current intent with `available_at = now + 5m`.
  Each relevant event advances observed generation and replaces the request/deadline.
  Changing mode alone does not schedule existing PRs.
- `Broker.RequestBuild` resolves owner access/current SHA, applies the
  Build/Retry/Rebuild contract ([10](10-build-policy-and-intents.md)), and inserts its
  wake-up in the same transaction.
  Manual override is immediate. The low-level `IntentService.Request` remains a
  persistence API; future browser handlers must use the coordinator rather than
  bypassing wake-up/dispatch capability checks.
- Switching to Manual cancels unstarted automatic intent. Dispatch and worker
  admission recheck cancellation and access. Already-running current authorized
  work may finish; newer PR generation or request revision still supersedes it.
- Close/merge supersedes active build intent and records immediate teardown through
  the PR's closed state/generation and a River wake-up. No debounce applies.

The coordinator and service accept an injected clock for deterministic eligibility
checks. Tests advance a clock instead of sleeping five minutes. Production uses
`time.Now`; consuming hosts must maintain synchronized clocks.

## Wake-ups and River uniqueness

`environment_wake` runs on `environment_coordination` with a 15-second timeout
(rescue floor: 30 seconds). Its unique argument key is:

```
wake:pr:<id>:g:<generation>:r:<revision>
```

The explicit key is the only tagged unique argument. Uniqueness covers available,
pending, running, retryable and scheduled jobs. A new event/request creates a new
immutable wake-up; it does not reschedule/mutate an older job's captured arguments.
A duplicate wake-up for the current request coalesces. Old wake-ups check current
generation/revision and exit without execution. Completed/retained wake-ups do not
prevent recovery from inserting another wake-up for still-active durable work.

Build and teardown continue sharing **one executable PR key across kinds**:

```
environment:pr:<id>
```

Generation/revision do not participate in this executable uniqueness key. A
five-minute scheduled wake-up never reserves a build/teardown slot. If a wake-up
fires while an older executable job owns the slot, River returns that older row;
the newer intent stays pending. It is marked queued only when the returned job
actually matches the current generation/revision/state. An arbitrary duplicate
River insertion is not proof the new request was admitted.

## Atomic eligibility and worker claim

Mutations lock **installation → repository → PR → intent**. Parsed events acquire
installation/repository locks before their PR UPSERT. Mode changes, manual requests,
dispatch and admission follow the same order and recheck ownership/access after
locking. The PR lock serializes event updates and same-generation request revisions.

Dispatch reads intent under those locks, checks current generation/SHA, active
state, mode and deadline, inserts the executable River job and marks matching
intent queued in the same transaction. Worker start repeats the guard using a
conditional update, conceptually:

```sql
UPDATE build_intents b
SET state = 'running', started_at = COALESCE(started_at, clock_timestamp())
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = $pr
  AND pr.state = 'open' AND pr.generation = $generation
  AND pr.head_sha = $sha
  AND b.generation = pr.generation AND b.head_sha = pr.head_sha
  AND b.revision = $revision
  AND b.state IN ('pending', 'queued', 'running')
  AND b.available_at <= $now
  AND (b.source = 'manual' OR $locked_mode = 'automatic' OR b.state = 'running');
```

`ClaimBuildIntent` in `scheduling_queries.sql` is the exact sqlc query. Installation
status and repository access are checked while their locks are held. Zero affected
rows skips runtime execution. Allowing `running` supports idempotent rescued/retried
attempts and the approved mode-change behavior; it does not allow parallel logical
requests to bypass the River PR key.

After a successful claim, `ClearBuildLogs` removes logs for that PR's environment
inside the same transaction, before runtime execution. This also starts a fresh log
for an admitted runtime retry. Stale, cancelled, revoked or not-yet-due work cannot
clear logs. A deletion failure rolls back the running claim and skips the runtime.
Already-published/pending-candidate replays return before admission and preserve logs.
The future log writer must fence appends by the current request; no streaming or
Docker log adapter is enabled here.

Migration 9 adds unconditional environment uniqueness by PR. The existing row and
subdomain survive rebuild, teardown and reopen; fixture tests exercise this lifecycle.
Initial environment provisioning still belongs to the later execution integration.

## Completion, handoff and recovery

`finishJob` uses the same scope/PR lock. It completes or terminally discards the old
River row, updates only its matching current intent, and considers authorized work
again in one transaction. Completing the row releases the executable unique key.
A successor is admitted only if current and eligible. Future automatic intent keeps
its durable delayed wake-up; a newer Manual observation alone creates no successor.
A racing event is serialized before or after this transaction. Access revocation
still permits releasing the old River row but blocks successor admission.

Ordinary retryable errors retain River's backoff/attempt budget. Terminal failures
use River's pinned conditional discard API and the same handoff. Current success/
failure updates intent only for its generation/revision. Completion of an obsolete
job cannot change the newer request's lifecycle.

`RecoverScheduling` scans a bounded keyset page of current pending/queued/running
intent and outstanding closed PRs, recreating missing wake-ups. Its protected
in-memory cursor provides scan fairness, resets at the end, and carries no authority;
restart begins at the first page while durable rows remain the source of truth.
A closed PR's `teardown_completed_generation` records executable teardown handoff
completion, **not proof of external route removal**. Route removal still requires
its durable operation finalization/recovery. A no-environment close is safe to
complete without creating a preview record.

No production periodic runner is added. Future app construction must schedule
coordination and existing operation/resource reconciliation with bounded batches.
Recovery can resume an authorized running request after exhausted crash rescue;
actual adapters must make replay safe and discover partial resources.

## Same-generation retries and publication fencing

Migration 7 adds environment applied/pending revision, durable operation revision
and the teardown completion marker. Requests, operation payloads/keys and runtime
resource references carry revision. Operation keys now include generation **and**
revision, so explicit Retry/Rebuild of one SHA cannot collide with an old operation.

Result writes take the scope/PR lock **before** executing their guarded SQL. This
matters for a concurrent same-generation revision change: the SQL's intent lookup
must use a snapshot after the lock wait. The SQL also checks PR generation/SHA,
open/closed state and expected current intent revision. Route activation checks
pending generation/revision; finalization and reporting repeat relevance checks.
A revision change during an external call marks the old operation superseded and
cannot commit the old candidate as active database state.

Active and pending artifact references remain protected until replacement/teardown.
Cleanup respects generation/revision bounds, exact artifact identity and protected
operations, including leased external work. Inventory-derived records carry the
resource's captured revision. Cleanup generated from retired database references
uses the accepting operation's version as a conservative deletion ceiling; adapters
must verify PR ownership, actual labels and current references and must never delete
newer resources. Container/image identities must not be reused across revisions.

A retry after candidate/outbox commit detects its matching durable pending candidate
and avoids building another artifact for the same operation key. External calls
remain outside database transactions. Real adapters must atomically enforce the
same version fence/idempotency/compensation contract; a pre-call DB check alone
cannot prevent an obsolete external effect. Fakes prove database behavior, not
actual Docker/Traefik/GitHub guarantees.

Generation zero is valid for bootstrap/discovery state. Migration 7 relaxes the
old positive-only pending/operation constraints to nonnegative; job validation also
accepts zero. No synthetic lifecycle event is created. A real authorized build
still carries a positive request revision. Legacy open work without current intent
fails closed; existing deployed fields are preserved.

## Tests

Run `make migrate-up`, export `TEST_DATABASE_URL`, then:

```sh
go test -count=1 -race ./internal/environments
```

Covers Manual no-build, deadline reset, stale/early wake-ups, manual override,
duplicate requests, mode/access revocation, recovery, atomic rollback, concurrent
observation/request changes, same-generation stale success/failure/publication,
generation-zero finalization, real River wake-up processing and PR uniqueness,
future handoff deadlines, success/terminal-failure handoff and close-during-build
cleanup. Real source/build/runtime/router/reporting adapters are the next step
([16-roadmap.md](16-roadmap.md)).
