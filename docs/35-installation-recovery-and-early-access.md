# 35 · Installation recovery and early-access approval

F5 implements roadmap Prompt 14; F6 implements the owner's 19 October 2026
choice of operator approval for every new workspace. Existing workspaces remain
approved when migration 0045 is applied. No customer workspace is automatically
suspended by this migration.

## Missed GitHub lifecycle deliveries

With App credentials configured, every worker registers the independent
`installation_reconcile` River queue, including workers running with builds
paused or without Docker. It runs at startup and every five minutes, selecting
at most 25 locally active/suspended installations, oldest checked first, with
token-failure signals prioritized. It verifies `GET /app` against `GITHUB_APP_ID`
using App JWT authentication before checking exact installation IDs.

An exact installation 404 means deleted; `suspended_at` means suspended. An
active installation's accessible repositories are compared with its **complete**
installation-token repository listing (100/page, at most 100 pages/10,000 IDs).
Missing totals/lists, inconsistent counts, repeated IDs, invalid pagination,
partial pages, 401, rate limits, outages and timeouts abort the pass without
revocation. Suspended installations are never restored by polling.

All external reads finish before one short transaction. The existing webhook
lifecycle function applies confirmed changes, source-time/removal tombstones,
`RevokeTx`, `cleanup_pending` and the teardown sweep wake-up. Installation
revisions captured before IO are checked under locks; a racing webhook wins.
Late/repeated uninstall or removal deliveries do not restart teardown. No second
cleanup state machine is introduced. Cleanup needs no fresh GitHub access.

A pass stops if it would remove access from every known installation (including
all accessible repositories of an installation), or exceed
`INSTALLATION_RECONCILE_MAX_CHANGES` (default 5, range 1–100). This deliberately
includes a deployment with only one known installation. The operator must inspect
App identity/configuration and GitHub delivery history, then redeliver the actual
signed event; increasing the small change budget does not disable the all-known
circuit breaker. Do not fabricate access or manually activate a deleted ID.

Token-mint 403/404 errors in the shared GitHub client record suspicion and a
durable, deduplicated reconciliation wake-up; they never revoke access in the
original HTTP/build/discovery/report request. Other errors retain normal handling.
A verified explicit reconnect may confirm the old installation is deleted and
apply the same lifecycle path before linking. It only considers conflicts in the
caller's authorized workspace. This maps the old roadmap's "same owner" rule to
shared-workspace authority: history never moves between workspaces. An explicit
reconnect can resolve a confirmed deletion even when the automatic all-known
breaker is open; it never restores an old installation ID. Existing Manual policy
and preserved history remain; restoring access does not authorize a build.

`previa_installation_reconciliation_total` has bounded outcomes: `deleted`,
`suspended`, `repository_removed`, `unchanged`, `error`, `circuit_open`. Every
repair logs a warning through the normal Sentry handler; identity failures and
circuits log errors. The optional Prometheus profile alerts on repairs and blocked
passes. See the [runbook](24-incident-runbook.md#missed-github-lifecycle-deliveries).

API reference: [authenticated App / installation endpoints](https://docs.github.com/en/rest/apps/apps)
and [complete installation repository listings](https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation).

## Workspace approval

`EARLY_ACCESS_ENABLED=true` is the default on both server and worker. Migration
0045 marks existing workspaces Approved, then makes Pending the default for new
rows, including automatically created default workspaces. Keep the setting
consistent between processes; production Compose uses their shared env file.
Setting it false is an explicit operator override for an unrestricted instance;
it does not alter stored approvals. Re-enabling it again gates pending workspaces.

The sidebar's **Early access** page (`/early-access`) shows the selected workspace's
actual status and a copyable reference. Creation leads a pending workspace to this
page. Its request is already recorded; no extra application form is required.
Reviewers neither see it nor open it (they go to Previews): approval changes nothing
they can do. Members, settings, support, logout, disconnect/deletion and cleanup
remain usable. Connecting code and running PR or baseline builds require approval.
While pending, Repositories offers no **Install on GitHub** button; it says connecting
opens once early access is approved and links to the status page, and a pull
request's build actions give way to the same reason. A form posted anyway (an old
tab) returns to the status page; an in-page request gets the short reason as text. HTTP feedback
is backed by checks inside the connection transaction, worker admission, execution,
lease renewal and publication. Approval itself starts no build.

Configured `SUPPORT_OPERATOR_GITHUB_IDS` operators use `/operator/early-access` to
review pending workspaces (25/page) and approve with a native CSRF-protected form.
An Owner/Admin is not an operator. An empty allowlist denies all operator access;
set the operator's numeric GitHub **user** ID before inviting early-access users.
A production server refuses to start with early access on and an empty allowlist
(`check-config` says so); a development server starts and logs a warning.
The env comments and [development setup](02-development-setup.md#f5f6-installation-recovery-and-workspace-approval)
explain where to find it. No approval email is promised.

Approval locks the active workspace, rechecks its revision, writes the immutable
operator ID/time audit and changes Pending to Approved atomically. Repeated
approval is a no-op. Deleted/deleting workspaces cannot be approved. Granting
customer roles cannot bypass this gate. Workspaces approved before migration
continue their current behavior; free limits, explicit build policy, source
permission and repository grants still apply afterwards.

## Verification

The feature report under `test-docs` records fake-TLS GitHub failures, the real
worker pipeline with a deliberately dropped uninstall webhook, migration down/up,
approval and compute-boundary tests, native/browser CSRF and operator isolation,
and existing feature regressions. Run tests against a disposable PostgreSQL DB;
no developer database migration or live GitHub write is needed.
