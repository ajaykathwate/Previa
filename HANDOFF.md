# Project handoff
Last updated: 2026-10-20

## Current task / do not restart completed work
- F7: final MVP addition, Google sign-in + explicit account linking + verified-email
  Reviewer invitations. Implementation, documentation and automated verification complete.
- Branch mvp-complete, prior HEAD bb4f970 (F5/F6 committed). F7 committed in one
  commit at the owner's request; not pushed. No subagents allowed.
- User wants completion without redoing successful work. Every shell command prefixes
  /home/ajaykathwatee/.local/bin/rtk. Do not edit technical-docs/inside-previa.md.

## Implementation / decisions
- Migration0046 / Schema46: optional unique GitHub ID and Google sub; verified Google
  email; email Reviewer invitations (XOR recipient, pending uniqueness); GitHub-required
  technical-role/owner triggers; deletion journal Google subject. Scratch roundtrip
  passes; rollback refuses after Google/email data exists.
- Auth method identifies local user; memberships/grants assign roles. Google-only
  signup creates no Owner workspace; welcome/join flow. GitHub link required before
  technical roles/new workspace, explicit assignment still required; F6 unchanged.
- Google code flow: PKCE S256, nonce, signed RS256 ID token (issuer/audience/expiry),
  provider/session/cookie-bound one-use state, rotated sessions. Tokens not stored.
  New maintained dependency coreos/go-oidc/v3v3.21.0 (+josev4.1.4); no startup IO.
- Profile Connect Google/GitHub uses authenticated CSRF POST + provider proof, same
  current account. No automatic email merge, identity reassignment or automatic role
  promotion. Separately-created accounts cannot merge in MVP; collision explained.
- Email invitations are Reviewer-only, verified-email matching, consent, original
  expiry/grants/revocation. Copy/share links; no invitation email delivery. Existing
  GitHub invitations preserved. Re-invite Google-only removed members by email.
- Local private-preview grants work without GitHub; Google-only outsider cannot use
  GitHub-reader fallback. Technical/source checks remain enforced.
- Google deletion clears email/profile, retains subject resurrection fence. Journalv2
  includes sub, accepts v1; membership replay uses stable public user reference.
- Optional GOOGLE_OAUTH_CLIENT_ID/SECRET pair in both env examples + ignored.env;
  both blank disables Google, half-config fails. Real client/credentials not created.
- Preserve approved theme; login/profile/consent provider-neutral. All 7 views captured
  at1440/390 light/dark and reviewed, additional mobile connection captures.

## Verification complete (do not rerun)
- make build, go vet, golangci-lint: PASS, 0 issues. make vuln: 0 affecting app;
  one module advisory not in imported/called vulnerable package.
- All52 packages with tests PASS; other57 packages compile/no test files. Original
  process exit143 resumed only unfinished packages; env-example labels fixed and
  config rerun passed. Proof/logs /tmp/previa-google-{all,remaining,config-final}.log.
- Focused auth/accounts/workspaces/previewaccess/config pass: signed token failures,
  provider confusion/replay/CSRF, identity collisions, roles, invitations, erasure,
  restore, migration and Google private-preview local grant/outsider/revocation.
- Existing7 browser suites94/94 PASS: general55, workspace14, setup5, review6,
  testdata3, usage4, accounts7. /tmp/previa-google-regression-browser.log.
- Google browser3/3 PASS27.36s: stakeholder finding with JS on/off; explicit existing
  GitHub->Google link and return; CSRF/role/collision refusals. Final captures reviewed.
  /tmp/previa-google-browser-final.log. Use /snap/bin/chromium, not raw snap binary.
- Local doc links valid; git diff --check PASS. Docs36, docs02, README/spec/DESIGN,
  identity/routing/support docs, tech/schema/userguide M1–M9, test docs updated.

## Completion / next steps
- All4 real E2E journeys PASS708.25s. /tmp/previa-google-e2e.log.
- Affected real private routing PASS11.83s (35.62s package), real Traefik auth,
  configuration recovery and revocation. /tmp/previa-google-private-routing.log.
  Runtime/housekeeping internals unchanged; previous F5/F6 baseline retained.
- Final evidence in test-docs/2026-10-20-google-sign-in-report.md; MVP-TODO marks
  F0–F7 engineering complete and committed. No required code left.
- Owner next: register Google Web OAuth client, configure optional env pair and exact
  APP_BASE_URL/auth/google/callback, migrate intended database to46 and restart
  server/worker. No live Google consent claimed. Manual rowsM1–M9, policy review,
  release rehearsal and deployment/live acceptance remain open.

## Environment / boundaries
- Disposable previa-f56-tests-postgres:55439, DBprevia_ux_test, userprevia,
  passwordprevia-test, Goose46/River007. Never migrate developer DB or stop user jobs.
- GOCACHE=/tmp/previa-go-build GOFLAGS=-buildvcs=false; lintcache
  /tmp/previa-golangci-lint. DB/socket/browser/Docker checks require_escalated.
- Existing egress image previa-egress:dev. No deployment/live provider writes.
  Support email remains env-configurable; operator numeric IDs remain empty.
