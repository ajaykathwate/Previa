# Project handoff
Last updated: 2026-10-16

## Current state
- Branch `mvp-complete`; commits are local (not pushed). Commit only when the user
  asks; push to `main`, never `master`.
- Release plan (README-MVP-PRODUCT-EXPANSION.md §1.2, decided 16 Oct): preview-first
  MVP. Done: Feature 1 (workspaces), 2 (onboarding), 3 (review and feedback,
  `2ddaaef`), 5 (ready-to-test sample data, `ea87e60`). Nothing pushed: GitHub `main`
  is at `6b07c5b`. **MVP-TODO.md is the roadmap**: remaining features (5B, Feature 6
  free plan, missed-uninstall detection, early-access decision), release acceptance,
  and the deployment checklist with what the owner provides. Deferred: Feature 4,
  GitHub review-status projection (step E), paid mode (step J), third journey.
- README/DESIGN remain source of truth (DESIGN §21 workspaces, §22 onboarding, §23
  reviews, §24 test data). Feature docs: docs/28–31.

## Feature 5: ready-to-test sample data (docs/31-test-data.md)
- Migration 0040 (`SchemaVersion` = 40): `preview_components.setup_ms` (runtime
  times setup per candidate) and `review_test_access` (one per repository: account,
  sealed secret, revision, updated/last revealed). Trigger notifies on revision
  change only (reveals and key rotation do not refresh pages).
- Review page (`internal/reviews`): Preview card shows the serving deployment's setup
  (components joined on the **newest publication**, not `e.applied_*`, which a failed
  update moves) and **Reset test data** (`resetForm` conditions in service.go). Test
  access card: Owners/Admins edit (`testaccess.go`, `Sealer` interface = secrets
  keyring, name `previa:test-access`); members with preview access reveal via
  `POST …/review/test-access/reveal` (JSON or page, `no-store`), recorded, spends
  allowance. ui.js keeps a revealed value across live swaps for 2 min unless the
  revision changed.
- Reset = pull request Build action with `action=rebuild&reset=true&return=review`
  (`pullrequests.returnLocation`); `environments.IntentCommand.Reset` (Rebuild only)
  and `Broker.WithResets` → `internal/app/reviews.go` `resetActivity`
  (`preview.reset_requested`).
- `make secrets-reencrypt` also rotates test access (`testAccessRotation`).
- Starter kits: migrate-and-seed presets per stack; seed guidance kept short (a
  guidance card must fit a 640 px viewport, checked by the UI suite).
- Reference journeys: `internal/app/testdata/reference` (shop API, notes web app with
  login), `TestRealReferenceJourneys` (`make test-reference`, also in `make test-e2e`).
  Fake GitHub supports same-repository branch PRs (`pushBranch`).
- Flagged deviations: no separate *Setup declared* state while building; test access
  is repository-level (per-deployment binding deferred); two journeys, not three.
- Known, unchanged: the PR page's *Runs with*/docs still join `e.applied_*`, so they
  hide after a failed update (review page fixed only).

## Feature 3: review and feedback (docs/30-reviews.md)
- Review page `GET /repositories/{id}/pull-requests/{n}/review`; Operators via
  `components.PullRequestTabs`, Reviewers via **Review** on Previews or links.
- No-JS POST forms with redirect; errors re-render with drafts (422/409/429). Writes
  lock PR (key share) → membership (share) → counters. Publications written in
  `environments.activateTx`; activity in `internal/activity` (hourly prune).
- Workspace guard: Reviewers reach review paths, `/events` and `…/open` only.

## Feature 2: guided onboarding (docs/29-guided-onboarding.md)
- Repository **Setup** tab, River `repository_setup_check`, migration 0038; "What's
  next" leads to the review page.

## UI direction (user requirements; also saved in memory)
- Compact, Jira-like pages: side-by-side cards, in-place edits with a toast, inline
  errors, no tabs for related details, each fact shown once, one primary per area.
- `components.PageTitle` headings; PR pages `prHero` + `PullRequestTabs`. Modals open
  in place; navigate only after submit. Screenshot new UI at 1440 and 390, both themes.

## Environments and verification
- Dev DB `previa-postgres-1` is at Goose 39; it needs Goose 40 (`make migrate-up`,
  then restart server and worker) to run this code. Not touched by the agent. Two
  leftover `ordering_test_*` schemas remain there.
- Test DB container `previa-member-ux-test-postgres` (127.0.0.1:55439,
  previa_ux_test, previa/previa-test) is TEST_DATABASE_URL; tests use isolated
  schemas. Remove only this container when the user is done.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium`; suites `make test-browser`,
  `test-workspace-browser`, `test-setup-browser`, `test-review-browser`.
- Feature 5 verification: build, lint 0 issues, `go test ./...` 47/47, `make test-e2e`
  3/3 on Docker 29.8.1, review 6/6, workspace 14/14, setup 5/5, UI 55/55; migration
  up/down/up checked. See test-docs/2026-10-16-test-data-report.md.

## Access invariants (unchanged)
- Active workspace membership is authoritative; Owner/Admin span all repositories;
  Developer/Reviewer need explicit grants (`workspace_repository_access`: read,
  preview, operate = Developer+, manage = Owner/Admin).
- Membership mutations serialize workspace → installations → PRs and revoke
  private cookies/streams; invitations are digest-only, addressed, seven-day.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- Follow MVP-TODO.md top to bottom; tick boxes only with evidence. Next: F0 (owner
  tests Feature 5, rows R12–R18), F1 (remove Feature 4 references from spec §8B, fix
  the PR page *Runs with* after a failed update), then F2 Feature 5B.
