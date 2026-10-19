# F5/F6: installation recovery and workspace approval

Date: 19 October 2026. Branch: `mvp-complete`, after F4 commit `9ca5c64`.
F5/F6 changes remain uncommitted; no push, live GitHub write or deployment.

## Scope and decisions

F5 implements roadmap Prompt 14 with the existing webhook lifecycle/Revoker,
source tombstones, teardown sweep and runtime cleanup. Polling only removes
verified access; it cannot restore access or authorize a build. The original
same-owner reconnect requirement follows the implemented workspace authority:
a verified Owner/Admin can preserve history only within the same workspace.

F6 implements the owner's explicit choice: operator approval per new workspace.
Migration 0045 preserves existing workspaces as approved. New/default workspaces
are pending; no customer role bypasses the gate. Operators use the existing
configurable immutable numeric GitHub user-ID allowlist. Empty denies everyone.
Approval is audited, revision checked, idempotent and starts no build.

Defaults: startup/five-minute reconciliation, 25 installations/pass,
100 pages of 100 repository IDs, a configurable five-change budget and an
all-known-installations circuit. The all-known circuit also counts losing every
accessible repository of an installation and deliberately protects one-installation
instances. Verified explicit reconnect can resolve an old exact 404; automatic
polling never bypasses that circuit. `EARLY_ACCESS_ENABLED=true` defaults on in
both processes. Both new settings are documented in env examples and local env.

## Environment and isolation

Tests used disposable PostgreSQL 17.11 in `previa-f56-tests-postgres`, port 55439,
with per-test schemas. The scratch root reached Goose45 and River007. Developer
PostgreSQL, running Air/server/worker and production resources were not migrated,
stopped or erased. Real Docker tests created only their labelled disposable
resources. Go used `/tmp/previa-go-build` and `GOFLAGS=-buildvcs=false`; socket,
TLS, browser and Docker tests ran with the necessary sandbox approval.

## Automated evidence

| Check | Result | Evidence |
| --- | --- | --- |
| `make build` and `go build ./...` | PASS | `/tmp/previa-f56-final-build-lint.log` |
| `make lint`: vet and golangci-lint | PASS, 0 issues | Same log |
| `go test -p=2 -timeout=30m ./... -count=1` with PostgreSQL | PASS, 51 tested packages, 0 failures | `/tmp/previa-f56-all.log` |
| OAuth callback original-workspace guard | PASS0.008s | `/tmp/previa-f56-callback-guard.log` |
| Final connection/revision-order/registration tests | PASS; accounts58.301s, app0.025s | `/tmp/previa-f56-final-targeted.log` |
| Full installation suite after connection helper extraction | PASS4.300s | `/tmp/previa-f56-installations-final.log` |
| Token-mint transport tests | PASS403/404 signals, 500/429 do not signal | `/tmp/previa-f56-targeted.log`, full Go suite |
| Migrations0044/0045 down/up | PASS in scratch schemas | `TestReconciliationMigrationRoundTrip`, `TestAdmissionMigrationPreservesExistingWorkspaces` |
| Accounts/early-access browser | PASS7, JS on/off, CSP | `/tmp/previa-f56-final-targeted.log` |
| Existing broad browser | PASS55, 195.27s | `/tmp/previa-f56-regression-browser.log` |
| Existing workspace/setup browsers | PASS14/5, 66.04s/17.60s | Same log |
| Review browser rerun | PASS6, 32.16s | `/tmp/previa-f56-browser-rerun.log` |
| Test data / usage browsers | PASS3/4, 18.52s/11.63s | Same log |
| All four real end-to-end journeys | PASS620.029s | `/tmp/previa-f56-e2e.log` |
| Real runtime regressions | PASS252.084s | `/tmp/previa-f56-real-regression.log` |
| Real routing rerun | PASS212.071s | `/tmp/previa-f56-routing-rerun.log` |
| Real housekeeping | PASS17.375s | `/tmp/previa-f56-housekeeping.log` |
| Prometheus rules/tests/config/dashboard queries | PASS29 alerts and28 dashboard rules | `/tmp/previa-f56-observability.log` |

The final small refactors moved the connection checks to a helper and
captured installation revisions before the App identity read. The final callback guard also lets OAuth callbacks use their original
session-bound workspace rather than a workspace selected later in another tab.
A focused guard test covers that case; the connection transaction remains the
admission authority. Targeted integration checks, full installation tests and
build/vet/lint verified these final changes;
there was no redundant second full regression cycle.

### F5 failure and ordering checks

Fake GitHub runs over TLS through the Go GitHub SDK. Tests cover exact deletion,
suspension, complete-list repository removal, stale/repeated signed loss events,
unsuspend without a build, 502/outage, rate limits, partial-page failure, count
mismatch, App401, wrong App identity, repository-list401 and context timeout.
A late error after an earlier exact404 aborts the entire batch without mutation.
The all-known and small change-budget circuits preserve access. A signed event
racing the external read changes the revision and wins over reconciliation.

The shared GitHub transport tests prove token creation403/404 only signals
suspicion. A database integration test proves the signal records a deduplicated
River wake-up and changes neither status nor repository access. Unknown IDs do
not create jobs. Worker registration is tested with builds paused and no Docker
or preview domain. Verified same-workspace reconnect preserves repository identity;
another workspace cannot acquire the old history.

### F6 authority and compute checks

A fresh pending workspace cannot connect through `Repository.Link`, reserve a PR
attempt or reserve a baseline attempt. It consumes no allowance. A prior reserved
attempt cannot execute after its workspace becomes pending. Customer Admin102
cannot approve, operator101 can, a stale revision is refused and duplicate approval
produces one audit. Approval itself does not create a physical attempt. Existing
workspaces survive the migration approved; new ones still wait. Empty operator
configuration grants nobody access.

The browser creates a real workspace, sees Pending, is refused at a connection
POST even with a valid CSRF token, and can use support. A nonoperator Admin cannot
read the approval console. The configured operator approves through the native
CSRF-protected form; the selected workspace then shows Approved. This runs with
and without JavaScript. Native approval forms use existing session/CSRF middleware.

### Real Docker evidence

All four `make test-e2e` journeys ran in one package invocation against isolated
resources: backend268.32s, reference93.34s, ordinary69.81s, persistent data188.51s.

The new ordinary E2E phase deliberately sends no uninstall delivery. An exact
App installation404 reaches the shared lifecycle path. The worker removes the
real route, containers, image and network without installation access. The same
workspace relinks to a new installation ID with the same repository identity,
preserving Manual policy and creating no build intent or metered attempt. A late
uninstall delivery is harmless. Four physical attempts remain four (25.767956
metered seconds).

Existing reference journeys still seed/reset, retain the earlier preview on a
failed seed, reveal test access, withhold fork secrets and clean up. Existing
persistent-data journeys still retain data across builds, reset/fallback safely,
refresh the baseline and finish disconnect/journal acknowledgement with complete
physical/source/baseline/cache erasure (11 attempts, including2 baselines).

Runtime: PASS252.084s, including PostgreSQL/MySQL data copy, managed MySQL,
sleep/wake/output, hardened candidates, fork secret withholding, network denial,
unhealthy/crashing/cancelled starts, operator opt-outs, capacity and cleanup.
The combined run was subsequently terminated with exit143 during routing, so
routing and housekeeping were restarted in independent invocations rather
than treating an incomplete log as a pass. Housekeeping passed17.375s; routing passed212.071s, including private auth,
HTTPS/failing swaps, stale operation fences, close/publication races, controller/
router restart, unrelated-object preservation, revoked access and in-flight cleanup.
The interrupted routing instance was identified from its exact scratch schema
and ownership label `ccfbea4fc23a`: its three previews/images, three ACME test
containers, three networks, ACME volume and scratch schema were removed. The
shared egress proxy was detached only from those two stale test networks; its
other connections and all current-test/user resources were preserved.

## Screens and manual acceptance

Pending, Approved and operator approval screens are captured at1440/390 in both
themes under ignored `web/tests/browser/artifacts/accounts-early-access-*.png`.
All12 layout/theme captures were visually reviewed. Automated checks reject
horizontal overflow and CSP errors. Final screenshot run: PASS7/7,21.597s
(`/tmp/previa-f56-screenshots.log`). The first pending
1440-light capture showed a previous-page view-transition snapshot; the capture
now reloads the pending page before screenshots, also checking persistence.

User guide manual rows L1–L9 cover real owner/operator approval and GitHub delivery
recovery. Automated equivalents passed as described above; owner-run hands-on
acceptance, live GitHub/DNS/host rehearsal and policy review are not claimed.
No new library, visual theme or architecture deviation. The same-workspace
ownership interpretation is documented in DESIGN28/docs35 rather than silently
allowing history transfer between teams.

## Findings and limits

Initial fixture errors (invalid manual actor/UUID and deferred workspace ownership)
were corrected to use valid automatic intents, UUID attempt keys and the actual
workspace creation service. TLS token tests were corrected to use the fake TLS
server's transport for token signing requests. These were test-fixture defects.
The first lint pass flagged connection-method complexity; extracting the workspace
check resolved it without changing its transaction or authorization order.

The first review browser run timed out opening an existing Reset modal; the
following no-script reveal then had no saved test access. All6 passed on a fresh
rerun without product changes. Its initial failed result is retained in the log;
no claim is made that this intermittent browser timing issue was repaired here.

The all-known circuit needs operator review and signed delivery redelivery, so
five minutes is a polling interval, not a guaranteed maximum cleanup time. Errors
and paused/unavailable workers may delay detection or physical cleanup. GitHub
comments/checks remain unchanged when access is lost. The developer DB must be
migrated to45 before restarting these binaries. Set a real numeric operator user
ID before approving new workspaces; its placeholder deliberately grants no access.

## Completion boundary

All required F5/F6 automated checks passed. All94 browser scenarios passed across
the initial runs and the documented review rerun. Final build/vet/lint passed
after all Go changes; documentation links and `git diff --check` pass. All test
jobs finished. F0–F6 in-scope feature implementation is complete; the remaining
MVP checklist includes owner manual/policy acceptance, requested commit/push,
release rehearsal and live deployment/GitHub/DNS/host verification. These are not
represented as implemented features or completed owner actions.
