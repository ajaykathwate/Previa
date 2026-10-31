# 33 · Workspace usage and the free plan

F3 implements expansion specification §9.3, §9.7 and delivery step H. Account
deletion, support/status and paid subscriptions remain deferred to their tasks.

## Plan and cutover

On 18 October 2026 the owner delegated the amounts and confirmed there are no
hosted users. The free early-access plan is **300 build minutes per UTC calendar
month, one concurrent build and two active preview slots per workspace**. One
build gives other teams a share of the host, two previews support parallel PR
review, and five hours of monthly execution bounds free compute without charging
anyone. Baseline refreshes share the same minutes and build slot. Existing host
CPU/memory/disk admission still applies.

Migration 0042 records the cutover time, installs the policy and gives every
active workspace a fresh entitlement. New workspaces receive it through an
insert trigger. The first of the next month at 00:00 UTC opens a fresh allowance;
ownership changes do not move usage. Historical `account_build_usage` and
`workspace_build_usage` remain labelled **legacy technical counters**. They are
not imported, displayed as current usage, or billed. There is no paid state,
purchase control, subscription or automatic overage.

Stop old execution workers before applying the migration, then restart the server
and workers. There is no new initialization command or configuration variable.
Make's migration/run targets load `.env`; direct Go commands and browser targets
require exported settings, including a disposable `TEST_DATABASE_URL`. See
[development setup](02-development-setup.md#f3-finite-free-plan-cutover).

## Accounting

`internal/usage` owns entitlements, usage periods, physical attempts and execution
segments. The existing admission transaction takes the global build-slot lock;
the workspace entitlement row serializes reservations, renewals and settlement.
Physical attempt UUIDs and unique segment sequences prevent duplicate settlement.
A retry that really executes is another physical attempt, including failed or
superseded work. Successful/stale job replay does not execute again.

Admission reserves at most 60 seconds in total. A reservation crossing UTC
midnight uses separate segments for each day/allowance period. Source preparation,
image building, startup, setup, seed and data-copy fallbacks run within the same
metered execution. Admission delay, queueing, quiet periods and snoozed capacity
waits consume no minutes. Starting execution rechecks its entitlement and replaces
the admission hold. Workers renew before expiry and settle elapsed microseconds;
the UI displays minutes to two decimal places. Completion releases unused time.

An independent timer cancels execution by the last committed reservation deadline
even if a database call stalls. Failed renewal cannot extend that timer. Accounting
outages become a retryable wait, and failed candidates are cleaned through the
existing fenced cleanup path. Cleanup, teardown and reporting run independently of
compute entitlement. The runtime's existing cancellation contract is unchanged.

Expired reservations are recovered at admission and by a River job every minute
(up to 100 workspaces per pass). Unstarted admissions consume zero. For a crashed
running attempt the last open reservation is charged through its bounded deadline,
never through recovery time; it is labelled **provisional**, since the exact crash
instant cannot be reconstructed. Late completion/repeated recovery cannot add it
again. Period totals must equal the sum of their authoritative segments.

## Limits and publication

The finite entitlement is always enforced. Existing operator keys
`BUILDS_PER_ACCOUNT` and `BUILD_MINUTES_PER_ACCOUNT_DAY` retain their names but add
workspace safety ceilings. Zero disables only that extra ceiling. Restart all
execution workers together after changing them; the worker records its actual
policy and checked time for the server to display. A platform safety cap cannot
be bypassed by a plan change.

Preview slots count distinct PRs with retained active/sleeping or pending previews
and admitted new PRs. A replacement of the same PR reuses its slot. Baselines use
compute without reserving a preview slot. Failed execution releases its hold;
serving previews remain until their existing expiry or explicit teardown.

Admission, execution start and route acceptance recheck current limits. Baseline
publication rechecks its compute entitlement too. Execution already accounted
within the allowance may publish at an exactly exhausted balance; a lowered or
restricted entitlement can delay publication. Limit waits snooze instead of
spending River retries. Fully consumed minutes wait for the real reset; minutes
held by another reservation are checked by its deadline, so they do not wait an
entire month when the hold can be released sooner. No queue position is invented.

## Usage screen and access

**Usage** opens `/workspaces/<public UUID>/usage`. Owners/Admins see plan, UTC
period/reset, consumed/reserved minutes, current builds and preview slots, extra
operator ceilings, real blocked work, pending/provisional recovery, measured time,
and reported preview lifetime, idle sleep and log retention. Unknown worker policy
is labelled unknown. Refresh works without JavaScript; there is no usage mutation.

Developers see technical usage and blocked work only for repositories they are
granted; they do not receive workspace totals. Reviewers cannot access usage.
Membership and repository grants are checked on every request, and responses are
not cached. Repository names and internal totals never enter GitHub reports;
reports retain only safe build-wait explanations.

At a limit, reading, feedback, account access, cancellation and teardown remain
available. No data is deleted because an allowance ran out. The page uses the
existing signal theme and renders exact UTC dates, with a stacked repository
breakdown on phones.

## Verification

`go test ./...` includes PostgreSQL accounting, replay, crash/boundary, concurrent
reservation, new-workspace/cutover, permission and complete environment admission/
publication tests. `make test-usage-browser` covers Owner, Developer and Reviewer
access, with/without JavaScript, CSP and desktop/mobile themes. The real Docker
end-to-end suite meters PRs and baselines and checks nonzero attempt totals and
period/segment reconciliation. Detailed gate results and manual rows are recorded
in the F3 report and the user guide.
