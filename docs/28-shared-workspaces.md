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
scope before accepting or declining. Accepting makes the joined workspace current and
lands where the role starts: Reviewers on **Previews** (the previews shared with
them), everyone else on the Overview's next steps
([guided onboarding](29-guided-onboarding.md)), with a toast naming the workspace.
Repeated acceptance is idempotent, and cannot
restore removed membership or replace a newer role. Invites issued by someone who
loses invitation authority are revoked.

## Navigation and workspace identity

One workspace represents one team. It can contain many members and repositories;
there are no nested teams or channels. A person can join several workspaces and
switch with the sidebar selector. Its aligned chevron, role capsule, full-name
tooltip and current-workspace indicator make the selected scope visible. The menu
also links directly to members, settings and the workspace picker.

**Members** handles people and access. Its Members and Invitations views keep
pending invitations separate from joined people. Search matches member names and GitHub usernames
literally, ignoring case, and works across the full paginated list. Members have
role capsules and repository summaries. Manage access opens one dialog containing
the role and grants. Admin roles explain their all-repository access; Developer
and Reviewer roles show explicit checkboxes. Owners can appoint Admins. Removing
or leaving requires a confirmation modal over the current page. Only managers see
invitations and invitation actions.

**Workspace settings** handles identity and ownership in a compact layout: a
**General** card (logo, name, description, copyable workspace ID) beside an
**Overview** card (owner, your role, member/repository counts, creation date), with
**Ownership** as one full-width row underneath. Owners/Admins edit the name (1–80
characters) and optional description (up to 240 characters) in place — Enter or ✓
saves (Ctrl/⌘+Enter for the description), Esc or ✕ restores, leaving a changed
field saves it — and a small toast confirms each save. Clicking the logo uploads a
replacement; the browser shrinks large images first, and the server accepts PNG/JPEG
up to 192 KiB and 512 × 512 pixels and re-encodes it. **Remove** restores the
initial. No public image URL or image service is involved. Settings is hidden and
inaccessible to Developers/Reviewers. A nominated Admin gets a handover link on
Members; the nomination and acceptance controls live under Settings → Ownership.

**Members** lists people in dense rows (photo, name and @username, role,
repository access, actions) with Members/Invitations/Deactivated switching and
search in one toolbar. Saves elsewhere on the page (access changes, removals)
confirm with the same toast; a refused change stays on Members with the reason
above the list. The workspace name is not repeated in the page heading — the
breadcrumb and switcher already show it.

- **Member profiles.** Clicking a person opens `/workspaces/{id}/members/{user}`:
  photo, @username, status, role, repository access, membership date, public GitHub
  details, and their recent activity in this workspace — only build requests in
  repositories the viewer can read, plus membership events; invitations they sent
  and removals they made are shown to managers only. Email is never shown there.
  Managers can manage access or remove the person from the profile too.
- **Deactivated.** Removing a member (or leaving) is a soft delete: the membership
  keeps its role, who removed them and the repositories they had, while all access
  ends at once. Deactivated people are listed on their own tab (managers only),
  with no access controls. **Re-invite** opens the invitation form preselected with
  their previous role and repositories; accepting reactivates the same membership.
- **Invitations inside Previa.** An invitation is addressed to a GitHub account,
  so a person who already uses Previa sees it under **Invitations for you** on the
  Workspaces page and as a badge on the workspace switcher, with the inviter, role,
  exact repositories and expiry; **Join workspace** or **Decline** answers it
  without the link. The one-time link still works for people who haven't signed in
  yet. Only the addressed GitHub account can answer either way.

**Creating a workspace.** "Create workspace" (switcher or Workspaces page) opens a
dialog over the current page; nothing navigates until it is created. The new
workspace opens on a setup checklist — invite teammates (inline invitation
form), connect repositories (GitHub App install), describe the workspace — whose
steps reflect real state.

**Deleting a workspace.** Owners can delete a workspace only when they are its only
active member and no GitHub App installation is still connected (uninstalling the
App lets Previa tear down that workspace's previews first). The confirmation
suggests inviting a teammate and handing over ownership instead, and requires the
owner to type `DELETE` in capital letters. Deletion is a soft delete: access ends
immediately, pending invitations are revoked and the logo is removed; usage records
stay.

The primary CTA on Members is Invite teammate; Settings links back to Members.
The command palette exposes both pages. **Previews** is the sole preview listing;
Reviewers get a read-only version there without source/log/build controls. Older
`/workspaces/{id}/previews` bookmarks select an authorized workspace and redirect
to `/previews`. No duplicate Previews tab is rendered inside settings.

Forms work without JavaScript: in-place fields are ordinary inputs whose ✓ submits
the form, image pickers show an Upload button, and the redirect shows the toast.
JavaScript adds in-place JSON saves (`Accept: application/json`), role
explanations, image shrinking and menu positioning. Invalid inputs show their error
next to the field and keep typed values; a stale submission gets a 409 with a
message so the user can review before trying again. Profile and logo edits
do not sign people out of previews or invalidate in-flight manual requests.
Light/dark palettes and narrow layouts use the existing signal tokens.

## Large repository lists and invitation recovery

A member row displays at most two truncated repository chips and a **+N more**
button. That button and **Manage access** open the same access form. Owners/Admins
show **All repositories** instead. The role is a set of styled, keyboard-accessible
radio cards with short capability descriptions. Admin access hides and disables
explicit grants; Developer/Reviewer grants are checked explicitly.

The repository picker has a bounded scroll area, search, a total selected count,
and **Select visible / Clear visible** controls. Bulk actions affect only the
current search results; selections outside the search remain intact. Full names
are available on hover and in the accessible labels. New repositories never
silently become Developer/Reviewer grants. The list and form remain usable on
small screens and without JavaScript (checkboxes and scroll area).

Invitation creation validates username syntax, resolves the real personal GitHub
account, checks existing active membership, and checks unexpired pending invitations
under the workspace mutation lock. Incorrect, missing, already-member, pending, and
provider-unavailable results have distinct messages at the username field. The
Members screen retains the typed value, selected role, and repository checks.
JavaScript reopens the same form as a native modal; without JavaScript the form is
visible inline. No invitation is saved on these failures. Pending invitations are
managed from Invitations, where links can be regenerated or revoked.

## Personal identity and UUID addresses

The member’s own first/last name takes precedence over the GitHub username; the
username remains visible for identification. Names and the profile photo can be
changed in place from **Your profile**, with optional fields, stale-edit detection,
and immutable GitHub identity. Uploaded photos are visible only to the member and
their current teammates. GitHub metadata sits in its own section beside the Previa
profile. Verified email is private to that member. See
[GitHub sign-in](04-github-sign-in-and-app.md).

Migration **0032** adds personal name and cached GitHub metadata. Migration **0033**
backfills persisted random `public_id UUID` columns with unique indexes on users,
workspaces, installations, repositories, pull requests, invitations, outbound
destinations, and environments. Bigint primary/foreign keys remain internal so
existing worker jobs, routes, usage, and containers keep their identity.
Migration **0035** adds uploaded profile photos (`user_avatars`, plus
`users.avatar_version` and `users.custom_avatar`) and fills never-edited blank
names from the cached GitHub name.

`internal/platform/publicid` resolves inbound UUIDs before feature authorization.
A UUID is an address, never evidence of permission. Pull-request UUID resolution
is bound to its repository. Handlers batch only references included in their
authorized display models; `web/components/resourceid` formats URLs from that
supplied data without importing backend features or querying during rendering.
Links, form values, redirects, HTMX headers, live streams, and GitHub report log
links use UUIDs. Older numeric HTML GET bookmarks authorize first and redirect to
the canonical UUID address. Numeric mutation, stream, logo and download addresses
are rejected by the production catalog. GitHub PR numbers remain readable labels
and GitHub protocol IDs remain unchanged. River retains its own administrative job
IDs. No global HTML response rewriting is involved.

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
old owner-based server serving requests. New binaries require Goose 34. Existing
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
Native feedback is implemented ([30](30-reviews.md)): granted Reviewers use a pull
request's review page, its forms, the repository's change stream and Open preview,
and nothing else on repository paths. Review decisions and assignments (Feature 4)
and account lifecycle/usage (Feature 6) are **not implemented**. The settings UI focuses on implemented workspace controls; unbuilt review and
account-management panels are omitted. This is not a claim that every
interaction in the original cross-feature permission matrix is finished.
Notification integrations remain deferred as agreed. Hosted, live multi-user
GitHub acceptance has not been exercised by the fixture-backed verification.
