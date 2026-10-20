# 34 · Account exits, support and restore-safe deletion

F4 implements expansion specification §9.5, §9.6 and §9.8, including the optional
final Disconnected comment (§10.3). Billing and missed-uninstall polling remain
outside this feature.

## Where to find it

The sidebar links **Account & connections** (`/account`) and **Support & status**
(`/help`). Your profile links to account controls and offers **End other
sessions**. Workspace settings also offer deletion. Help links to the existing
FAQ, `/status`, and `/policies/privacy`, `/policies/terms`,
`/policies/acceptable-use`. Public status/policy pages need no sign-in; request
submission and personal request history require sign-in.

All mutations use native POST forms with CSRF. Destructive confirmation opens
an accessible modal over the current page, with typed DELETE or DISCONNECT.
Native dialog commands also work with JavaScript disabled in supported browsers.
The signal theme and shared shells are retained.

## Authorization and sessions

`users.session_epoch` is checked on each authenticated request and live response
authorization check. End other sessions increments it and rotates this browser's
token/CSRF while keeping it signed in. Existing streams retain their original
epoch and stop when it changes. They also check the original session's existence,
so logout and idle expiry end streams. Private-preview cookies and one-time access
tokens carry the epoch and a one-way issuing-session digest; forward
authorization checks these and current membership. Logout/idle expiry also revoke
private-preview grants. Restore discards old dashboard sessions and advances all
epochs.
Private previews can ask even the retained browser to reopen after revocation.

Leaving a workspace uses its existing membership/grant revocation and repairs
unauthorized manual PR requests. Membership-removal triggers cancel manual
baseline refreshes from that member. Other members' previews and automatic team
work remain owned by the workspace. Personal deletion applies those fences to
every former workspace and revokes all sessions immediately.

## Durable exit operations

Migration **0043** adds lifecycle operations, permanent GitHub disconnection
identities, the independent journal, final-comment attempts, support requests,
operator audit, incidents and dependency observations. Repository/installation
ownership foreign keys cascade only when the coordinator erases their parent.
Rollback refuses after lifecycle/revocation data exists: forward-fix instead of
discarding deletion authority. Stop old server/workers before migration.

`internal/accounts` coordinates feature-owned contracts. Acceptance locks and
rechecks active workspace membership, role, revision and confirmation. Owners
delete workspaces; Owners/Admins disconnect their workspace's connections.
Database ownership guards serialize creation/transfer against personal deletion.
The acceptance transaction:

1. Records a stable operation and journal identifiers.
2. Makes the repository/installation unavailable or workspace Deleting.
3. Supersedes intents, starts fenced teardown and accepts route tombstones.
4. Disables test data and cancels pending/running baseline refreshes.
5. For workspace deletion, revokes invitations, grants and memberships and
   restricts compute. New writes/admission and late GitHub activation cannot undo it.

The `accounts` River queue checks pending operations every 30 seconds and on
worker start. It verifies runtime inventory, routes, teardown/cleanup operations
and execution leases; removes retained baseline containers/images/files, source
workspaces and repository build caches; then erases feature data. A durable owner
sidecar is written before baseline bytes, so interrupted/unrecorded dumps can be
attributed and removed too. Parent installation/workspace operations wait for
their children. Cleanup does not use build allowance or require fresh GitHub reads.
A worker can restart cleanup during a GitHub outage using the configured App ID.

Completion requires both successful physical cleanup and independent journal
acknowledgement. Missing dependencies, failed cleanup or a missing journal
publisher leave visible pending progress, never a completed deletion. Operation
pages show safe step codes and accepted/completed times. The requester retains
progress access after workspace membership is revoked.

Local disconnection does **not** uninstall the GitHub App. Its installation/repo
numeric IDs remain terminal fences, including after product rows are erased.
Late inserts are ignored and updates cannot reactivate the connection. A fresh
explicit App installation with a new installation ID can connect after cleanup;
old secrets, settings and review history are not restored.

Personal deletion requires every owned workspace to be transferred or fully
deleted. It removes profile/avatar, memberships/grants/preferences and personal
support requests. Shared authorship keeps a user row named **Deleted member**;
immutable GitHub/public IDs remain revocation identifiers. Deleted identities
cannot sign in again through an ordinary callback. A random HttpOnly progress
receipt lasts seven days; only its hash is stored. Shared team source/reviews
and usage are not cascaded through the deleted user.

## Optional final GitHub comment

While App access permits, the worker updates an existing App-owned comment with
the living-comment marker to Disconnected. It does not create a new comment or
edit a human's comment. The normal report lease serializes writes. There are at
most three attempts, with 30-second delays and a ten-minute overall window;
permission/not-found rejection ends immediately. GitHub failure does not undo the
local fence or prevent physical teardown. It can delay final erasure only within
that finite window.

## Support, operators and status

Configuration is documented in both env examples. Defaults: 16 KiB UTF-8 message,
five requests per user per UTC day, resolved requests retained 90 days. Open
requests stay until resolution or account/workspace erasure. Submission serializes
the per-user allowance and validates optional public repository/PR references
against current access. Bodies are escaped and never copied into logs.
Each persisted request has a safe diagnostic UUID. Submission stores a request;
it does not promise email delivery, staffed chat or a response deadline.

`SUPPORT_EMAIL` configures contact links; `OPERATOR_NAME` configures policy text.
`SUPPORT_OPERATOR_GITHUB_IDS` contains comma-separated **numeric GitHub user IDs**,
not logins, App IDs or installation IDs. Find yours under Your profile → GitHub
account → GitHub ID, or the `id` at `https://api.github.com/users/YOUR_LOGIN`.
Empty means nobody can use `/operator/support`. Customer Owner/Admin roles confer
no operator privilege. Operator request reads/resolutions and incident changes
are audited with immutable operator ID, action, reference and time. This console
does not grant source, secret or private-preview access.

Execution workers probe Docker and the actual loaded Traefik fallback router
every 30 seconds. Paused builds report unavailable. Reports older than 90 seconds
are unknown; missing reports do not imply health. The public page states exactly
what is checked and does not promise end-to-end uptime. Operators publish/update/
resolve incidents through CSRF-protected forms with stale-revision protection.

Policy pages describe actual data, essential cookies, GitHub permissions,
retention, finite usage and deletion semantics. The operator must check their
identity/contact and review these operational terms before public launch; this
implementation makes no claim of jurisdiction-specific legal review.

## Independent encrypted deletion journal

The journal contains UUIDs, numeric provider identifiers, action and time; no
request/review body, credentials or deleted content. Its plaintext limit defaults
to 1 MiB (`DELETION_JOURNAL_MAX_BYTES`, maximum 16 MiB). Overflow fails closed.
Acknowledged completed events expire after 90 days by default
(`DELETION_JOURNAL_RETENTION_DAYS`); unacknowledged/unfinished events are retained.
This retention must exceed `BACKUP_RETENTION_DAYS` (default 30).

`deploy/production/deletion-journal.sh` uses the existing age/rclone backup
channel. A one-minute systemd timer exports via the private server CLI using the dynamic-IP maintenance `journal` service (no HTTP listener), encrypts
before upload, stores `BACKUP_REMOTE/deletion-journal/latest.json.age`, and only
then acknowledges exactly the exported identifiers. A channel lock prevents
older concurrent exports replacing newer ones. Production uses off-host R2;
an absolute directory is suitable for a disposable drill, not host-loss recovery.

`backup.sh` marks a snapshot boundary before pg_dump and requires a subsequent
journal upload. `restore.sh` locks the publisher, stops server/worker/router,
retrieves and decrypts the independent latest journal, restores/migrates the DB,
checks instance/version/size/time, then replays missing events before serving.
Events already present in the snapshot are not reapplied over later legitimate
grants. Ownership transfers replay before subsequent personal deletion. Missing
targets still retain provider revocation fences. Each event is checkpointed;
partial replay is safe to retry. Old generated preview routes are removed before
router startup; the worker reconstructs the accepted, fenced table with builds
paused. Missing, foreign, malformed or older-than-snapshot journals stop recovery.

Do not start an old binary or restore a pre-F4 snapshot that lacks this journal
identity. Retained backups are a recovery boundary, not permission to bring back
completed deletions. See [production B5/B6](25-production-deployment.md#step-b5--restore-the-database-on-an-existing-host)
and the [incident runbook](24-incident-runbook.md).

## Verification

`make build`, `make lint`, `go test ./...` with a disposable PostgreSQL URL,
`make test-accounts-browser`, the existing browser/runtime/routing regressions,
and the real Test data journey exercise this feature. That journey now finishes
with explicit repository disconnection, journal acknowledgement and verification
that runtime objects, baseline/source files and build caches are gone. Production
smoke testing exercises the actual encrypted backup/restore scripts. Commands
and current evidence are in [development setup](02-development-setup.md#f4-account-exits-support-and-deletion)
and the F4 report under `test-docs`.

## Google identity erasure and journalv2 (F7)

Google subjects are immutable optional account identifiers. Erasure removes their
verified email/profile but retains subject tombstones. Journalv2 includes the
Google subject for account deletion and uses public user IDs for membership
replay, protecting accounts created after a backup too. Current readers acceptv1;
old readers rejectv2. Restore must use the updated application/schema46. No tokens
or emails enter the journal. [36](36-google-sign-in-and-reviewer-invitations.md).
