# Project handoff
Last updated: 2026-10-17

## Current state
- Branch `mvp-complete`; commits are local (not pushed). Commit only when the user
  asks; push to `main`, never `master`.
- Release plan (README-MVP-PRODUCT-EXPANSION.md §1.2): preview-first MVP. Committed:
  Features 1, 2, 3 (`2ddaaef`), 5 (`ea87e60`), MVP-TODO.md (`6b85018`), then F1
  (spec corrections, PR page reads the serving deployment) and F2 = Feature 5B
  persistent test data in 14 commits after `6b85018`. GitHub `main` is at `6b07c5b`.
- **MVP-TODO.md is the roadmap.** F0–F2 are complete (the owner delegated the 5B
  defaults: 64 MiB, 2m, 1h; Claude walked through rows R12–R18 and T1–T14; the
  owner's hands-on pass moved to Part 2). Next: F3/F4 (Feature 6 free plan), F5
  missed-uninstall detection, F6 early-access decision, release acceptance.
- README/DESIGN are the source of truth (DESIGN §21–§25). Feature docs: docs/28–32.

## Feature 5B: persistent test data (docs/32, DESIGN §25)
- Migration 0041 (`SchemaVersion` = 41): `repository_sampledata_settings`,
  `baseline_refreshes` (+ log counters), `baseline_logs`, `repository_baselines`,
  `preview_components.seed/seed_ms/data_source/data_detail`,
  `build_intents.start_source`, `worker_capabilities.baselines`, trigger
  `previa_test_data_changed` (nested IF: OLD.state exists only on refreshes).
- `internal/runtime/data.go`: per SQL service kept → baseline → empty
  (`order`, `startFor`); setup after every source, seed only on empty; fallback
  restarts DB containers (tmpfs empties). `baseline.go`: `RunBaseline` (no secrets,
  own cache scope, internal network, setup+seed, capture via exec streams).
- `internal/sampledata`: store (requests coalesce; one River job per repo on the
  build queue, `JobRetryTx` hastens), storage (gzip + streamed AES-GCM, sha256),
  runner (advisory lock, claim/spacing/admission, sequence fence, discards results
  whose settings/branch changed), jobs (dispatcher 1m, freshness 1h/24h, cleanup 1h),
  service + handler (tab, settings, off, refresh, delete, refresh logs), GitHub
  branch heads. Spacing only for merged/daily/unusable triggers.
- Wiring `internal/app/testdata.go` (activity, logs, starting data, baseline
  builder, worker capability); webhook merge trigger `DeliveryService.WithMerges`;
  workspace middleware: `/test-data/settings|off|delete` = manage.
- UI: `web/pages/sampledata` (tab, log page), `web/styles/testdata.css`; PR page
  *Test data* row; review page line + Reset source radios (`start_source`, a Reset
  without one starts empty); comment line; seed diagnosis; kits split setup/seed.
- ui.js: in-place forms re-bind after htmx swaps; a live refresh is not swapped in
  while someone types in a `[data-live-pause]` form inside its target.
- Deploy: worker image + compose `baselines` volume at `/var/lib/previa/baselines`
  (not backed up); four optional worker settings in `.env.example`.
- Flagged deviations: MySQL proven at runtime level, not a web journey; no Rebuild
  modal (note beside *Runs with*); Developers can open the tab read-only; spacing
  skips person-triggered refreshes; cleanup lives in sampledata jobs; defaults
  64MiB/2m/1h instead of the spec's proposed 128MiB/2m/10m. With Test data off a
  candidate records no starting source (`l.startPlan.Enabled`), so Feature 5's
  pages and comment are unchanged.

## Feature 5 and F1 (docs/31)
- Migration 0040: `setup_ms`, `review_test_access`. Review page reads the newest
  publication; F1 made the PR page, report snapshot and component retention do too.
- Reset = Build action `action=rebuild&reset=true&return=review` (+ `start_source`).

## UI direction (user requirements; also saved in memory)
- Compact, Jira-like pages: side-by-side cards, in-place edits with a toast, inline
  errors, each fact once. Modals open in place; navigate only after submit.
  Screenshot new UI at 1440 and 390, both themes.

## Environments and verification
- Dev DB `previa-postgres-1` is at Goose 41 (checked read-only; not changed by the
  agent). The user's worker (`go run ./cmd/worker`) predates F2: restart it.
- Test DB container `previa-member-ux-test-postgres` (127.0.0.1:55439,
  previa_ux_test, previa/previa-test) is TEST_DATABASE_URL; tests use isolated
  schemas. Remove only this container when the user is done.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium` (Playwright's own download is
  a different revision); suites test-browser, test-workspace-browser,
  test-setup-browser, test-review-browser, test-test-data-browser.
- F2 verification: build, lint 0 issues, `go test ./...` 48/48, `make test-e2e` 4/4
  (686 s), `make test-runtime` 11/11, browser test-data 3/3, review 6/6, setup 5/5,
  workspace 14/14, UI 55/55; migration 0041 up/down/up checked. See
  test-docs/2026-10-17-persistent-test-data-report.md.

## Access invariants (unchanged)
- Active workspace membership is authoritative; Owner/Admin span all repositories;
  Developer/Reviewer need explicit grants (`workspace_repository_access`: read,
  preview, operate = Developer+, manage = Owner/Admin).
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- Owner: restart the worker (it predates F2). Then MVP-TODO.md F3.
