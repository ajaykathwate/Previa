# Shared workspace verification · 2026-10-14

This cycle verifies approved Feature 1 from
[the MVP specification](../README-MVP-PRODUCT-EXPANSION.md). The implementation
and user flows are described in [shared workspaces](../docs/28-shared-workspaces.md).
Later review, billing/deletion and notification features remain outside this cycle.

## Results

| Check | Result |
| --- | --- |
| Generated templates, SQL queries and theme assets | Passed |
| `go build ./...` | Passed |
| `go vet ./...` | Passed |
| Pinned `golangci-lint run` | Passed, 0 issues |
| Full `go test -race -count=1 ./...` with isolated PostgreSQL fixtures | 1,476 test/subtest passes, 40 opt-in skips, no failures or races |
| Final affected-package race checks after function refactors | Workspaces, environments, installations and runtime passed |
| Existing Chromium browser regressions | 55/55 passed |
| Workspace Chromium browser journeys | 4/4 passed |
| Real Docker runtime regressions | Passed, 13 test/subtest results |
| Real backend and basic worker pipelines | Both passed; 15 total runtime/pipeline test/subtest results |
| Documentation links and `git diff --check` | Passed |

The full-suite count includes subtests. Opt-in browser and Docker checks are
run separately; a default-suite skip is not evidence that those checks failed.
The final affected-package run verifies the small function refactors needed to
keep the repository's existing complexity limits.

## Access and user-flow coverage

- Owner/Admin/Developer/Reviewer permissions, explicit repository grants,
  forged workspace/resource IDs, source permission failures and CSRF checks.
- Workspace creation, selection, separate team navigation, the one-use
  connection callback's captured workspace and refusal of cross-team claims.
- Invitations addressed by immutable GitHub ID, exact scope consent, expiry,
  regenerate/revoke, decline and idempotent acceptance. Removed inviters and
  stale forms cannot grant access; repeated acceptance cannot restore removal.
- Owner handover and concurrent acceptance preserve one active Owner and stable
  connection, preview and quota identities. Owner departure requires handover.
- Removing a member stops subsequent repository/log/output stream frames and
  rejects existing private-preview cookies and unredeemed tokens. A removed
  member cannot regain access through the GitHub-readable outsider path.
- Unaccepted manual candidates are cancelled even after their build succeeds.
  Their pending references are released for cleanup, the previous serving
  artifact stays protected, and usage is neither lost nor counted twice.
- Accepted-route recovery and final running-build accounting survive actor
  departure. A direct membership revocation also fails the route acceptance
  fence without relying solely on a generation increment.
- Two developers share one workspace build limit. Historical installer labels
  resolve through PR identity after ownership transfer.
- Private previews and Reviewer links keep working while an older preview serves
  during a publishing/failed replacement; inaccessible repositories expose no link.
- Team browser checks use real PostgreSQL, sessions, CSRF and feature handlers.
  They cover themes/mobile, access confirmations, creation/switching, invitation
  consent without JavaScript and nominated-Admin ownership acceptance.

## Corrections found during verification

The `no-referrer` policy on team pages caused Chromium's normal form requests to
send a null Origin, which CSRF correctly rejected. Team/invitation pages now use
`same-origin`: forms retain their origin and external requests receive no invite URL.

Offboarding originally fenced pending/queued/running builds but missed a finished
candidate awaiting the asynchronous route fence. Cancellation now includes that
state, detaches the candidate and preserves already accepted workspace routes.

Filtering private authentication by the latest environment's `running` state
blocked the previous preview during rebuild/failure. Authentication now checks
current repository/workspace availability and grants; accepted routing determines
which preview can serve.

## Environment and practical limits

Go 1.26.9, PostgreSQL 17 and local Chromium/Playwright were used. Database fixtures
create and remove isolated schemas. Real runtime/pipeline checks use disposable
Docker/BuildKit/Traefik resources; external GitHub responses are fixtures, including
permission reads and comment/check-run reporting. No live GitHub writes occurred.

This cycle does not certify production hosting, live multi-user GitHub acceptance,
or every optional builder/routing/housekeeping/release suite. Existing baseline
results remain in the [previous report](2026-10-13-regression-report.md).

Development/production databases were not migrated or deployed by this task.
Activation needs the coordinated schema-30 cutover and server/worker restart in
[development setup](../docs/02-development-setup.md#shared-workspace-cutover).
