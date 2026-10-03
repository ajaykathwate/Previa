# Build policy and repository → PR → preview flow

## Decision and implementation status

The accepted build policy is **Manual by default for every repository**, with an
owner-controlled Automatic option. Automatic builds use a **five-minute trailing
quiet period**: each relevant new head commit resets that PR's deadline. Manual
builds bypass this delay, but still obey capacity, authorization and safety limits.

Policy persistence and ownership-scoped intent APIs are implemented by Prompt 2;
see [BUILD_INTENTS.md](BUILD_INTENTS.md). Prompt 3 implements coordination and
River scheduling behind tested interfaces; see [BUILD_SCHEDULING.md](BUILD_SCHEDULING.md).
Production preview dispatch is not connected.
Installation linking and paginated repository browsing work. Builds and Environments
are placeholder pages; PR browsing and manual build controls remain unbuilt. Production workers execute only ping.
The tested orchestration core now admits only current authorized eligible intent.
Production must supply real runtime/publication adapters before enabling dispatch.

The screen structure below is a **recommended next UI design**, not authorization
to implement new screens, routes, database columns or a PR synchronization runner.

## What each workspace page means

| Page | Responsibility |
| --- | --- |
| Overview | Real summary counts and current activity, linking to detailed pages. |
| Repositories | Connected repositories, current access, build mode and entry point to a repository's PRs. |
| Builds | Current/latest requested build work across repositories: waiting, queued, building, failed or completed, with commit and logs. |
| Environments | Preview deployments: URL, actually deployed commit, runtime state, health and expiry; open-preview and teardown actions. |
| Developer Tools | Developer diagnostics such as the ping round-trip; not a customer build workflow. |

A build produces an image and a candidate deployment. An environment is the
preview managed for a PR across rebuilds. PR #42 can have commit B building while
its environment still serves commit A. If B fails, the old healthy preview remains
available. Display latest PR SHA, requested build SHA and deployed SHA separately.
One status badge cannot accurately represent all three.

The existing MVP scope keeps one environment row per PR and only current-build
logs. It has **no persisted historical builds table**. The Builds page must not
promise a browsable history of every attempt. Adding durable build history requires
a separate scope decision; River job retention is not a substitute for that history.

## Recommended browser flow

1. Sign in → Overview → Repositories. Connect a GitHub App installation if needed.
2. Select an available repository → repository detail with paginated Pull Requests.
   Show full repository name, access and build mode at the top. Keep connection
   management in the header rather than the center of populated content.
3. List PR number, title, author, branch, latest SHA, PR state, current/latest build
   state and preview availability. Default to open PRs; provide search and explicit
   state filters. Follow the existing ten-row pagination and table-scroll pattern.
4. Select a PR → PR detail showing GitHub link, latest SHA, build request/status,
   current-build logs, deployed SHA and preview URL when available.
5. In Manual mode, choose **Build latest commit** for that PR. Show waiting/queued
   status immediately after the durable request is accepted. Repeated submissions
   must not create parallel duplicate builds for the same requested work.
6. In Automatic mode, show the pending deadline, such as “Waiting for changes to
   settle,” separately from the queue state. A manual action can request an
   immediate build; consume/coalesce matching pending automatic work.
7. Open the preview from PR detail or Environments. A close/merge removes it;
   expiry/manual teardown are also visible lifecycle outcomes.

Repository “Build” actions must choose a PR; do not silently build every PR or the
default branch. A PR with no build request has “Not requested,” not “Failed.” A
successful older preview should be identified as serving an older commit when the
PR has moved forward. Do not create an environment merely to display an unbuilt PR.

Suggested URLs, subject to implementation review:

- `/repositories/{repoID}` — repository detail and paginated PRs.
- `/repositories/{repoID}/pull-requests/{number}` — PR detail.
- `/builds?repo_id=...` — current/latest work filtered to a repository.
- `/environments?repo_id=...` — deployments filtered to a repository.

These routes do not exist today. Mutations use authenticated, ownership-scoped,
CSRF-protected POST requests. Destructive confirmation uses a modal. Reuse the
approved layout, palette, fonts, focus style, components and HTMX GET behavior.
Handlers stay feature-owned; view models and templates stay under `web/`.

## PR discovery is separate from build scheduling

Future authenticated webhooks record relevant PR state in both modes. Manual mode
means no automatic build, not no PR tracking. Comments/reviews do not trigger a
build or reset its timer. Closed/merged PRs invalidate pending work and request
teardown immediately in either mode.

Webhooks alone cannot show **all existing PRs** on a newly linked repository.
Recommended addition: an ownership-checked, bounded, paginated GitHub API sync for
initial discovery and explicit refresh, plus later reconciliation for missed events.
This sync must obey installation permissions, rate limits and event-ordering rules;
an older API snapshot must not overwrite a newer webhook state. Syncing PRs must
not implicitly authorize builds. The exact sync execution model and persistence
fields need review before implementation; do not perform an unbounded GitHub fetch
on every page render.

Until this exists, GitHub's repository Pull Requests page is the place to inspect
all PRs. Local PR browsing is not currently connected.

## Scheduling and ordering requirements

1. Store repository mode as an owner-controlled database setting. It is not a
   `.previewrc.yaml` field: a PR contributor must not enable automatic platform spend.
2. Preserve the monotonic PR generation for processed relevant lifecycle events.
   Distinguish **observed PR state** from **authorized build intent**. Merely
   observing a newer SHA in Manual mode must never authorize another build.
3. Persist the pending request identity, captured generation/SHA, source and due
   time in PostgreSQL. Migration 6 implements one current `build_intents` row per PR
   with a monotonic request revision; see [BUILD_INTENTS.md](BUILD_INTENTS.md).
   Automatic mode moves the deadline to five minutes after the latest relevant
   change. Older River wake-ups recheck durable eligibility and exit cleanly.
4. Record state, scheduling intent and necessary River wake-up in a short atomic
   transaction. Use delayed jobs/durable scheduling, not in-memory timers or workers
   sleeping through the quiet period. Recheck mode, access, PR state, generation
   and due time before permitting execution, including retries and recovery.
5. Keep one active build/teardown per PR. Change `finishJob` and terminal-error
   handoff to schedule only currently authorized, due work—not every newer observed
   generation. A pending future deadline remains durable until eligible.
6. A manual request resolves the current PR SHA under the ordering boundary and
   creates immediate intent without pretending a webhook occurred. Deduplicate
   repeated clicks. Ordinary Build coalesces with current work/result. Explicit Retry
   after failure or Rebuild after success requires a fresh key and expected revision;
   it advances revision even for the same SHA, without advancing PR generation.
7. Keep atomic result generation checks and durable publication. A new commit
   supersedes an older in-flight result even in Manual mode, but does not start
   the new build in Manual mode. Cleanup stale candidates; preserve an existing
   healthy preview until a newer authorized deployment succeeds.
8. A close/merge invalidates delayed and in-flight builds and records teardown
   immediately; there is no quiet period for teardown. Actual cleanup still follows
   the existing uniqueness, publication and resource ownership constraints.
9. Switching to Manual cancels unstarted automatic intent, including queued work
   that has not begun. It does not itself remove a deployed preview. Approved:
   an already executing authorized build may finish under its current PR generation
   and request guards; a mode change alone does not cancel running work.
10. Enabling Automatic applies to subsequent relevant events. It does not bulk-build
    existing PRs; those remain available for an explicit manual request.

Example: pushes at 12:00 and 12:04 in Automatic mode make the latest commit
eligible at 12:09. Eligibility is not a promise of execution at that instant:
capacity can add queue wait. Manual “Build latest commit” bypasses the quiet period.

## Tradeoffs and outstanding decisions

- Manual default and coalescing reduce unnecessary builds. Manual users must request
  updates; Automatic users trade immediate feedback for fewer intermediate builds.
- Continuous commits can keep postponing Automatic builds. The manual action is
  the immediate override; no maximum-wait policy is added in this decision.
- Runtime costs remain after building. TTL, per-repository limits and cleanup are
  still needed; debounce is not a spending cap.
- Tracking PRs and authorized work separately adds persistence and race tests,
  but is required to keep the existing successor mechanism from defeating Manual.
- Initial-sync mechanics, exact mutation routes and production runtime integration
  remain later tasks; River coordination is implemented behind tested interfaces. The current policy/intent schema is implemented; historical
  builds remain outside the agreed MVP scope. Current-only idempotency retention
  rejects stale commands through expected revision instead of an unbounded ledger.

## Implementation sequence and checks

1. Standalone `repoconfig` is complete; build mode stays outside repo YAML.
   Approved defaults, tiers and checks are in [REPO_CONFIGURATION.md](REPO_CONFIGURATION.md).
2. Complete: repository policy + durable request/deadline persistence. PostgreSQL
   tests cover concurrency, rollback, access checks and same-SHA request fencing.
3. Complete: parsed-event processing, broker handoff, retries and recovery enforce
   intent. Tests cover Manual no-build, deadline reset, stale wake-ups, manual
   override, cancellation, recovery, same-SHA revision and close-during-build races.
   Next: authenticated durable webhook ingress (Prompt 4).
4. Add PR discovery/list/detail queries and pages with ownership boundaries. Test
   existing-PR discovery, pagination and stale-sync/webhook races.
5. Add mode/build controls and connect real build/runtime/publication integrations
   in their planned phases. Cover duplicate clicks, access revocation, latest versus
   deployed SHA, failed rebuild preserving preview, logs and teardown confirmation.

Read this alongside README Sections 2.3–2.4 and DESIGN Sections 6, 7, 13 and 17.
The publication guarantees remain in [DESIGN_DURABLE_PUBLICATION.md](DESIGN_DURABLE_PUBLICATION.md);
current code behavior is documented in [GENERATION_ORDERING.md](GENERATION_ORDERING.md).
