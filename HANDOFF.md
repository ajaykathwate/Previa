# Project handoff
Last updated: 2026-10-14

## Current task
- Feature 1's shared workspace core is implemented and verified; references to
  review/assignment actions and usage/account lifecycle remain pending in Features 3/6.
  Do not resume unrelated earlier deployment/tunnel/roadmap tasks.
- README.md and DESIGN.md are source of truth; DESIGN section 21 records the
  approved access-model replacement. README-MVP-PRODUCT-EXPANSION.md is the
  six-feature specification; Features 2–6 remain planned, notifications deferred.
- docs/28-shared-workspaces.md: flows, roles, architecture, upgrade and limitations.
- Flat internal/<feature> packages; app composition in internal/app. Presentation
  imports no backend features. Preserve signal theme and accessible modal confirmations.
- Do not edit technical-docs/inside-previa.md unless the owner asks.

## Workspaces
- internal/workspaces owns creation/selection, membership/grants, expiring addressed
  invitations (digest only), consent, revoke/regenerate, leaving and two-step handover.
- Owner/Admin see all repositories; Developer/Reviewer require explicit grants.
  Only Owner appoints/demotes Admins. Reviewer gets a permitted preview directory.
- internal/platform/scope carries navigation selection only; each resource query
  rechecks actual workspace membership. Sessions store scalar selection, never roles.
- Source-sensitive actions require fresh GitHub human permission plus numeric ID;
  local Reviewer preview grants work without private-source access. App wiring uses
  consumer interfaces; external GitHub calls happen outside database transactions.
- Connection callbacks capture the chosen workspace once; existing installations
  cannot be claimed across workspaces. Installer user ID is provenance only.
- Membership changes serialize workspace → installations → PRs, invalidate manual
  authorization stamps and fence unaccepted candidates, including completed builds.
  Accepted routes belong to the workspace and preserve recovery/accounting after
  actor departure. Detached candidates become eligible for existing resource cleanup.
- Private cookies/tokens and live repository/log/output streams recheck local grants.
  Removed members cannot use GitHub outsider access to bypass offboarding.
  Private access works while an older preview serves during a replacement/failure.
- Shared quotas/fairness/consumption use stable workspace IDs; existing *_PER_ACCOUNT*
  config names stay. New containers carry io.previa.workspace-id; legacy user labels
  resolve through PR identity. Owner handover does not move historical consumption.
- web/pages/workspaces, web/components/workspace and web/styles/teams.css implement
  the selector, directory, members/settings, invitations and Reviewer directory.
  No-JS forms, responsive layouts and light/dark themes are supported.
- Native feedback/review assignments and durable workspace deletion/billing are
  labelled Coming next interfaces for Features 3/6; no future feature scaffold.

## Schema and running
- Go 1.26.9+, PostgreSQL 17; schema minimum is now Goose 30 (River 007 unchanged).
- Migration 0030 backfills one workspace per installer with connections, preserving
  repository/PR/environment IDs. Manual intents capture workspace and actor stamps.
- Coordinated cutover: back up, stop old server/Air and drain/stop old workers,
  make migrate-up, make build, restart only workspace-aware binaries. Old containers
  remain valid. Air does not apply migrations. See docs/02-development-setup.md.
- This task has used isolated test schemas; development/production DB cutover has
  not been performed. Last prior recorded development schema was 29 (2026-10-13).
- Solo rollback copies workspace usage to the legacy ledger. Invitations, extra
  memberships, new workspaces or handovers make rollback refuse data loss.
- Make run/migration commands load trusted .env automatically. Direct Go/test
  commands require manual exports; never print credentials. New browser target:
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-workspace-browser, with
  TEST_DATABASE_URL exported. Existing make test-browser remains available.
- WSL: use absolute Linux paths for caches/router state; Docker socket, PostgreSQL
  and local HTTP/Chromium tests require the authorized host execution context.

## Verification
- Full go test -race -count=1 ./... with TEST_DATABASE_URL: 1476 test/subtest passes,
  40 opt-in skips, no failures/races. Final affected-package refactor checks passed.
- go build ./... and go vet ./... passed; pinned golangci-lint: 0 issues.
- Browser: existing 55/55 and new team 4/4 passed under production CSP. Team suite
  uses real PostgreSQL, sessions and CSRF; GitHub responses are fixtures.
- New tests cover roles/forged IDs/CSRF, invitations, concurrent handover, removal,
  stale manual publication, accepted-route recovery, cleanup references, shared
  build slots, legacy container quota identity, private grants and stream closure.
- Real Docker runtime and backend/basic worker E2E checks passed: 15 test/subtest
  results, including HTTPS, rebuilding, reporting recovery and teardown.
- Tests found/fixed null-Origin CSRF rejection from no-referrer on team pages;
  same-origin referrer policy allows forms and hides invite URLs from external sites.
- Relative documentation links and git diff --check passed. Evidence:
  test-docs/2026-10-14-shared-workspaces-report.md.

## Existing baseline and remaining scope
- Current branch mvp-complete. User requested a local commit of workspace changes;
  no push/deployment requested. Port-8080 stop and fresh-reset instructions are in docs/02.
- Prior compatibility/build-secret/managed-service/log-viewer/routing improvements
  remain; docs/07–17 and test-docs/2026-10-13-regression-report.md describe that baseline.
- Public /open-source-tests still labels unverified application recipes In testing.
- Hosted production acceptance and live GitHub lifecycle proofs from older tasks
  remain separate. No live GitHub writes or webhook/tunnel changes in this task.
