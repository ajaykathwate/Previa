# Project handoff
Last updated: 2026-11-01

## Latest task — product README (uncommitted)
- README.md is now the product overview (features, roles, repo files, get started,
  not available yet, docs table). The old requirements/design README moved verbatim
  to REQUIREMENTS.md (git mv); AGENTS.md, DESIGN, MVP-TODO, the expansion plan and
  docs 01/11/12/15/25/27 + docs index now cite REQUIREMENTS.md. test-docs untouched.
- Open: FAQ says previews expire 48 h after publication (code default), but the
  production env example sets ENV_TTL_HOURS=24; README avoids a number.

## go-jose CVE fix (committed 820f213 on mvp-complete, not yet on master)
- Master's release scan failed on CVE-2026-116587 (go-jose/v4 v4.1.4 via go-oidc);
  bumped to v4.1.7. Needs a PR mvp-complete → master, then tag v0.1.0 (no tags yet).
- Production host: OVH VPS-1 Singapore ordered (trial; minimum-host settings in
  docs/25), pending OVH document verification. Sentry project created.

## Previous task — owner requests after the regression (committed)
- Branch mvp-complete, PR #2 into master. Commit only when the owner asks.
- Open Source Tested (nav/footer/home/page title; URL /open-source-tests kept): ten
  projects (mermaid-live-editor, umami, miniflux, fastapi template, bakerydemo,
  saleor, sure, spring-petclinic, bookstack, docmost); no per-project status or Core
  tags (TestPageRendersEveryProjectOnceWithoutStatuses); each card's disclosure is
  "Test it yourself". Caveat for the owner: none of the ten has a recorded live run.
- Logout sets previa_signed_out so the next GitHub login offers an account switch
  (prompt=select_account); Get started goes to /auth/login when Google is configured.
- Production refuses EARLY_ACCESS_ENABLED=true with empty SUPPORT_OPERATOR_GITHUB_IDS
  (server config + check-config); development logs a warning. prod smoke env sets
  operator 1. Docs 02, 25, 35, both env examples.
- Flaky browser tests: a fill during the live refetch's settle was lost; liveIdle().
- .previewrc.yaml so Previa previews itself (server stage last in the Dockerfile;
  secrets SESSION_SECRET + previews GitHub OAuth App; APP_BASE_URL after the first
  build; docs/02 "Preview Previa on Previa"). Sign-in choices page redesigned.
- CI fixes (committed): egress proxy/forwarder try every vetted address (IPv4 first,
  4 s each); Tailwind download retries and is cached in a BuildKit mount.
- CI flakes (2026-10-24): routing stand-in app was a one-connection nc loop (Traefik
  500 on overlap), now `nc -lk -e`; forged-CSRF browser test submitted a form a live
  refresh had just replaced (locator lookup then evaluate), now liveIdle() + query in
  page.evaluate. CI green on f4f6de5 (all 4 jobs). Builder/runtime keep the nc loop.
- Secrets became one .env file per repository (owner decision, 2026-10-23; DESIGN
  §16 deviation). repository_env_files (migration 47, SchemaVersion 47) stores the
  sealed file (AAD ".env"); SaveFile keeps env_secrets = names with values in one tx
  (revision check, removals confirmed in a modal, NAME= kept but unset, 64 KiB, 500
  names); masked view + Show values and edit (Owner/Admin), live size, guidance card
  scrolls at the file panel's height; Reencrypt rotates files (FileRotationStore).
  Old per-line editor and /secrets/delete removed. Docs 02, 07, 21, DESIGN, README,
  FAQ/home copy updated.
- Every command may be prefixed /home/ajaykathwatee/.local/bin/rtk. No subagents.
  Do not edit technical-docs/inside-previa.md.

## Previous task — full regression (report test-docs/2026-10-22-full-regression-report.md)
- Defects D-01..D-06 fixed with tests (details in the report): Go 1.26.10 pin
  (Dockerfile GO_IMAGE digest fe3da92a…), tailwind-merge-go v0.2.3 race, workspace
  delete 503, early-access guards, missed pushes via pullrequests.Origin.

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
- Browser: 7 suites PASS; main suite 56/56 after the 2026-10-24 CSRF-test fix.
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
- Go-live (docs/25 Step 4 item 6, Step 5 table, Step 10 item 1): production Google
  client (publish "In production"), SUPPORT_OPERATOR_GITHUB_IDS, approve own workspace,
  migrations 46-47 on the intended DB. Self-preview .env: docs/02 "Preview Previa".

## Environment / boundaries
- Disposable DB previa-f56-tests-postgres 127.0.0.1:55439: previa_ux_test (tests),
  previa_live (live run evidence). Never migrate the developer DB (previa-postgres-1).
- GOCACHE=/tmp/previa-go-build GOFLAGS=-buildvcs=false; UI_BROWSER_EXECUTABLE=
  /snap/bin/chromium. /tmp is wiped on reboot; recreate env exports as needed.
- After an interrupted real suite, remove only its labelled fixtures (docs/02).
- Pre-existing: internal/installations/service.go not gofmt-formatted at base;
  an old git stash "unfinished work by claude code" exists — leave both alone.
