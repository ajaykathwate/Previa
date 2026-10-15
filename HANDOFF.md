# Project handoff
Last updated: 2026-10-15

## Current state
- Branch `mvp-complete`, all work committed (not pushed). Earlier: b863def (UUIDs,
  PR descriptions, execution health), 380b3d5 (profile/settings/members + photos),
  5de8338 (docs). Then: UI shell (headings, sidebar, create dialog), member
  lifecycle (0036), PR head commit (0037), docs. Commit only when the user asks.
- README/DESIGN remain source of truth (DESIGN §21 bullets cover 0035–0037,
  deletion scope, in-app invitations, derived activity, heading pattern).

## UI direction (user requirements; also saved in memory)
- Compact, Jira-like pages: side-by-side label/value cards, in-place editing with
  a toast, inline errors, no tabs for related details, each fact shown once.
- Page headings: `components.PageTitle(title, info)` — 22px title plus an info
  tooltip holding the description (no eyebrow, no paragraph). Used on Overview,
  Repositories, Builds, Previews, Starter kits, Developer tools, FAQ and team pages.
- Interactions: modals open over the current page (create-workspace dialog lives in
  the app shell, `components.CreateWorkspaceDialog`); navigation only after submit;
  ui.js closes menus on link click/submit and closes menus + dialogs on `pageswap`.
- Sidebar: brand, workspace switcher (pinned, absolute menu; rail menu opens beside
  the rail), primary nav, "Workspace" group (Members, Settings), Developer tools and
  Help & FAQ at the bottom. Quick jump and "Preview control plane" were removed.

## This session's backend
- 0035 photos (`user_avatars`, `users.avatar_version/custom_avatar`), CSP allows
  `https://avatars.githubusercontent.com`; `/avatars/{uuid}` for self + teammates.
- 0036 member lifecycle: removal records `removed_by` + `removed_repository_ids`
  (Deactivated tab, re-invite prefill via `?view=deactivated&reinvite=<uuid>`);
  in-app invitations (`ReceivedInvitations`, POST
  `/workspace-invitations/received/{invitation uuid}`, switcher badge via
  `WorkspaceShell.Invitations`); member activity query `MemberActivity` (builds,
  joined/created, invited, left/removed; filtered by viewer access); soft workspace
  delete (`Service.Delete`: owner, sole active member, no active/suspended
  installation, exact name). Create redirects to `/workspaces/{id}/setup`.
- Member profile page: GET `/workspaces/{id}/members/{user}` (deactivated profiles
  managers only). Own profile shows Recent activity (auth `ActivityFeed` interface).
- 0037 PR head commit cache (`pullrequests/commits.go`, `GitHubSource.Commit`,
  2.5 s cap, 10 min retry back-off). PR page: Description under status; commit
  message under Latest commit in Details. `SchemaVersion` = 37.
- Member changes that fail re-render Members with an inline error (no dead-end page).

## Dev database (repaired 2026-10-15, with the user's go-ahead)
- previa-postgres-1 had applied an early draft of 0030 and lacked
  `workspace_request_published` / `workspace_publication_authorized` (broke member
  removal and River builds). Both were recreated from 0030; its public schema now
  matches a fresh migration exactly at version 37. 26 earlier `build` jobs stay
  discarded (not touched); Retry build on those PRs re-queues them.
- Two leftover `ordering_test_*` schemas remain in the dev DB (not touched).

## Environments and verification
- Test DB container `previa-member-ux-test-postgres` (127.0.0.1:55439,
  previa_ux_test, previa/previa-test), migrated to 37; the auth persistence test
  uses its public schema. Remove only this container when the user is done.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium`. GOCACHE=/tmp/previa-product-spec-go-cache.
- Verified: build, vet, golangci-lint (0 issues), full Go suite, workspace browser
  suite (14 scenarios) and UI browser suite (55) — see final summary for counts.

## Access invariants (unchanged)
- Active workspace membership is authoritative; Owner/Admin span all repositories;
  Developer/Reviewer need explicit grants. GitHub checks use the GitHub login.
- Membership mutations serialize workspace → installations → PRs and revoke
  private cookies/streams; invitations are digest-only, addressed, seven-day.
- Profile/logo/photo edits never change authorization revisions.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- User review of the latest UI; push only when asked (to `main`, never `master`).
