# Project handoff
Last updated: 2026-10-16

## Current state
- Branch `mvp-complete`; commits are local (not pushed). Commit only when the user
  asks; push to `main`, never `master`.
- README-MVP-PRODUCT-EXPANSION.md: Feature 1 (shared workspaces), Feature 2 (guided
  onboarding) and Feature 3 (native review and feedback, with step B: publication
  records and activity) are implemented. Feature 3 is not committed yet. Next in the
  documented order: Feature 4 (review rounds, decisions, review queue, timeline).
- README/DESIGN remain source of truth (DESIGN §21 workspaces, §22 onboarding, §23
  reviews). Feature docs: docs/28, docs/29, docs/30.

## Feature 3: review and feedback (docs/30-reviews.md)
- Review page `GET /repositories/{id}/pull-requests/{n}/review` (`internal/reviews`,
  `web/pages/reviews`, `web/styles/reviews.css`). Operators reach it via
  `components.PullRequestTabs` (Preview | Review, open-findings count) on the PR
  page; Reviewers via **Review** on Previews or a PR link (guard redirects).
- Forms (all plain POST + redirect, no-JS safe): `…/review/instructions`,
  `/findings`, `/findings/{n}/replies`, `/findings/{n}/messages/{p}/edit|remove`,
  `/findings/{n}/resolve|reopen|remove`. Errors re-render with the draft
  (422/409/429). Writes lock PR (key share) → membership (share) → counters.
- Migration 0039 (`SchemaVersion` = 39): `preview_publications` (written in
  `environments.activateTx`, numbered per PR, backfilled *Present at migration*),
  `preview_components.config_hash` (threaded from `runtime.buildImage`),
  `activity_events`/`activity_pruned` (`internal/activity`, hourly `activity_prune`
  on every worker), review tables, counters, `review_write_budgets` (300/member/h).
- Workspace guard: Reviewers may use review paths, `/repositories/{id}/events` and
  `…/open`; review POSTs need only "preview" ability (no GitHub source check).
- Open preview (`internal/onboarding`) now takes a validated `?path=` (instructions'
  start/docs pages). GitHub comment adds a link-only *Review in Previa* line.
- ui.js `initReviews`: in-place edit toggles, one removal modal
  (`data-remove-*`), drafts kept across live swaps (`data-draft-form/key`), refresh
  deferred while typing, Ctrl/⌘+Enter, auto-submit filters. Finding headers are
  stretched links to `?finding=N` (user request). htmx `attributesToSettle` no
  longer copies `style` (CSP).
- Flagged deviations: one page instead of Review/Feedback/Activity tabs; link-only
  GitHub projection (step E needs a sharing policy); activity has no view yet
  (Feature 4); 200 messages per finding and the hourly allowance are extra bounds.
- Labelled as not available: requesting specific reviewers, review decisions.

## Feature 2: guided onboarding (docs/29-guided-onboarding.md)
- Repository **Setup** tab for Owner/Admin/Developer, five derived steps, checks via
  River `repository_setup_check` (queue `setup_checks`), migration 0038. After an
  opened preview, "What's next" leads to the review page (Write testing instructions).

## UI direction (user requirements; also saved in memory)
- Compact, Jira-like pages: side-by-side cards, in-place edits with a toast, inline
  errors, no tabs for related details, each fact shown once, one primary per area.
- Page headings: `components.PageTitle`; repository pages use `RepositoryHero` +
  `RepositoryTabs`; PR pages `prHero` + `PullRequestTabs`. Modals open in place;
  navigation only after submit; menus and dialogs close before navigation.
  Screenshot new pages at 1440 and 390, both themes.

## Environments and verification
- Dev DB `previa-postgres-1` needs Goose 39 (`make migrate-up`, then restart server
  and worker); the user was already testing review pages on it. Two leftover
  `ordering_test_*` schemas remain there (not touched).
- Test DB container `previa-member-ux-test-postgres` (127.0.0.1:55439,
  previa_ux_test, previa/previa-test) is TEST_DATABASE_URL; tests use isolated
  schemas. Remove only this container when the user is done.
- Browser: `UI_BROWSER_EXECUTABLE=/snap/bin/chromium`; suites `make test-browser`,
  `make test-workspace-browser`, `make test-setup-browser`, `make test-review-browser`.
- Feature 3 verification: build, lint (0 issues), `go test ./...` 47/47 packages,
  review 5/5, setup 5/5, workspace 14/14, UI 55/55; migration up/down/up checked.
  See test-docs/2026-10-16-review-feedback-report.md.

## Access invariants (unchanged)
- Active workspace membership is authoritative; Owner/Admin span all repositories;
  Developer/Reviewer need explicit grants (`workspace_repository_access`: read,
  preview, operate = Developer+, manage = Owner/Admin).
- Membership mutations serialize workspace → installations → PRs and revoke
  private cookies/streams; invitations are digest-only, addressed, seven-day.
- Do not edit technical-docs/inside-previa.md unless asked. No deployment/push.

## Next steps
- User reviews and tests Feature 3, then asks for a commit.
- Feature 4: review rounds/assignments/decisions on publications, review queue,
  activity timeline view (events already recorded), freshness states.
