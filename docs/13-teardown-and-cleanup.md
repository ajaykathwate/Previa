# 13 · Teardown and cleanup

When a pull request closes or merges, or GitHub access to it is revoked, Previa
withdraws its preview's route and removes every Docker object it created for that
PR, durably and without GitHub. This document covers the triggers, the states, the
order of removal, what is kept, how revocation works, why an old teardown can never
remove a newer preview, and how each step resumes after a crash. It extends the
existing teardown job, outbox and cleanup contracts
([19](19-generation-ordering.md)); there is no second teardown queue. The worker
registers the teardown, route, cleanup and sweep workers whenever it can build
([22](22-mvp-operations.md)); a teardown recorded while none runs waits durably.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| One teardown path | Close keeps its teardown job; revocation, owner requests and expiry record a **teardown request** and start the same transition | Every teardown moves the environment's artifacts into one durable route-removal operation; cleanup follows from it. |
| Fencing a teardown of an open PR | Requesting one advances the PR's generation and supersedes its build request | Every in-flight build, publication and report of an earlier generation is defeated by the existing fences, and nothing rebuilds until a new owner request or a later relevant Automatic event. |
| GitHub access | **Never needed** for teardown, route removal or cleanup | Revoked access must not strand a preview; locks follow the usual order without requiring an active installation or an accessible repository. |
| Removal order | Route first (confirmed by the router), then containers (graceful stop, then force), then images (never forced), then networks | No request reaches a removed container, and no image is removed while anything uses it. |
| Retry budget | Route removal and cleanup are retried until they succeed; a cleanup that failed is re-armed when the same resource is scheduled again | They are idempotent and must eventually happen; running out of River attempts leaves them retryable, visible and recoverable. |
| Kept | The PR row, its lifetime environment (and hostname), repository secrets, build caches | Reopening keeps identity and URL; secrets belong to the repository; caches are per source repository (housekeeping, Prompt 9). |

## The flow

```
close/merge webhook ──► teardown job ──► Service.Teardown ─┐
revocation webhook ──► teardown requests (same tx) ──► sweep ┤   one short transaction, no external call:
owner request (dashboard) / expiry ([15](15-housekeeping-and-capacity.md)) ─► RequestTeardown ─┘   environment ─► tearing_down at generation g;
                                                                its active and pending artifacts move into
                                                                remove_route(g, 0) in the outbox
remove_route ──► route controller: tombstone (g, 0) unless a newer route exists;
                 file written; router confirms the host is gone ([12](12-routing-and-https.md))
             ──► environment ─► torn_down (only if still in teardown g);
                 cleanup_artifact for every artifact the teardown took over
cleanup_artifact ──► runtime.RemoveResource: inspect, verify labels, stop, remove;
                     image after its container; the PR network once idle
```

### What each state means

| Field | Meaning |
| --- | --- |
| `environments.status = tearing_down`, `teardown_generation = g` | Teardown g started: the environment holds no artifacts; the route-removal operation and the route row protect them. |
| `environments.status = torn_down` | The route removal for that teardown converged and was recorded. **It does not mean Docker is clean**: cleanup operations may still be pending or retrying (visible in `environment_operations` with their `last_error_code`). |
| `pull_requests.teardown_requested_generation`, `teardown_reason` | A local teardown (`revoked`, `requested`, `expired`) was requested at that generation. |
| `pull_requests.teardown_completed_generation` | The teardown required at that generation was started (its durable operation exists). |
| `installations.cleanup_pending` | A revocation happened; cleared only when nothing of that installation's revoked PRs remains (below). |

## Ownership and safety

- **Verified by labels, never by names.** Before removing anything,
  `RemoveResource` inspects the object by its ID or reference and requires
  `io.previa.managed=true`, the right kind (preview, builder, image, preview or build
  network) **and** `io.previa.pull-request-id` equal to the resource's PR. Anything
  else is refused permanently (`not_owned`). An object that is already gone counts as
  removed, which is safe only because the ID or reference was recorded from a
  verified object.
- **References are protected.** A cleanup operation re-checks under the PR lock just
  before running and skips anything referenced: the active and pending artifacts,
  artifacts held by unfinished route operations, everything the router serves or may
  still serve (accepted, served and unconfirmed targets), and resources of the
  current request while its build is still running (unless that build scheduled the
  cleanup itself).
- **Containers:** stopped gracefully, bounded by their stop timeout (10 seconds for
  previews), then removed by force with their anonymous volumes.
- **Images:** removed without force after their container, so one still used by any
  container is kept (`in_use`, retried). Tags are per PR, generation and revision,
  so an image is never shared between previews.
- **Networks:** a build network or the PR's preview network is removed once no
  preview or builder is attached; the router and egress proxy are disconnected first.
  The preview network goes as soon as the PR's last preview is removed, and the next
  build recreates it.
- **Candidates (backend previews):** a candidate's managed services, setup and
  process containers carry its key and are removed with its app container, in
  one cleanup; the PR's services network goes with its preview network once idle
  ([11](11-backend-previews.md)).
- **No global prune**, ever. Unrelated containers and images, and another PR's
  labelled objects, survive (verified).

## Revocation

- Suspension and deletion of the installation, and removal of repositories from it,
  request teardown **in the same transaction** that records the revocation: every PR
  of the affected repositories that still holds a preview or an unfinished build
  request gets a new generation, a teardown request (`revoked`) and a superseded build
  request, and a sweep wake-up (`environment_teardown_sweep`) is inserted. Restoring
  access before the sweep runs does not cancel these teardowns.
- `TeardownSweeper.RunOnce` (bounded, idempotent) starts recorded teardowns without
  GitHub access. For each installation with `cleanup_pending` it also requests
  teardown of anything revoked now however access was lost, and clears the marker
  only when no teardown is unstarted, no environment is live in a revoked scope or
  still tearing down, no route removal or cleanup is unfinished (failed ones keep
  it), and Docker holds nothing for those PRs. Leftovers it finds are scheduled for
  cleanup. A lifecycle event that arrives meanwhile (the installation's revision
  changes) keeps the marker.
- Restored access never rebuilds by itself: Manual waits for an owner request;
  Automatic waits for the next relevant event.

## Reopening and newer work

- A teardown records a tombstone in the route row. A removal is relevant only while
  no newer route version exists: after a reopened PR's activation is accepted, the
  old removal is superseded and never touches the new route.
- The environment is finalized only by the teardown it is in (`teardown_generation`),
  so a late removal cannot mark a newer teardown or a newer publication `torn_down`.
- The artifacts an old teardown took over predate it, so they are cleaned up even
  when the removal itself is superseded; the reopened PR's artifacts are never among
  them, and cleanup's reference check protects anything newer regardless.

## Crashes and partial failures

| Crash point | Durable state | Resumption |
| --- | --- | --- |
| Before the teardown transaction commits | Nothing changed | The job or sweep retries. |
| After it, before the route removal | Environment tearing down; removal pending | The operation runs (River, or `RecoverOperations`). |
| After the tombstone, before the file write | Removal accepted | Retry finds it accepted, writes the file, confirms; `Sync` also repairs the file. |
| After the write, before confirmation | File current | Retry confirms. A router outage keeps it retryable; the containers stay until it converges. |
| After confirmation, before finalization | Route served as removed | Retry is idempotent and finalizes. |
| During cleanup (after a stop, after a removal) | Cleanup pending or retrying | Retry inspects again; gone means removed. |
| River attempts exhausted | Removal or cleanup still `retryable` | Recovery re-enqueues it; rescheduling re-arms a failed cleanup. |

## Secrets and data

Repository secrets are repository-level and kept (their lifecycle is the owner's,
[07](07-repository-secrets.md)). No environment-scoped copy exists in PostgreSQL:
decrypted values live only in the preview container's configuration, which is
removed with it (until then, anyone with Docker access can read them, as documented
in [10](10-preview-runtime.md)). Build logs follow their retention ([08](08-build-logs.md)).

## Owner teardown from the dashboard

**Tear down preview** appears on an open PR's page (and **Tear down** on its
Environments row) while it has a preview or unfinished work. It opens a confirmation
dialog; the confirmed form is a CSRF-protected POST to
`/repositories/{id}/pull-requests/{n}/teardown`. The handler resolves the PR for the
signed-in owner only (another user's PR is a 404), refuses a closed or merged PR
(those are torn down already) and calls `RequestTeardown` with reason `requested`.
That supersedes the build request; the worker then removes the route and the
containers, and the PR comment says the preview was removed. Repeating the request
records nothing new. A teardown needs no running worker to be recorded. Building
again brings the preview back at the same address.

The periodic sweep and reconciler run as housekeeping jobs
([15](15-housekeeping-and-capacity.md)); build caches of revoked repositories and
source-workspace leftovers are pruned there.

## Verified

`make test-routing` (real Traefik, previews and PostgreSQL; see [12](12-routing-and-https.md)):

| Scenario | Result |
| --- | --- |
| Close after a swap; a duplicate teardown | one removal; route withdrawn (unavailable page); everything removed |
| Router stopped during the removal | removal accepted but retryable; containers kept until it converged; finished after the router came back |
| Container already removed before its cleanup | counted as removed; its image then removed |
| Worker "crashes" after a removal, before recording it | the retry finds nothing and succeeds |
| Installation suspended (no GitHub client in the test) | teardown requested in the webhook transaction; the sweep starts it; marker cleared after cleanup; restored access does not rebuild |
| Close while the image builds | the build is refused before a candidate starts; its image is removed |
| Owner teardown through the worker's own pipeline (`make test-e2e`) | route and containers removed; the PR comment and check run say so |
| Afterwards | no container, image or network labelled for the PR; the router left its network; an unlabelled container, another PR's labelled container and image survive; PR and lifetime environment kept |

PostgreSQL tests cover teardown without access, duplicate and empty requests, the
revocation webhook and sweep (including a Docker leftover keeping the marker),
removal and cleanup never given up, re-armed failed cleanup, and the old teardown
after a reopen. `TestTeardownIsConfirmedOwnerOnlyAndIdempotent` covers the owner
endpoint (CSRF, owner only, idempotent, a fixed return path); browser tests cover
the dialog with and without JavaScript.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/environments/teardown.go`, `teardown_queries.sql` | Teardown requests, the start transition, revocation, the sweep. |
| `internal/environments/operation_worker.go`, `operation_finalize.go` | The operation worker; removal relevance and completion, superseded and failed outcomes, retry-until-done. |
| `internal/environments/repository.go` | Access-free teardown in `Apply`; reference protection in cleanup scheduling. |
| `internal/runtime/cleanup.go`, `inventory.go` | Owner-verified removal and the cleanup executor; inventory including networks. |
| `internal/webhooks/delivery.go` | Revocation requests inside the lifecycle transaction. |
| `internal/pullrequests/actions.go` | The owner's teardown endpoint. |
