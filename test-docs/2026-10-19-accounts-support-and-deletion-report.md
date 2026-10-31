# F4 account exits, support/status and restore-safe deletion

Date: 19 October 2026. Branch: `mvp-complete`; included in the F4 commit.
Scope: roadmap F4 / expansion §9.5, §9.6, §9.8 and optional §10.3 final
Disconnected comments. Feature design and recovery boundaries are documented in
[docs/34](../docs/34-accounts-support-and-deletion.md) and DESIGN §27.

## Environment and commands

Go 1.26.9, PostgreSQL 17 on a disposable local database, Docker Desktop,
Chromium `/snap/bin/chromium`, locked Playwright 1.58.2. Database integration
tests create isolated schemas and apply all 43 migrations plus River migrations.
The development database was not migrated or erased. Checks use temporary
Go/lint caches and `GOFLAGS=-buildvcs=false` to avoid slow WSL Git metadata
scanning. No live GitHub writes, deployment or push occurs in this cycle.

```sh
# TEST_DATABASE_URL must already point at an explicitly chosen disposable DB.
make build
make lint
go test -p=2 -timeout=30m ./... -count=1
UI_BROWSER_EXECUTABLE=/snap/bin/chromium UI_BROWSER_SCREENSHOTS=1 \
  make test-browser test-workspace-browser test-setup-browser test-review-browser test-accounts-browser
TEST_E2E=1 go test ./internal/app -run '^TestRealTestDataJourneys$' -count=1 -v -timeout=60m
make release-images PREVIA_VERSION=f4-local
make prod-smoke PREVIA_VERSION=f4-local
```

## Recorded evidence

- Account lifecycle/journal/support integration tests pass: immediate access
  fences, revision/role/confirmation checks, parent-child cleanup, independent
  acknowledgement before erasure, no resurrection through old webhooks, personal
  ownership prerequisite, receipt expiry, pseudonymous attribution and shared
  data preservation. Migration 43 rolls down/up in a fresh scratch schema.
- Restore integration tests use a second older schema. Missing events replay
  before traffic; ownership moves before former-owner deletion; journal events
  already present in the snapshot do not undo a later legitimate regrant.
  Malformed, foreign, stale and oversized journals are refused. Completed
  acknowledged events obey retention; unfinished/unacknowledged ones remain.
- Support tests cover scoped references, escaped bodies, immutable operator
  allowlist, audited reads/resolutions/incidents, stale revisions, size/daily
  bounds, retention, account/workspace erasure, and the per-person allowance
  surviving workspace message deletion. Status uses fresh actual observations
  and changes to Unknown when stale; no synthetic uptime.
- Assembled HTTP/session tests pass, including real SCS logout/session validation,
  epoch rotation and private-grant revocation. Existing private-preview tests
  cover membership removal, epoch change and issuing-session logout independently.
- Final-comment tests pass for updating only an existing marked App-owned
  comment, success/idempotence, permission loss, finite outage retries and
  refusing human comments. Source/cache erasure tests preserve unrelated objects
  and wait for busy caches. Baseline oversized/aborted writers remove sidecars.
- Real TestData worker journey passes (451.872s): 11 metered attempts, including
  two baselines, reconciled to 243.770071 executed seconds. It ends with explicit
  repository disconnect, journal acknowledgement and verified removal of runtime
  objects, baseline/source files and repository build caches.
- Initial accounts browser pass: 5/5, JavaScript on/off, CSP, typed in-place
  confirmation, durable progress, support/status/policies and end-other-session.
  Final wider regression and expanded screenshot review are recorded below.
- Pinned ShellCheck 0.10.0 passes production backup, restore, journal, setup
  and smoke scripts. Real age 1.2.1 is used for the encrypted restore drill.

## Corrections found during verification

1. Interrupted/oversized baseline dumps could leave an attribution sidecar.
   Oversized and aborted writers now remove it; explicit erasure can discover
   orphaned sidecars without deleting another repository's data.
2. A maintenance command launched as another fixed-IP dashboard container
   collided with the running server. Production journal commands now use a
   dedicated dynamic-IP maintenance service with no HTTP listener.
3. Snapshot restore could resurrect previously logged-out sessions. Replay
   clears restored SCS sessions and advances every authorization epoch. Private
   grants also bind to a digest of the issuing session, so ordinary logout and
   idle expiry revoke them without exposing a dashboard bearer token.
4. Shared profile field CSS squeezed resource names and indented help copy on
   phones. Customer resource rows now size naturally and notes align with text.
5. Existing workspace assertions assumed deletion was unavailable with members.
   They now verify F4's owner-only resumable deletion. The workspace browser
   fixture now mounts the real lifecycle handler and verifies pending progress,
   rather than the former immediate-deletion toast. Two initial database tests
   timed out under concurrent WSL build load; both passed on regression rerun.

## Final gate

All automated gates below completed successfully. The original full Go command
had two failures: the environment package's aggregate ten-minute timeout during
fixture setup, and the workspace test import graph changing while that command
was already running. Both packages passed fresh isolated reruns; all other
packages passed the full run. No failing assertion remains.

| Gate | Final result |
| --- | --- |
| Build, formatting and whitespace | Pass |
| Vet and golangci-lint | Pass; 0 issues, including final fixture changes |
| All existing Go tests | All packages pass; environments rerun with `-timeout=30m` (444.194s), workspaces rerun (112.528s) |
| Lifecycle migration down/up | Pass in disposable schema |
| Broad UI browser | Pass, 55 scenarios (320.302s); additional 25-case group also passed |
| Workspace browser | Pass, 14 scenarios (155.663s) |
| Setup / Review / Accounts browsers | Pass, 5 / 6 / 5 scenarios (39.936s / 70.770s / 49.623s) |
| Real backend / ordinary pipeline | Pass (393.14s / 137.74s) |
| Real reference journeys | Pass (189.222s), run without a concurrent smoke stack |
| Real TestData + explicit disconnect | Pass (451.872s), verified physical/file/cache erasure |
| Real runtime / routing / housekeeping | Pass (343.909s / 261.820s / 28.756s) |
| Release images | All three built successfully |
| Encrypted production backup/restore | Pass, 45 checks; restore 52s |
| Production ShellCheck 0.10.0 | Pass |

Screenshots cover account settings, support, operator console, public status,
policy pages, pending/completed progress, disconnect and session dialogs at
1440/390 in both themes. Overflow assertions pass. Representative desktop/phone,
light/dark views were reviewed, including the corrected resource rows and notes.
Screenshots remain ignored inspection artifacts. Logs are `/tmp/previa-f4-*.log`.
Support integration coverage lives in `internal/accounts/support_test.go`;
`internal/support` has no separate test file.

## Owner acceptance and operational boundaries

- Manual checklist: user guide rows A1–A15 under Account exits/support.
- Support email is configured only in the ignored local env. Operator IDs remain
  empty (denies everyone); enter comma-separated numeric GitHub **user** IDs,
  found on Your profile or the GitHub user API's `id` field. Env comments explain
  this. Defaults: 16 KiB messages, five per UTC day, resolved retention 90 days.
- Before using the new binary, stop old server/workers, migrate to 43, restart.
  Production additionally needs the minute journal timer and encrypted off-host
  backup destination; cleanup deliberately remains pending without upload ACK.
- Backups default to 30 days, journal 90 days/1 MiB; unfinished entries never
  expire. Pre-F4 snapshots lacking journal identity require a separate manual
  recovery decision and are refused by ordinary restore.
- Operational policy text is implemented, but owner identity/contact and legal
  review remain a pre-launch owner gate. Live host/R2 disaster recovery is not
  claimed by a local smoke rehearsal. No theme/library/architecture deviation.
