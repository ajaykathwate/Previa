# Project handoff
Last updated: 2026-10-18

## Current state
- Branch `mvp-complete`; F3 is committed locally. Commit only when the user asks;
  push to `main`, never `master`. No deployment/push was performed.
- **MVP-TODO.md is the roadmap.** F0–F3 are implemented and verified. Next
  implementation: F4 (account exits, support,
  status and policies), then F5 missed-uninstall detection and F6 early access.
- README/DESIGN are authoritative; F3 design is DESIGN §26, docs/33. F2 is
  DESIGN §25, docs/32; F1/Feature 5 is docs/31.

## F3: workspace usage and finite free limits
- Owner delegated amounts and confirmed no hosted users: **300 build minutes per
  UTC calendar month, one concurrent build, two preview slots per workspace**.
- Migration **0042**, `SchemaVersion` 42 (River still 007): finite free policy and
  workspace entitlements, periods, physical attempts, execution segments, wait
  deadlines/reasons. Existing active workspaces start fresh; an insert trigger
  grants new workspaces. Historical elapsed-time tables stay legacy, never billed
  or imported. There is no extra initializer, env variable or paid state.
- `internal/usage`: entitlement lock + existing global admission lock, UUID per
  physical attempt, <=60s total reservations split at UTC day/month boundaries,
  renewals/settlement, independent cancellation timer and minute River recovery.
  An unstarted hold costs zero; a crash uses its final bounded reservation and
  stays provisional. Period consumption equals segment sums; replay is idempotent.
- Environments and baseline runner share the ledger. Source/build/startup/setup/
  seed/fallback execution counts, including failed/superseded attempts. Waits do
  not count. Start and publication recheck entitlement; limit waits snooze without
  spending River retries. Same-PR replacement reuses a slot; baselines take none.
- Serving previews, reads, feedback, account access and teardown bypass compute
  gates. `BUILDS_PER_ACCOUNT`/`BUILD_MINUTES_PER_ACCOUNT_DAY` add workspace ceilings;
  zero disables only that extra ceiling. Restart all workers together on changes.
- `/workspaces/<public UUID>/usage`: Owner/Admin totals; Developer granted-repo
  technical usage only; Reviewer denied. Real blocked work and next checks,
  measured/reset/cutover times, pending/provisional accounting, worker-reported
  expiry/idle/log retention (unknown before report). No cache or purchase control.
- Presentation: `web/pages/usage`, `web/styles/usage.css`, sidebar Usage. Shared
  signal theme, exact UTC timestamps, stacked repository values on phones.
- Feature report: test-docs/2026-10-18-usage-and-free-limits-report.md; user guide
  rows U1–U10. Owner hands-on acceptance and commit remain separate release steps.

## Prior Feature 5B / F1 context
- Migration 0041: repository Test data settings, baseline refresh/log/history,
  starting-data and seed facts, `start_source`, worker baseline capability.
- `internal/sampledata`: baseline admission, sequence/coalescing, encrypted gzip
  AES-GCM storage, default-branch refreshes/merge triggers/freshness/cleanup.
  Runtime source order per SQL service: kept PR data → baseline → empty + seed.
- Production baseline volume `/var/lib/previa/baselines` is derived, not backed up;
  defaults 64 MiB/dump, 2m copies, 1h automatic-refresh spacing. Test data stays off
  until enabled; off records no starting source or GitHub comment line.
- PR facts/report snapshot/component retention follow the serving publication.
  See test-docs/2026-10-17-persistent-test-data-report.md for F1/F2 evidence.

## Environments and verification
- Dev DB `previa-postgres-1` remains Goose 41; not migrated by the agent. The
  user's running worker predates F2/F3: stop it before `make migrate-up`, then
  restart the server and worker. Make migration/run targets load .env; direct Go
  and browser test commands require exported settings.
- Disposable test container `previa-member-ux-test-postgres`, 127.0.0.1:55439,
  database previa_ux_test. TEST_DATABASE_URL tests use isolated
  schemas; leave this existing container intact.
- Browser `UI_BROWSER_EXECUTABLE=/snap/bin/chromium`; Usage captures with
  `UI_BROWSER_SCREENSHOTS=1` are in web/tests/browser/artifacts (ignored).
- F3 verification passed: build; vet/lint 0 issues; all 49 Go test packages on
  PostgreSQL; final usage race detector (88.952 s); migration up/down/up; lifecycle
  and compute-independent access. Browser: UI 55/55, workspace 14/14, setup 5/5,
  review 6/6, Test data 3/3, Usage 4/4. Runtime 11/11, routing 8/8; E2E four/four
  (945.591 s), including two metered baselines and period/segment reconciliation.
  All eight final Owner/Developer screenshots reviewed at 1440/390, light/dark.
- Broad UI harness now allows 10m (Go timeout 12m) for 55 scenarios on loaded
  hosts; individual action deadlines unchanged. Final run passed in 262.527 s.
- Corrected historical intent migration fixture/dependency order and two existing
  flaky assertions (checksum mutation and one-byte ciphertext coincidence).

## Access and working invariants
- Active membership is authoritative; Owner/Admin span repositories; Developer/
  Reviewer need explicit grants. Presentation must not import backend features.
- Preserve compact signal UI, modal confirmations, CSRF, honest unbuilt labels.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.
- Keep this handoff under 100 lines; replace stale facts instead of appending logs.
