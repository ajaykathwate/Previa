# Project handoff
Last updated: 2026-10-15

## Current task and sources
- User requested workspace UI/UX refinements: separate direct Members/Settings,
  aligned switcher, capsules/CTAs, no duplicate Previews tab, editable identity/logo.
  Implementation, regression checks and documentation are complete.
  Do not resume unrelated earlier deployment/tunnel/roadmap tasks.
- README.md and DESIGN.md section 21 are source of truth. docs/28-shared-workspaces.md
  records flows and boundaries. docs/02-development-setup.md records schema/startup.
- Flat internal/<feature> packages; presentation has no backend imports. Preserve
  signal tokens and accessible current-screen modal confirmations. No new library.
- Do not edit technical-docs/inside-previa.md unless the owner asks.

## Workspace state
- One workspace is one team with multiple members/repositories; users can belong
  to several workspaces. No nested teams/channels are implemented.
- Members and Workspace settings are direct sidebar/switcher/palette destinations.
  Members owns people, invitation list/search and access dialogs; Settings owns
  name/description, logo and ownership. Settings requires current Owner/Admin.
- Previews remains at /previews; Reviewer uses a read-only presentation of the
  pullrequests scoped activity query. Legacy workspace-preview URLs redirect after
  membership checks. The duplicate workspace query/tab has been removed.
- Presentation: web/pages/workspaces/{index,members,settings,shared}.templ,
  web/pages/previews/reviewer.templ, shared workspace components, teams.css and ui.js.
  Switcher SVG/grid alignment and stacking keep its menu above page content.
- internal/workspaces/profile.go and profile_handler.go own bounded PNG/JPEG
  upload/re-encoding, protected reads, metadata and stale-form handling. Profiles
  have a separate revision: branding edits do not revoke preview access/cancel work.
- Members and invitations have literal case-insensitive username search and
  database pagination. Admin repository access is explained in the access modal.
  Ownership selection now lives inside the nomination modal. No-JS forms work.

## Existing access invariants
- Active workspace membership is authoritative. Owner/Admin span all repositories;
  Developer/Reviewer require explicit grants. Sessions remember selection, not roles.
- Source-sensitive operations additionally check fresh GitHub numeric identity and
  readable permission; local Reviewer preview grants work without source permission.
- Membership mutations serialize workspace → installations → PRs, fence unfinished
  manual candidates and revoke private cookies/streams. Accepted serving routes,
  recovery and captured usage remain with the workspace after actor departure.
- Invitations use digest-only, addressed seven-day consent/revoke/regenerate flows.
  Only Owner appoints/demotes Admins; ownership handover requires nominee acceptance.
- Quotas/fairness use stable workspace IDs. Existing *_PER_ACCOUNT* config names
  remain; old container labels resolve through PR identity. Installer is provenance.
- Native reviews/assignments and account usage/deletion/disconnection remain the
  existing Features 3/6 boundaries. Their unbuilt settings controls are omitted.
  Notification integrations remain deferred. No later feature scaffold added.

## Schema and running
- Go 1.26.9+, PostgreSQL 17; minimum Goose 31, River 007 unchanged.
- 0030 is the coordinated access-model cutover; backfills preserve resource IDs.
  Stop old owner-based server/worker before upgrading; see setup documentation.
- 0031 adds description, profile_revision and workspace_logos. Stop server/worker,
  make migrate-up, make build, restart both. Air does not apply migrations.
  No new env/service; PNG/JPEG input max 192 KiB/512px, existing body cap 256 KiB.
- 0031 Down drops profile data. 0030 rollback refuses lossy team rollback once
  invitations/memberships/new workspaces/handovers exist; solo usage is preserved.
- Development DB was not migrated/restarted in this task. Docker and development
  PostgreSQL were unavailable in this WSL session. Tests used a disposable native
  PostgreSQL 17 C.UTF-8 instance under /tmp; it was stopped and removed after checks.
  Development data was not altered.
- Make run/migration commands load trusted .env; direct Go/tests require exports.
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-workspace-browser requires
  TEST_DATABASE_URL. Existing make test-browser remains optional and database-free.

## Verification and delivery
- Generated assets/templates/sqlc, go build ./... and go vet ./... passed.
  Final pinned golangci-lint: 0 issues. Full Go suite: 1481 test/subtest passes,
  40 opt-in skips, no failures (PostgreSQL C.UTF-8).
- Race checks passed for workspaces, auth, pullrequests and app. Initial full race
  run was terminated externally without a test failure; do not claim full race pass.
- Browser: existing 55/55, workspace 7/7 and final navigation/accessibility 6/6
  passed. Screenshots were visually inspected and remain ignored artifacts.
- First disposable DB used C locale; one existing Unicode search case failed.
  Final checks use C.UTF-8. New profile/search/migration/CSRF tests passed.
- Evidence: test-docs/2026-10-15-workspace-ux-report.md.
- Branch mvp-complete, following adfef83 feat: add shared team workspace access.
  User authorized a local commit of the completed UI refinements; commit title:
  feat: refine shared workspace navigation and settings. No push/deployment requested.
- Previous core report: test-docs/2026-10-14-shared-workspaces-report.md. Its Docker
  and real runtime proofs are historical; this UI task does not repeat them or
  claim hosted/live GitHub acceptance. No live GitHub writes or tunnel changes.
