# Project handoff
Last updated: 2026-10-18

## Current state
- Branch `mvp-complete`; F3 is committed locally (`f0fc89b`). The final deployment
  setup below is **uncommitted** in the working tree. Commit only when the user
  asks; push to `main`, never `master`. No deployment/push was performed.
- **MVP-TODO.md is the roadmap.** F0–F3 are implemented and verified. Next
  implementation: F4 (account exits, support,
  status and policies), then F5 missed-uninstall detection and F6 early access.
- README/DESIGN are authoritative; F3 design is DESIGN §26, docs/33. F2 is
  DESIGN §25, docs/32; F1/Feature 5 is docs/31.

## Final deployment setup (agreed with the owner 2026-10-18; uncommitted)
- One OVHcloud VPS-2 (EU, €7.21) + Docker Compose; images **built on the server**
  from a release tag (no GHCR; `release.yml` is manual-only, refuses non-tags);
  `previa.live` = landing/dashboard, previews on a separate domain still to buy
  (placeholder `preview.example.net`); Cloudflare DNS + R2; Sentry free for
  errors/logs, uptime (previa.live, 1 min) and backup cron monitor, email via one
  alert rule; SSH only over Tailscale (port 22 closed); lazydocker. About €8/month
  + VAT. No Prometheus/Grafana/Alertmanager in prod (profile kept, optional), no
  Terraform, no staging VPS (rehearse restore on the prod VPS before launch), gVisor
  off for early access. Docs: docs/25 (overview, two Mermaid diagrams, cost,
  capacity, S1–S4, logs/SSH/monthly checks, pre-launch rehearsal), MVP-TODO Part 3.
- Code: `internal/platform/errortracking` (sentry-go v0.49.0 + slog): `logging.New`
  fans redacted records to it; error → issue, warn+ → Sentry Logs; `SENTRY_DSN`
  (env `APP_ENV`); `check-config` validates it; `previa-server sentry-test`.
  `backup.sh` derives the cron check-in URL from `SENTRY_DSN` (monitor
  `previa-backup`, 1-day interval); `BACKUP_FAILURE_WEBHOOK` removed. `setup.sh`
  installs Tailscale + lazydocker 0.25.2 (SHA-256 pinned), keeps SSH on tailscale0
  once moved. CI `paths-ignore` for docs-only changes.
- Gaps: wildcard cert unmonitored (docs/25 "Monthly checks"); GitHub CI runs of
  Oct 11–15 failed. `inside-previa.md` untouched.

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
- Migration 0041 / `internal/sampledata`: Test data settings, encrypted baselines,
  refresh/freshness/cleanup; source order kept PR data → baseline → empty + seed.
  Baseline volume is derived, not backed up. Evidence:
  test-docs/2026-10-17-persistent-test-data-report.md.

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
- Deployment-setup verification: build; vet/lint 0 issues; `go test ./...` 50/50
  packages on the test DB; errortracking/logging race ×5; actionlint (ci, release);
  shellcheck (setup, backup, restore, smoke); backup check-ins with stub tools;
  lazydocker install/checksum/rerun; `check-config` with bad/good/empty DSN;
  docs/25 Mermaid diagrams render (mermaid-cli 11.4.2); `make prod-smoke` 40/40
  incl. backup/restore. Not run: browser/E2E suites (no UI or pipeline change),
  anything against real Sentry, Tailscale or OVHcloud.

## Access and working invariants
- Active membership is authoritative; Owner/Admin span repositories; Developer/
  Reviewer need explicit grants. Presentation must not import backend features.
- Preserve compact signal UI, modal confirmations, CSRF, honest unbuilt labels.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.
- Keep this handoff under 100 lines; replace stale facts instead of appending logs.
