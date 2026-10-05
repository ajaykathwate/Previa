# 13 · Build policy and build intent

Who decides that a pull request gets built, and how that decision is stored so it
survives crashes, retries and races. Scheduling the decision is
[14-build-scheduling.md](14-build-scheduling.md); executing it safely is
[15](15-durable-publication-design.md) and [16](16-generation-ordering.md).

## The decision

- **Manual by default** for every repository: PR events are tracked, but a build
  happens only when the owner asks for one on a specific PR.
- **Automatic is opt-in** per repository and uses a **five-minute trailing quiet
  period**: each relevant new commit resets that PR's deadline. Pushes at 12:00 and
  12:04 make the latest commit eligible at 12:09 (capacity can add queue wait).
- **Manual requests bypass the quiet period** but still obey capacity, access and
  safety checks.
- The mode is **owner-controlled platform data**, never a `.previewrc.yaml` field:
  a PR contributor must not be able to switch on platform spend.

### Why

| Concern | How the policy answers it |
| --- | --- |
| Cost and abuse | Nothing builds without owner consent; contributors cannot opt in. |
| Wasted builds during bursts of pushes | The trailing debounce builds the commit that settles, not every intermediate one. |
| Need for an immediate preview | **Build latest commit** skips the quiet period. |
| Predictability | Switching modes never bulk-builds or tears down existing previews. |

Accepted trade-offs: Manual users must click to refresh a preview; continuous pushes
keep postponing an Automatic build (Manual is the override, and there is no
maximum-wait rule); debounce is not a spending cap, so TTLs and per-repository
limits are still required ([19-roadmap.md](19-roadmap.md)).

## Observed state versus authorized intent

The central idea: **seeing a new commit is not permission to build it.**

- `pull_requests` holds **observed** state (head SHA, lifecycle, `generation`),
  updated by webhooks and discovery in both modes.
- `build_intents` holds the single current **authorized** request for a PR, with
  its own `revision`. Only an owner request, or an Automatic-mode event, writes it.
- A PR therefore has three commits that must be shown separately: the latest
  observed, the one requested for building, and the one actually deployed. A
  preview can keep serving commit A while B builds; if B fails, A stays up.

Product rules that follow:

- A PR with no request shows **Not requested**, never "Failed".
- Repository-level actions must target a PR; never build every PR or the default branch.
- Discovering or displaying a PR never creates an environment.
- There is **no build history**: one current intent and current-build logs only.
  River job retention is not a history either. Adding history is a separate decision.

## Behaviour rules

1. New and existing repositories default to Manual. Only the linked installation
   owner, with current repository access, can read or change policy or request work.
2. Enabling Automatic applies to subsequent events only; existing PRs stay as they
   are until someone requests them.
3. Switching to Manual atomically cancels **unstarted** automatic intent (`pending`,
   `queued`). Manual intent, running work and deployed previews are untouched; a
   running automatic build may finish unless a newer commit or request supersedes it.
4. Ordinary **Build** coalesces with current pending or running work and, after
   success or failure, returns the existing result instead of building again.
5. Explicit **Retry** (after failure) or **Rebuild** (after success) needs a fresh
   idempotency key and the current revision. It advances the revision even for the
   same SHA, without advancing the PR generation.
6. If the PR has moved to a newer generation, Retry/Rebuild of the old snapshot
   conflicts; use Build latest instead of silently retrying old code.
7. A manual request over pending/queued automatic work creates a new immediate
   manual revision; equivalent running work coalesces.
8. A new commit supersedes an older in-flight result even in Manual mode, but does
   not start a new build in Manual mode.
9. Close or merge invalidates delayed and in-flight builds and records teardown
   immediately, in either mode, with no quiet period.
10. Mergeability gates every request: Checking or Conflicts refuses the build
    ([05](05-webhooks-and-pr-discovery.md#mergeability)).

## Schema

`repos.build_mode` is `manual` (default) or `automatic`. Installation upserts,
renames and same-owner reinstalls preserve it; revoked or suspended access blocks
all policy and intent APIs.

`build_intents` is keyed by `pull_request_id`, giving one current row per PR:

| Fields | Purpose |
| --- | --- |
| `revision`, `base_revision` | Current request version and the version the command expected (`revision = base_revision + 1`). |
| `generation`, `head_sha` | The observed PR state captured by this request. |
| `source`, `request_action`, `requested_by` | Manual/automatic origin; build/retry/rebuild/automatic; the requesting owner. |
| `idempotency_key` | Opaque token for replaying the current command. Never logged or shown. |
| `state`, `available_at` | `pending → queued → running → succeeded/failed`, or `cancelled`/`superseded`; eligibility deadline. |
| `requested_at`, `started_at`, `completed_at`, `updated_at` | Current timestamps, not attempt history. |

Constraints enforce valid states and actions, positive revisions, source/actor
consistency (manual needs a user, automatic must not have one), start/completion
timestamps and a non-empty SHA. A partial index covers due pending intent.
Generation may be zero for bootstrap/discovery state; manual requests never invent
a webhook by incrementing it.

## Idempotency and same-commit fencing

A manual command carries the PR, action, expected revision and a 16–128 character
URL-safe key. Expected revision is 0 when no intent exists; it is **not** the PR
generation.

1. PR at generation 7, no intent. Build with expected revision 0 → revision 1.
2. Replaying that key/action/base returns revision 1, even after it completes.
3. Another current Build coalesces with revision 1's work or result.
4. After failure, Retry with a fresh key and expected revision 1 → revision 2, still
   capturing generation 7.
5. A stale command expecting revision 0, or an old transition for revision 1, cannot
   overwrite revision 2. Reusing the current key with a different action is a conflict.

Only the current key is stored; stale replays are rejected by their expected revision,
so no unbounded token ledger is needed. New requests use an UPSERT guarded by the PR's
current generation/SHA/open state and the expected revision. Lifecycle transitions
follow this pattern (the PR row is already locked in the same transaction):

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

Zero rows means superseded; it never grants authority to overwrite a newer request.
The revision then travels through job arguments, result and finalization guards,
operation keys, reporting and cleanup ([14](14-build-scheduling.md#same-generation-retries-and-publication-fencing)).

## Transaction rules

- Every mutation locks **installation → repository → PR → intent**, then rechecks
  ownership, installation status and repository access. Mode cancellation locks only
  affected PRs, in ascending ID order.
- The guarded SQL runs in that same transaction; external work never does. Lock
  waits honour the caller's context.
- `ObserveTx` composes with a trusted lifecycle update in one transaction and reads
  the locked PR row, never a caller-supplied SHA. Manual observations create no
  work and supersede stale intent; Automatic observations of a new generation set a
  deadline; a duplicate observation does not reset it. Discovery never calls it.

## Code

- `installations.BuildPolicyService`: owner-scoped mode Get/Set; mode change and
  automatic cancellation commit together.
- `environments.IntentService`: Get, Request, Advance, ObserveTx over a small store
  interface, independent of runtime adapters; `IntentRepository` owns the locks and
  guarded SQL. Browser handlers must go through the scheduling coordinator
  ([14](14-build-scheduling.md)), not the low-level `Request`.
- Migration `0006_build_intents.sql`; revision fencing extended in `0007`.

## Tests

```sh
TEST_DATABASE_URL=... go test -count=1 -race -run '^TestIntent' ./internal/environments
```

Covers defaults and constraints, ownership and revocation, mode preservation on
rename/reinstall, duplicate keys and actions, Retry/Rebuild, stale same-SHA
transitions, Manual observation, Automatic deadlines, manual override, mode switches
during pending/queued/running work, close/new-commit supersession, concurrent
requests and rollback after a failed cancellation.
