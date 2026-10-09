# 15 · Housekeeping, expiry and capacity

Previa keeps the real world matching the database without anyone watching it:
lost River jobs, crashed workers, missed webhooks, forgotten previews, orphaned
containers and full disks are found and repaired by bounded background jobs, and
builds and previews never take more of the host than it can give. This document
covers the owner decisions, the jobs, what each crash point leaves behind and who
resumes it, expiry and its feedback, orphan detection, PR reconciliation with
GitHub, pruning, capacity controls, dry runs, configuration and what was verified.
It extends the environments contracts ([17](17-build-scheduling.md),
[18](18-durable-publication-design.md), [13](13-teardown-and-cleanup.md)); the
code is `internal/housekeeping` plus capacity in `internal/environments`,
`internal/runtime` and `internal/builds`. The worker registers it whenever it can
build ([22](22-mvp-operations.md)); `make housekeeping-dry-run` runs it as a
diagnostic.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Expiry (owner decision) | `ENV_TTL_HOURS` (default 48) after the preview's **latest publication**. A rebuild that publishes restarts it; a failed attempt does not; teardown clears it; 0 disables expiry | Active PRs keep their preview; abandoned ones stop costing resources. The setting name is the one README §4.8 reserved. |
| Deleting orphans (owner decision) | Delete, but only objects older than a **15-minute grace period**; every decision is logged; `HOUSEKEEPING_DRY_RUN=true` logs and deletes nothing | A just-created object is never mistaken for an orphan, and operators can watch before trusting it. |
| PR reconciliation (owner decision) | Only PRs with **live work** (a preview or an unfinished build request), every 15 minutes, 10 per pass, each PR at most once per 15 minutes | Missed closes and new commits are caught without spending GitHub's rate limit on idle PRs. |
| Capacity full (owner decision) | **Wait and retry**: the request keeps its place and says "Waiting for capacity"; its job is snoozed with backoff (30 s doubling to 5 min) and spends no attempt; cleanup keeps its own queue | A busy host delays previews instead of failing them, and freeing capacity is never stuck behind waiting builds. |
| Instance identity (added, flagged) | Each database has an identity (`platform_instance`, migration 0016) in the label and name of every Docker object it creates; housekeeping, capacity counts per repository and removal only ever consider their own instance's objects | A test schema next to a development database (or two deployments on one host) could otherwise delete each other's previews when PR IDs coincide. |
| Expiry is a teardown request | Housekeeping calls `Repository.RequestTeardown(…, expired)` | The generation advances and the request is superseded, so nothing rebuilds the preview until an owner asks or a later relevant Automatic event arrives: no rebuild loops. |

## The jobs

All run on the `housekeeping` queue (one at a time, apart from builds, routes and
cleanup), once when a worker's River client starts leading and then on their
interval. At most one unfinished job of each kind exists; a failed pass is not
retried because the next one runs anyway. River runs periodic jobs only on its
elected leader.

| Job | Every | Does |
| --- | --- | --- |
| `housekeeping_recover` | 1 min | Recovers scheduling (wake-ups for due, queued and running requests and for closed PRs awaiting teardown), operations (lost wake-ups, expired leases), starts requested teardowns (revocations) and rewrites the router's file if it drifted. When a build waits for disk space, also prunes at once (at most every 5 min). |
| `housekeeping_resources` | 5 min | Inventories a batch of PRs' Docker objects and schedules cleanup of orphans; removes objects of PRs the database does not know. |
| `housekeeping_pull_requests` | 15 min | Confirms up to 10 live PRs with GitHub. |
| `housekeeping_expiry` | 10 min | Tears down previews past their expiry. |
| `housekeeping_prune` | 1 h | Untagged images, leftover source workspaces, build caches. |

Expired build logs keep their own hourly retention job (`build_log_prune`,
[08](08-build-logs.md)).

## Recovery: crash points and who resumes them

Recovery reads durable rows, never process memory, and never authorizes work:
every recovered build goes through the usual dispatch checks (Manual/Automatic
policy, deadline, generation, mergeability).

| What was lost | What remains | Resumed by |
| --- | --- | --- |
| A wake-up job (deleted, queue dropped) | A pending or queued request | Scheduling recovery inserts a new wake-up |
| A build job, or its final attempt died before the handoff (retries exhausted) | A running request | Scheduling recovery wakes it; dispatch inserts a new build job (River's uniqueness ignores discarded jobs); admission accepts the running request; an image built earlier for the revision is reused |
| An operation's job | A pending or retryable operation | Operation recovery |
| A worker during an operation | An operation with an expired lease | Operation recovery releases the lease |
| A revocation's teardown start | A teardown request | The teardown sweep |
| A close webhook | An open PR with live work | PR reconciliation records the close; scheduling recovery starts the teardown |
| The router's file or a Traefik restart | The accepted route table | Route sync |
| A candidate whose result was never applied | A labelled container | The resource inventory, after the grace period |
| An expiry pass | A preview past `expires_at` | The next expiry pass |

## Expiry and what reviewers see

- Publication (route accepted and confirmed) sets `environments.expires_at` to now
  plus the lifetime; a failed attempt leaves it; teardown clears it.
- The living comment of a live preview says *"The preview expires at 14:05 UTC on
  7 Oct 2026 unless a newer one is published."* After expiry it reads *Preview
  expired* with how to bring it back; an unfinished check run is stopped
  ([14](14-pr-feedback.md)).
- A preview whose PR has a pending, queued or running request is left until that
  request publishes (and sets a new expiry) or ends.
- After expiry, recovery never rebuilds it. A new commit on an Automatic
  repository, or an owner's Manual request, builds a new preview at the same
  address.

## Orphans

- **Instance identity.** Objects carry `io.previa.instance` and the instance in
  their names (`previa-<instance>-pr-<n>` networks, `previa/<instance>-pr-<n>:…`
  images, `previa-buildcache-<instance>-…` volumes). Inventory filters by it;
  removal, image reuse, the pre-publication check and network reuse require it.
  Advisory lock keys include it, because locks are shared by a database's schemas.
- **PRs the database knows.** The environments reconciler compares each object
  with the active and pending artifacts, those held by unfinished route operations
  and whatever the router may still serve, skips the current request's own build,
  and schedules durable cleanup operations (containers, then images, then
  networks) on the cleanup queue.
- **PRs it does not know** (for example after restoring an older backup): their
  objects are removed directly, containers first, after verifying their labels.
- **Grace.** Objects younger than `HOUSEKEEPING_GRACE` (by Docker's creation time),
  or whose creation time is unknown, are left alone.
- **Dry run.** Cleanup is planned in a transaction that is rolled back; every
  object is logged with its PR, generation, revision and ID.
- Objects created before migration 0016 have no instance label and are never
  touched by housekeeping; nothing deployed predates it, but remove such leftovers
  of local tests by hand (`docker ps -a --filter label=io.previa.managed=true`).

## Confirming PRs with GitHub

Each pass reads the least recently confirmed live PRs through
`VerifyUpstream` ([06](06-source-and-configuration.md)): one installation-token GET
per PR, recorded under the usual installation → repository → PR fences, so a
concurrent webhook wins. It never authorizes work and discovery stays open-only:
a PR missing from a listing is never taken as closed. Outcomes are stable
categories in `pr_reconciliations` (`current`, `changed`, `closed`, `blocked`,
`inaccessible`, `unavailable`, `error`). A rate limit stops the pass and leaves
that PR first in line. PRs of suspended installations or inaccessible
repositories are not read; an `inaccessible` answer tears nothing down, because
installation lifecycle events and repository sync are the authority on access
([04](04-github-sign-in-and-app.md), [13](13-teardown-and-cleanup.md)).

## Pruning

- **Untagged images** of this instance (an attempt for the same revision re-tagged
  its result), never forced: an image a container runs stays.
- **Source workspaces** older than `BUILD_TIMEOUT` plus the grace period: no build
  runs that long. Workspaces live on the worker's disk; with one worker host (the
  supported deployment) every leftover is found.
- **Build caches**: first those of repositories this instance can no longer build
  (removed, inaccessible or revoked), then the oldest unused ones until all fit
  `BUILD_CACHE_BUDGET`, or half of it when a build waits for disk space. A cache a
  builder holds is never removed.

## Capacity

| Control | How it holds |
| --- | --- |
| Global build concurrency (`MAX_CONCURRENT_BUILDS`, across every worker) | Admission takes a transaction-scoped advisory lock and counts running requests. A running request holds its slot from admission until its final attempt ends, across River retries and restarts. When all slots are taken the request stays queued, notes that it waits, and its job is snoozed. |
| Preview CPU, memory and per-repository previews | A session advisory lock (`runtime.AdmissionLock`) is held from the capacity check until the candidate container exists with its labels, so concurrent admissions on any worker see each other. Usage is read from the containers' labels: the active preview and its candidate both count during a swap, and capacity is released only when a container is actually removed. The per-repository limit counts PRs, so a PR's own swap never competes with its active preview. A worker that crashes loses its connection and with it the lock; there are no reservations to leak or reconcile. |
| Before admission | The runtime's `Admit` checks that at least the smallest preview (0.5 CPU, 512 MiB) would fit; if not, the request waits queued instead of building an image it could not start. |
| Backend previews | A candidate counts its app, processes, managed services and setup container together, all created (and counted) under the admission lock before any starts ([11](11-backend-previews.md)). |
| Disk | The builder requires `BUILD_MIN_FREE_DISK` plus `BUILD_DISK_BUDGET` for every other running build of this instance; builders exist before they measure, so two starting together count each other. A shortage is a wait (*Waiting for disk space*) and triggers an emergency prune. |
| After admission | If the preview budget fills while the image built, or disk runs short, the admitted request keeps its slot, says it waits and is retried; no failure is recorded and an image already built is reused. |
| Cleanup stays responsive | Route removal, cleanup and housekeeping each have their own queues; waiting builds never hold them up. |

Limits, stated plainly: CPU and memory budgets count every Previa preview on the
Docker host, whatever its instance, so a second instance's previews reduce this
one's room. A request superseded or torn down frees its build slot at once; its
build process notices within seconds and stops. Waiting requests are retried
with backoff, not in submission order, and no queue position is shown.

## Dry-run diagnostics

```sh
make housekeeping-dry-run                       # WORKSPACES=/abs/dir to include source workspaces
```

One inventory, expiry and prune pass against the configured database and Docker,
whatever `HOUSEKEEPING_DRY_RUN` says. Every decision is logged and nothing is
changed; recovery and PR reconciliation are skipped because they write state. It
ends with a summary such as:

```
Dry run for instance 40faee5f0a2d (nothing was changed):
  orphans of known PRs: 0 (0 objects younger than the 15m0s grace period left alone)
  objects of PRs the database does not know: 0 from 0 PRs
  previews past their expiry: 0
  untagged images: 0, leftover workspaces: 0, build caches: 0 (0 bytes)
```

## Configuration

| Setting | Default | Meaning |
| --- | --- | --- |
| `ENV_TTL_HOURS` | 48 | Preview lifetime after its latest publication; 0 never expires |
| `HOUSEKEEPING_DRY_RUN` | false | Log every deletion (orphans, expiry, pruning) and make none |
| `HOUSEKEEPING_GRACE` | 15m | Minimum age of an orphan (1m–24h) |
| `BUILD_CACHE_BUDGET` | 40GiB | All build caches together (at least 1GiB) |
| `MAX_CONCURRENT_BUILDS` | (required) | Builds running at once across all workers |

## Wiring

`internal/app/execution.go` composes it: `Repository.WithLifetime(cfg.PreviewLifetime()).WithBuildSlots(cfg.MaxConcurrentBuilds)`;
`Service.WithHeadroom(runtime)`; `runtime.Parts{Instance, Admission: runtime.NewAdmissionLock(pool)}`
and `builds.ConfigFrom(…, instance, …)` with `instance` from `database.Instance`;
`housekeeping.NewRecovery(broker, repo, sweeper, controller)`,
`NewResources(repo, runtime, …)`, `NewPullRequests(pool, syncService, …)`,
`NewExpiry(pool, repo, …)`, `NewPrune(builder, workspaces, …)` with
`SettingsFrom(cfg.HousekeepingConfig, cfg.BuildTimeout)`, gathered in a
`Housekeeper` whose `Register` is passed to `queue.NewWorker`. The housekeeping queue
has one worker per process. Jobs run every minute (recovery), 5 minutes (resources),
15 minutes (PR reconciliation), 10 minutes (expiry) and hourly (pruning), with
timeouts of 2, 5, 5, 2 and 15 minutes. The dashboard shows
the waiting reason and the expiry on PR pages, Builds and Environments.

## What was verified

- `go test ./internal/housekeeping` against PostgreSQL and River: expiry without
  rebuild loops (and a new commit bringing the preview back), dry runs, lost
  wake-ups and operation jobs, expired leases, a build whose retries were
  exhausted without its handoff, PR reconciliation (live work only, the interval,
  rate limits, safe outcomes) and a missed close found through the real
  `VerifyUpstream` that leads to a teardown job, orphan grace and dry runs, unknown
  PRs, cache budgets and in-use caches, and the disk-pressure prune.
- `internal/environments`: build slots under concurrent admission (6 requests, 2
  slots), waiting instead of failing for capacity and disk, headroom refusals, the
  backoff, expiry set by publication and kept by failures; `internal/runtime`: the
  admission lock serializes and survives a cancelled wait; `internal/reporting`:
  the waiting and expiry wording.
- Real Docker: `make test-housekeeping` (orphans of this instance removed after the
  grace period, the serving preview and another instance's container kept, an
  unknown PR's container removed, an emergency prune removing an unavailable
  repository's cache and an untagged image while keeping a cache in use) and
  `make test-runtime` (two candidates racing for room for one: exactly one starts).
  The builder, runtime and routing suites pass with instance-scoped names.

## Not done yet

- Capacity and disk pressure on a target host's real resources with
  `MAX_CONCURRENT_BUILDS` concurrent builds ([22](22-mvp-operations.md)).
- A queue position for waiting requests: waits are retried with backoff, not served
  in strict order, so none is shown.
- Metrics and alerts for disk pressure and stuck work (Prompt 11).
- More than one worker host: source workspaces and the route file are per host.
