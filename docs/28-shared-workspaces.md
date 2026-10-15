# 28 · Shared team workspaces

Previa's customer boundary is a workspace. A GitHub installation belongs to one
workspace; the person who originally connected it is connection history. A team
can keep its repositories, preview addresses and consumption when that person
hands over ownership or leaves.

## User flow

Sign in with GitHub, create a named workspace or open an invitation. New users
without connections land on Workspaces and can join without creating a personal
workspace. Existing installers receive a workspace containing their existing
connections. The sidebar selector opens only workspaces with active membership.
Selection is remembered in the current session, with a saved default after an
explicit switch. Opening a resource selects its actual workspace for navigation;
selection never proves access to that resource.

Owners and administrators use Repositories to connect code through the existing
GitHub verification flow. The connection captures the selected workspace before
leaving Previa. Switching teams during GitHub authorization does not change that
capture. Reconnecting or replacing an inactive installation preserves repository
IDs only within the same workspace. A connection already claimed by another team
cannot be reassigned through this flow.

Open **Members** directly in the sidebar to invite a GitHub username, select a role and
choose repositories. Previa resolves the immutable GitHub account ID and displays
an invitation link once. Copy and share it manually. Links expire in seven days;
regeneration invalidates the old link. Only a digest is stored, and request logs
redact invitation paths. The signed-in recipient sees the role and exact repository
scope before accepting or declining. Repeated acceptance is idempotent, and cannot
restore removed membership or replace a newer role. Invites issued by someone who
loses invitation authority are revoked.

## Navigation and workspace identity

One workspace represents one team. It can contain many members and repositories;
there are no nested teams or channels. A person can join several workspaces and
switch with the sidebar selector. Its aligned chevron, role capsule, full-name
tooltip and current-workspace indicator make the selected scope visible. The menu
also links directly to members, settings and the workspace picker.

**Members** handles people and access. Its Members and Invitations views keep
pending invitations separate from joined people. Search matches GitHub usernames
literally, ignoring case, and works across the full paginated list. Members have
role capsules and repository summaries. Manage access opens one dialog containing
the role and grants. Admin roles explain their all-repository access; Developer
and Reviewer roles show explicit checkboxes. Owners can appoint Admins. Removing
or leaving requires a confirmation modal over the current page. Only managers see
invitations and invitation actions.

**Workspace settings** handles identity and ownership. Owners/Admins can edit the
name (1–80 characters), optional description (up to 240 characters), and logo. A
summary shows the actual owner, role, member/repository counts, creation date and
copyable workspace ID. Upload PNG/JPEG up to 192 KiB and 512 × 512 pixels; square
images work best. Preview a selected image, upload it, or remove it to restore the
initial. No public image URL or image service is involved. Settings is hidden and
inaccessible to Developers/Reviewers. A nominated Admin gets a handover link on
Members; the nomination and acceptance controls live under Settings → Ownership.

The primary CTA on Members is Invite teammate; Settings has a Manage members link.
The command palette exposes both pages. **Previews** is the sole preview listing;
Reviewers get a read-only version there without source/log/build controls. Older
`/workspaces/{id}/previews` bookmarks select an authorized workspace and redirect
to `/previews`. No duplicate Previews tab is rendered inside settings.

Forms work without JavaScript; JavaScript adds role explanations, unchanged-save
detection, image preview and menu positioning. Invalid profile inputs retain page
context and typed values; a stale profile submission shows current details with a
409 response so the user can review before trying again. Profile and logo edits
do not sign people out of previews or invalidate in-flight manual requests.
Light/dark palettes and narrow layouts use the existing signal tokens.

## Roles

| Permission | Owner | Admin | Developer | Reviewer |
| --- | --- | --- | --- | --- |
| Repository scope | All | All | Explicit grants | Explicit grants |
| View permitted previews | Yes | Yes | Yes | Yes |
| Dashboard, PR/build metadata | Yes | Yes | Granted repositories | Permitted preview directory |
| Build, retry, teardown, sync, inspect build/app logs | Yes, with GitHub source permission | Yes, with GitHub source permission | Granted repositories, with GitHub source permission | No |
| Connect code, secrets, outbound, private/Automatic policy | Yes | Yes | No | No |
| Invite/manage Developers and Reviewers | Yes | Yes | No | No |
| Appoint or demote Admins | Yes | No | No | No |
| Nominate the next Owner | Yes | No | No | No |
| Leave | After handover | Yes | Yes | Yes |

Source-sensitive operations verify a current readable GitHub permission using
App credentials and the human's numeric GitHub ID. A reclaimed username does not
inherit another account's permission. GitHub outages fail these operations closed
with a retryable explanation. Local membership reads and reviewer previews remain
available. Administrators can still delete secrets or withdraw outbound approvals
when source access is unavailable. Account lookup can be temporarily limited by
GitHub's API; no invitation is saved when lookup fails.

GitHub's collaborator-permission response includes the user ID and supports App
installation tokens with Metadata read permission; Previa compares that ID before
accepting the returned permission. See the [GitHub API contract](https://docs.github.com/en/rest/collaborators/collaborators?apiVersion=2022-11-28#get-repository-permissions-for-a-user).

Developers execute repository code. That code can receive deliberately configured
runtime secrets; hiding the Secrets tab does not prevent code from reading its
runtime environment. Reviewers intentionally receive preview disclosure, including
private previews, without needing private-source permission.

Role and grant edits require confirmation over the current screen. Removing a
member clears grants and ends dashboard access. Saving unchanged access is a
no-op. Changed membership revisions fence unfinished manual requests, including
finished builds awaiting route acceptance. Unaccepted candidates are detached;
the existing resource reconciler cleans their containers and images after its
grace period. A candidate already accepted by the durable route fence stays with
the team, including recovery after a routing-worker crash. An older serving
preview remains available when its replacement is cancelled. Member/history rows remain for
attribution. Native comments, review decisions and assignment offboarding belong
to the later review features and are not fabricated here.

## Ownership and protected access

The Owner nominates an existing Admin in Settings. That Admin must confirm
acceptance before the seven-day deadline. A transaction demotes the previous Owner
to Admin and appoints the new Owner; database constraints preserve exactly one
active Owner. Stale or concurrent confirmations cannot appoint a second Owner.
Membership edits invalidate pending proposals. The former Owner can then leave.
Workspace IDs, installation links, quota identity and existing preview URLs remain.

Private-preview authentication rechecks PostgreSQL on every router request,
including token redemption. Cookies include workspace authorization revision and
whether the grant came from local membership. Removed members cannot fall back to
GitHub outsider access. Changes can require sign-in again despite the cookie's
12-hour expiry. GitHub-readable outsiders retain preview-only access after a fresh
permission check at sign-in; their source-only cookie expires within 12 hours and
is also invalidated by workspace authorization changes. Public previews retain
their public-address behavior.
Private authentication also works while a replacement is publishing or failed;
the route table determines what is live, rather than the latest build's status.

Repository event streams, build logs and app output check local access before
sending subsequent frames and during idle heartbeats. Removal stops further data;
it cannot retract output already delivered to a browser.

## Implementation

`internal/workspaces` owns handlers, services, queries and membership rules.
`internal/platform/scope` carries verified selection only. Display data enters
`web/components` through context; templates never query backend features.
`web/pages/workspaces` and `web/styles/teams.css` reuse the signal palette and the
existing shell. The selector and member/settings layout follow familiar team
workspace patterns; no chat/channel system or new UI library is introduced.
Members, pending invitations and previews use database pagination.
Migration **0031** adds workspace descriptions, profile revisions and separate
logo storage. Name/description/logo writes lock the workspace and recheck current
Owner/Admin membership. Their revision is separate from authorization revisions.
Logos are decoded with bounded dimensions, re-encoded as PNG to strip metadata,
and read only after an active membership check; responses use private/no-store.

Migration **0030** adds workspaces, memberships, preferences, invitation/grant and
handover records, and the stable workspace usage ledger. It backfills existing
owner-linked connections without replacing repository, PR or environment IDs.
`installations.workspace_id` is authoritative; `owner_user_id` remains provenance.
Parameterized, invoker-security SQL predicates enforce resource authorization in
each feature's own queries, including counts, logs, downloads and settings.

Manual intents capture workspace and membership/grant revisions. Membership
mutations lock workspace → connected installations in ID order → affected PRs.
Protected mutations use compatible installation → repository → PR locks and
repeat authorization after waiting. External GitHub calls run outside transactions.
Workers recheck local authority at admission and execution boundaries, and verify
manual actors' current source permission before protected runtime work. Route
acceptance independently rechecks the captured local authority; finished build
accounting and route recovery preserve a candidate already accepted for the team.

Build slots, fair sharing, daily minutes and preview limits use stable workspace
identity. Existing `*_PER_ACCOUNT*` configuration names are retained and now mean
workspace. New containers carry `io.previa.workspace-id`; old user-based account
labels are reconciled through the container's stable PR ID. Completed and revoked
running attempts record consumption against the captured workspace, so ownership
handover does not reset limits. Old and workspace ledgers use existing retention.

## Upgrade and verification

Follow [development setup](02-development-setup.md#shared-workspace-cutover).
Stop old server/worker processes before applying 0030; run only workspace-aware
binaries afterward. This access-model cutover is not compatible with leaving an
old owner-based server serving requests. New binaries require Goose 31. Existing
preview containers do not need recreation for label migration.

A solo upgrade can roll back before team data exists, preserving newly recorded
usage in the legacy ledger. Once invitations, extra memberships, handovers or
new workspaces exist, rollback refuses to discard them. Use a forward fix or a
rehearsed pre-upgrade backup restore with the matching previous binaries.

Database/HTTP tests cover roles and CSRF, invitation identity/expiry/regeneration,
concurrent handover, grants, removal, request fencing, shared quotas, legacy
container identity, publication/offboarding races, private token/cookie revocation,
and stream closure. The
workspace browser suite uses real PostgreSQL and session/CSRF handlers with
fixture GitHub responses; it exercises creation/switching, access confirmations,
light/dark/mobile layouts, direct navigation, literal search, invitation consent
without JavaScript, profile/logo persistence, stale edits and handover.
See [browser setup](02-development-setup.md#browser-regression-checks-optional).

The shared workspace core is implemented and verified. The original Feature 1
section also references native feedback/review decisions, review-assignment
offboarding, visible usage/account controls and durable deletion/disconnection.
Those dependencies are **not implemented**: the review UI belongs to Feature 3
and account lifecycle/usage to Feature 6. The settings UI focuses on implemented workspace controls; unbuilt review and
account-management panels are omitted. This is not a claim that every
interaction in the original cross-feature permission matrix is finished.
Notification integrations remain deferred as agreed. Hosted, live multi-user
GitHub acceptance has not been exercised by the fixture-backed verification.
