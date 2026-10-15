# Project handoff
Last updated: 2026-10-15

## Current state
- Branch `mvp-complete`, baseline commit 2b112db. All work below is uncommitted;
  the user has not asked for a commit or push.
- Codex's member/workspace task (names, invitation errors, 25+ repo picker, public
  UUIDs, PR descriptions, execution health) is finished and verified, together
  with the compact profile/settings/members redesign and editable profile photos.
- README/DESIGN remain source of truth (DESIGN workspace bullets now cover 0035,
  photos, the CSP image origin and in-place editing). No subagents authorized.

## UI direction (user requirement, also in memory)
- Pages must be compact and use horizontal space well (Jira-like density):
  side-by-side cards of label/value rows, in-place editing, small toast on save,
  errors inline, no tabs for related details, each fact shown once. Check
  1440px and 390px screenshots before calling UI work done.
- Shared pieces: `web/components/inline_edit.templ` (UserAvatar, Toasts,
  InlineText/InlineTextarea), `web/styles/fields.css`, ui.js `toast()`,
  `initInlineForms`, `initImageEditors`. Inline focus follows base.css quiet focus
  (ink-soft border, no glow). Team page spacing lives in `web/styles/teams.css`.
- Profile (`/protected`): Previa profile (photo, names, email read-only, member ID,
  joined, session/logout) beside GitHub account (username, ID, name, profile,
  company, location, bio, joined GitHub, last refreshed, Refresh).
- Settings: General (logo, name, description, ID) beside Overview; Ownership is a
  full-width row. Members: dense rows, no repeated workspace context line.

## Backend added this session
- Migration 0035: `user_avatars`, `users.avatar_version` (monotonic) and
  `users.custom_avatar`; backfills never-edited blank names from GitHub name.
  UpsertUser also prefills when profile_revision=1 and names are blank.
  `database.SchemaVersion` = 35.
- CSP img-src adds `https://avatars.githubusercontent.com` (root cause of the
  missing profile picture). Constant: `httpx.GitHubAvatarOrigin`.
- auth: `/protected/profile/avatar` (multipart upload), `/avatar/remove`,
  `GET /avatars/{userUUID}` (self or current teammates via consumer interface
  `AvatarAudience.SharesWorkspace`, wired in server.go; others 404).
- In-place saves: `Accept: application/json` → `httpx.FormResult` (message,
  error, revision, fields) from profile name, photo, workspace details/logo.
  Plain forms still redirect with `?saved=` (ui.js strips it after the toast).
- Settings no-JS errors carry `Page.ErrorField` ("logo"/"details") so they render
  once beside their field.

## Environments and verification
- Dev DB (previa-postgres-1) reached 0035 at 13:53 via the user's own `make dev`
  restart; do not run migrations on it yourself.
- Dedicated test DB container `previa-member-ux-test-postgres` (loopback 55439,
  db previa_ux_test, user previa/previa-test) is migrated to 35. The auth
  persistence test uses its public schema, so keep it migrated. Stop/remove only
  this container when the user is done with it.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium` (Playwright's own cache is
  incomplete). GOCACHE=/tmp/previa-product-spec-go-cache.
- Verified this session: go build, go vet, golangci-lint (0 issues), full Go suite
  with TEST_DATABASE_URL, workspace browser suite 11/11 with screenshots, UI
  browser suite (see final summary for counts).
- Screenshots: web/tests/browser/artifacts/workspace-{member-profile*,logo,
  inline-edit,toast,members-*,settings-*,directory}.png.

## Existing access invariants (unchanged)
- Active workspace membership is authoritative. Owner/Admin span all repositories;
  Developer/Reviewer require explicit grants. Sessions remember selection, not roles.
- Source-sensitive operations check fresh GitHub numeric identity and readable
  permission; GitHub permission checks use the GitHub login, never display names.
- Membership mutations serialize workspace → installations → PRs, fence unfinished
  manual candidates and revoke private cookies/streams.
- Invitations use digest-only, addressed seven-day consent/revoke/regenerate flows.
  Only Owner appoints/demotes Admins; ownership handover requires acceptance.
- Profile/logo/photo edits never change authorization revisions or cancel work.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- User review of the redesigned pages; apply the same density rules to any other
  page they flag (Overview, Repositories, Builds were not part of this pass).
- Commit only when asked, on the current branch (see memory for push rules).
