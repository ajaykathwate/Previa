# Project handoff
Last updated: 2026-10-23

## Latest task — owner requests after the regression (committed)
- Branch mvp-complete, PR #2 into master. Commit only when the owner asks.
- Open Source Tested (nav/footer/home/page title; URL /open-source-tests kept): ten
  projects (mermaid-live-editor, umami, miniflux, fastapi template, bakerydemo,
  saleor, sure, spring-petclinic, bookstack, docmost); no per-project status or Core
  tags (TestPageRendersEveryProjectOnceWithoutStatuses); each card's disclosure is
  "Test it yourself". Caveat for the owner: none of the ten has a recorded live run.
- Logout → account switch: logout sets previa_signed_out (__Host- over HTTPS, 30
  days); next GitHub login adds prompt=select_account until a sign-in completes;
  connecting GitHub from the profile always does. Navbar Get started goes to
  /auth/login when Google is configured (components.SignInURL). Docs 04, 36, DESIGN.
- Production refuses EARLY_ACCESS_ENABLED=true with empty SUPPORT_OPERATOR_GITHUB_IDS
  (server config + check-config); development logs a warning. prod smoke env sets
  operator 1. Docs 02, 25, 35, both env examples.
- Flaky browser tests: root cause was the live stream's first refetch swapping the
  panel; htmx wires new fields after its 20 ms settle, so a fill in that window
  was lost. ui.test.cjs liveIdle() now waits before typing on live pages.
- Google sign-in: the owner created a Google OAuth client locally (docs/02 F7).
- .previewrc.yaml so Previa previews itself (dashboard
  on managed PostgreSQL 17, setup previa-migrate up; secrets SESSION_SECRET and a
  previews GitHub OAuth App; APP_BASE_URL added after the first build; docs/02
  "Preview Previa on Previa"); Dockerfile stage order now ends with server (release
  builds all name targets, instructions identical; verified by a real no-target
  build, read-only run, 200 on own host, 421 by address). Sign-in choices page
  redesigned (web/pages/auth/login.templ, .auth-* in workspace.css, GoogleMark).
- CI on PR #2 (mvp-complete → master) failed twice, both fixed: go.mod was not tidy
  (go-jose is a direct import; CI runs go mod tidy + git diff), and the running-build
  browser scenario read the tracker across a live-refresh swap (now one snapshot).
  Local replay of the checks job: generate, build, lint, vuln, race suite 54 pkgs,
  observability; browser 55/55. The Docker job's prod smoke then failed in the backup
  drill: deletion-journal.sh was 100644 in Git (added from /mnt/c), so backup.sh and
  the systemd journal unit could not run it in production either. Now 100755, guarded
  by TestDeployScriptsAreExecutableInGit; full smoke incl. drill run from a Linux clone.
- Every command may be prefixed /home/ajaykathwatee/.local/bin/rtk. No subagents.
  Do not edit technical-docs/inside-previa.md.

## Previous task — full regression (report test-docs/2026-10-22-full-regression-report.md)
## Defects fixed (each with a test)
- D-01 Go 1.26.10 (GO-2026-6809, reachable crypto/x509): go.mod, tools/go.mod,
  Dockerfile GO_IMAGE digest (index digest fe3da92a…), docs/02, docs/23, tech docs.
- D-02 production data race: tailwind-merge-go v0.2.0 shared LRU → v0.2.3;
  web/components/utils/utils_test.go. Real class sets verified identical.
- D-03 /workspaces/{id}/delete 503 → internal/app/accounts.go workspaceDeletion.
- D-04 pending workspace Install GitHub bare 403 → earlyaccess Guard redirects forms
  to /early-access; Repositories shows status, no button.
- D-05 Reviewers no longer see/open Early access (sidebar + workspaces middleware).
- D-06 missed pushes: pullrequests.Origin (Discovered/Confirmed/Pushed);
  housekeeping uses ConfirmUpstream; SyncArgs.Confirm for Sync button. A new commit
  on a tracked open PR schedules Automatic work (never forks; Manual records only;
  connection discovery and pre-build VerifyUpstream never start work).

## Owner-requested product changes (docs updated)
- "Only what a person can use" (docs/21): no disabled controls; role-based hiding;
  temporary blocks as status text. Exceptions kept: busy "Starting…" buttons and
  type-DELETE confirm dialogs. Reviewers do not see View on GitHub (docs/30).
- Automatic countdown (docs/16): components.Countdown + ui.js initCountdowns;
  ListPRs now selects build_available_at/request_source (sqlc regenerated).

## New tests
- internal/app/access_matrix_test.go: TestEveryRouteRespectsEveryRole (accessMatrix,
  accessFingerprints, accessNavigation), TestPagesOfferOnlyWhatPeopleCanUse, dump
  helpers (ACCESS_MATRIX_DUMP, USABILITY_DUMP). A new route must be classified.
- internal/app/e2e_roles_test.go: TestRealRoleJourney (in make test-e2e regex).
- server.go seam newServerFeaturesWithApp; e2e fake GitHub has per-login permissions.

## Verification (this cycle)
- go test -race -shuffle -p=3 ./...: 53 pkgs, 701 top + 1181 sub, 0 races.
  Use -p≤3: higher parallelism exhausts Postgres locks (SQLSTATE 53200).
- Real Docker: builder 6+8, runtime 11+5, routing 8, housekeeping 2, e2e 5 journeys
  (incl. role journey) all PASS.
- Browser: 7 suites PASS; main suite 54/55 per full run then (slow search /
  never-answers flakes), hardened 2026-10-23 (see Latest task).
- release-images (Go 1.26.10 pin) + prod-smoke 33/33 PASS (no age installed).
- make lint 0 issues; make vuln 0 reachable.
- Live GitHub (ajaykathwate/previa-live-test, DB previa_live in the test container):
  sign-in, early-access approval, install, build, trusted HTTPS preview, PR report,
  Reviewer invite (2nd account ajaykathwatee), missed-push recovery → Deployment 2.
  Not done live: resolve on Deployment 2, UI teardown, PR close (covered by role
  journey). Live preview resources removed; router restored (dev routes v33, stopped).

## Owner actions pending
- Untick Active on the GitHub App webhook (URL points at a dead trycloudflare tunnel;
  the API cannot toggle Active). Close PR #1 of previa-live-test when convenient.
- Still from earlier: real Google OAuth client/env, migration 46 on intended DB,
  SUPPORT_OPERATOR_GITHUB_IDS for production (now enforced), release/deployment
  acceptance.

## Environment / boundaries
- Disposable DB previa-f56-tests-postgres 127.0.0.1:55439: previa_ux_test (tests),
  previa_live (live run evidence). Never migrate the developer DB (previa-postgres-1).
- GOCACHE=/tmp/previa-go-build GOFLAGS=-buildvcs=false; UI_BROWSER_EXECUTABLE=
  /snap/bin/chromium. /tmp is wiped on reboot; recreate env exports as needed.
- After an interrupted real suite, remove only its labelled fixtures (docs/02).
- Pre-existing: internal/installations/service.go not gofmt-formatted at base;
  an old git stash "unfinished work by claude code" exists — leave both alone.
