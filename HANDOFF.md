# Project handoff
Last updated: 2026-10-21

## Latest task complete — aggressive build-only verification
- User requested a fresh aggressive test pass of build-related functionality only.
  Completed with no product defects or data races detected in executed checks.
- Branch mvp-complete, base75f5681 (F7 committed). Build-testing changes committed
  in one commit at the owner's request; not pushed. Only4 tests +4 docs changed.
- Do not repeat completed verification. No subagents allowed. Every command prefixes
  /home/ajaykathwatee/.local/bin/rtk. Do not edit technical-docs/inside-previa.md.

## Tests added (application code unchanged)
- internal/environments/burst_test.go: 16 simultaneous requests across2 workspaces;
  4 global/workspace limit combinations, both legacy caps and F3 entitlements.
  Exact runtime admissions/ledger counts; waiting builds never execute/reserve.
- internal/repoconfig/fuzz_test.go: arbitrary build YAML, varying operator ceilings,
  deterministic effective settings/hash and bounded CPU/memory/timeout/processes.
- internal/source/fuzz_test.go: in-memory archive-path/symlink containment fuzzing.
- internal/source/workspace_failure_test.go: real closed-root file/directory/link
  failure, safe retry classification, no partial writes and successful recovery.
  Initial authoring syntax typo corrected before successful runs; no app defect.

## Completed verification — do not rerun
- 19 build/support packages with -race/-shuffle/coverage PASS: 420 parent +239
  subtest results; opt-in skips separately accounted. Package-local coverage63.6%
  excludes external-process/cross-package/system/browser execution, not full coverage.
- Global/workspace and F3 bursts repeated5: 40 bursts /640 submissions PASS.
- Quota/cancellation/replay stress count10: 60 PASS. Ordering/close/reopen/lost-job/
  publication recovery count5: 40 PASS. Source-workspace failure count5: 15 cases PASS.
- Build-specific approval/revocation fences count3: 27 parent +30 subtests PASS.
  Build reporting/App client: 30 parent +18 subtests PASS; 2live tests skipped.
  Assembled pipeline/capacity/source checks count3: 15 PASS.
- Build YAML fuzz60s:21498 executions PASS. Archive path fuzz60s:12833 PASS.
- Build-only Chromium scenarios19, Setup5, persistent-data3: all27 PASS.
- Sequential real Docker stages (-race/-shuffle): builder6 +8 subcases PASS271s;
  runtime11 +5 subcases PASS299s; routing8 PASS225s; housekeeping2 PASS22s;
  all4 E2E journeys PASS753s command. Total31 parents +13 subcases.
- E2E confirms physical worker builds, caches, networking/secrets, setup/seed,
  failed replacement preservation, Automatic newest SHA, GitHub outage recovery,
  baseline/kept-data/reset/fallback/fork/merge, missed uninstall/disconnect cleanup,
  stable routing, journal ack and usage reconciliation. Race instruments Go test
  processes; separately compiled worker/app fixture processes use normal build flags.
- make build PASS; final go vet/golangci-lint PASS0 after all added tests.
  Docs links/whitespace valid. Final Docker running inventory only original DB/egress.
- Evidence: test-docs/2026-10-20-build-stress-report.md. Raw logs/coverage/scripts/
  final-summary.json in /tmp/previa-build-audit/. All background jobs finished.
- No live GitHub writes or production acceptance claimed. Unrelated account/profile/
  support/review-authoring/usage-settings UI suites intentionally excluded.

## State / next steps
- Testing task finished; no required code work or verification remains for it.
- docs02 documents focused race/fuzz commands and sequential real Docker suites;
  test-docs index links the new report. Committed at the owner's request.
- F0–F7 implementation already complete and committed in prior work. Owner still
  needs real Google Web OAuth client/env/callback, migration46 on intended DB,
  manual/policy/release/deployment acceptance. Operator numeric IDs still empty.

## Environment / boundaries
- Disposable DB container previa-f56-tests-postgres at127.0.0.1:55439,
  DBprevia_ux_test, Goose46/River007. Never migrate developer DB or stop user jobs.
- GOCACHE=/tmp/previa-go-build GOFLAGS=-buildvcs=false; lintcache
  /tmp/previa-golangci-lint. DB/socket/browser/Docker commands require_escalated.
- UI_BROWSER_EXECUTABLE=/snap/bin/chromium. Egress imageprevia-egress:dev exists.
  No unrelated Docker pruning or development-process changes performed.
