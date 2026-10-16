# Project handoff
Last updated: 2026-10-16

## Current state
- Branch `mvp-complete`; commits are local (not pushed). Commit only when the user
  asks; push to `main`, never `master`.
- Feature 1 (shared workspaces) and Feature 2 (guided onboarding) of
  README-MVP-PRODUCT-EXPANSION.md are implemented. Feature 2 was done ahead of the
  §11.2 order at the user's request. Feature 5B (persistent test data) is spec only
  (§8B, delivery step G). The user follows the documented feature order next.
- README/DESIGN remain source of truth (DESIGN §21 workspaces, §22 onboarding).

## Feature 2: guided onboarding (docs/29-guided-onboarding.md)
- Repository **Setup** tab (`/repositories/{id}/setup`, Owner/Admin/Developer):
  connect → check configuration → review what the app needs → publish → open from
  Previa, for one open PR (picker = `details[data-menu]` of links). Steps derived
  in `internal/onboarding/steps.go`; page in `web/pages/onboarding`.
- Migration 0038 (`SchemaVersion` = 38): `repository_setup_checks` (one per PR,
  revisioned, last result kept on failed attempts), `preview_opens`,
  `setup_check_budgets` (60/workspace/hour); triggers wake live pages.
- Checks: POST `…/setup/check?pr=` (operate + source permission via middleware,
  20 s cooldown, once per commit) → River `repository_setup_check` on queue
  `setup_checks`, registered on any worker with the App (`app/onboarding.go`).
  GitHub reads by repo ID: config, Dockerfile in context, root `.gitmodules`
  (`source.SubmodulesInContext` shared with builds). Platform fingerprint = offered
  services + build deadline + check rules (same offer as Starter kits).
- Build from Setup posts to the PR Build action with `return=setup`
  (`pullrequests.finishMutation`/`refusedFromSetup`, notice codes `build-*`).
- Open preview for operators → `GET …/pull-requests/{pr}/open` (records only
  same-origin/none `Sec-Fetch-Site`, not prefetches; 303 + no-referrer). PR page,
  PR list, Builds/Previews and Overview use `prpages.PreviewHref`; Reviewers keep
  direct links. `/open` is excluded from prefetch and numeric public IDs.
- Overview checklist is role-aware (`dashboardpages.Journey`, 4 steps, action on
  the next step only). Accepted invitations: Reviewer → `/previews`, others →
  `/dashboard`, joined workspace remembered, "Welcome to …" toast.
- ui.js: `initBusyForms` (`data-busy-form`), menus looked up per event, live
  refresh deferred/not swapped while a `details[data-menu]` is open, `notice`/
  `until` stripped like `saved`. Repo tabs scroll on phones (no wrap).
- Not built (labelled in UI): Request review / review instructions (Feature 3),
  review queue (Feature 4). PR page still shows operator build hints in production
  (setup page uses hosted wording) — possible follow-up.

## UI direction (user requirements; also saved in memory)
- Compact, Jira-like pages: side-by-side cards, in-place edits with a toast, inline
  errors, no tabs for related details, each fact shown once, one primary per area.
- Page headings: `components.PageTitle`; repository pages use `RepositoryHero` +
  `RepositoryTabs`. Modals open in place; navigation only after submit; menus and
  dialogs close before navigation. Screenshot new pages at 1440 and 390, both themes.

## Environments and verification
- Dev DB `previa-postgres-1` is at schema 37: run `make migrate-up` to reach 38,
  then restart the server (Air) and the worker (an old `go run ./cmd/worker` was
  running). Two leftover `ordering_test_*` schemas remain there (not touched).
- Test DB container `previa-member-ux-test-postgres` (127.0.0.1:55439,
  previa_ux_test, previa/previa-test) used as TEST_DATABASE_URL; tests use isolated
  schemas. Remove only this container when the user is done.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium`; suites `make test-browser`,
  `make test-workspace-browser`, `make test-setup-browser` (new, DB-backed).
- Feature 2 verification: see test-docs/2026-10-16-guided-onboarding-report.md.

## Access invariants (unchanged)
- Active workspace membership is authoritative; Owner/Admin span all repositories;
  Developer/Reviewer need explicit grants (`workspace_repository_access`: read,
  preview, operate = Developer+, manage = Owner/Admin).
- Membership mutations serialize workspace → installations → PRs and revoke
  private cookies/streams; invitations are digest-only, addressed, seven-day.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- User manual test of the Setup tab (steps given in chat), then the next feature in
  the documented order (Feature 3 review page, with publication records first).
