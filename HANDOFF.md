# Project handoff
Last updated: 2026-10-19

## Current state
- Branch mvp-complete; F4 commit9ca5c64 precedes the owner-requested single
  F5/F6 commit. No push or deployment performed.
- All in-scope F0–F6 engineering implemented. MVP-TODO marks automated F5/F6
  requirements/common gate done; owner manual/policy acceptance, push, release
  rehearsal and live deployment checks remain open.
- Owner chose operator approval per new workspace, existing ones grandfathered.
  Ignored .env retains configurable support email and empty numeric operator IDs
  with comments. Two new env settings appended without exposing other values.

## F5/F6 implementation and decisions
- Migrations0044/0045; SchemaVersion45. Reconciled/requested timestamps, workspace
  pending/approved and operator audit. SQLC/Templ regenerated; no new library.
- App-JWT identity check then exact installation404/suspension or complete
  paginated numeric repository IDs. Snapshot revision before ALL external IO.
  Bound25 installations,100x100 repositories; observe whole batch before applying.
  Outages/401/rate/partial/wrongApp change nothing. Defaultmax5 and all-known
  circuit stop excessive losses, including sole-known installation/all its repos.
- Uses existing webhook lifecycle/RevokeTx/tombstones/cleanup/sweep transaction.
  Signed webhook revision wins races; late loss delivery does not advance cleanup
  twice. Polling never restores access or authorizes a build.
- Token-mint403/404 record suspicion + deduplicated River wake-up, never revoke
  from request/build/report path. Startup/5min queue runs without Docker/paused.
- Verified reconnect can confirm old404 and use same revocation path to preserve
  history only in the same authorized workspace. Never moves another team's
  history; restoration starts no build. Explicit reconnect can resolve sole oldID
  despite automatic all-known breaker. Metrics6 bounded outcomes + Sentry/alerts.
- Earlyaccess package native status/operator pages, CSRF, immutable operator IDs,
  revision/audit/idempotency. Existing workspaces approved; new/default pending.
  Connection transaction + physical PR/baseline admission/execution/renewal/
  publication enforce approval without allowance spend. Support/settings/members/
  account exits/cleanup usable. Approval starts no build. Callback authority stays
  with original session workspace if another tab selects a different one.
- EARLY_ACCESS_ENABLED=true and INSTALLATION_RECONCILE_MAX_CHANGES=5 in env
  examples/local.env, documented range1–100. Server and worker must agree.
  SUPPORT_OPERATOR_GITHUB_IDS uses numeric GitHub USER IDs, not names/App/install
  IDs. Empty means nobody can approve; configure before inviting new workspaces.
- README/spec/DESIGN28–29, docs35/02/04/05/12/14/24/27, user guide L1–L9,
  stack/schema diagrams and MVP-TODO updated. No theme/architecture deviation;
  old same-owner rule maps to current shared-workspace authority, documented.

## Final verification
- make build / go build ./... PASS; vet/golangci-lint PASS0 after final Go edits.
- Full Go suite PostgreSQL -p2 timeout30m PASS51 tested packages. Later small
  connection/revision-order refactors PASS targeted accounts58.301s/app0.025s,
  full installations4.300s, callback guard0.008s. Both44/45 down/up PASS.
- Browsers PASS94 scenarios: broad55,workspace14,setup5,review6,testdata3,usage4,
  accounts7; JS on/off and CSP. Initial review modal timeout; all6 pass on fresh
  rerun without product changes, retained in report. Final accounts7 PASS21.597s.
- New pending/approved/operator screens all12 widths/themes captured/reviewed.
  Capture reloads pending page to avoid previous-page view-transition snapshot.
- All4 real E2E PASS620.029s: backend268.32,reference93.34,ordinary69.81,
  persistent188.51. Dropped uninstall proves actual teardown/relink/no build.
  Existing persistent disconnect/journal/source/baseline/cache cleanup retained.
- Real runtime252.084s, routing212.071s, housekeeping17.375s PASS. First combined
  routing process terminated143; standalone rerun passes. Old interrupted instance
  ccfbea4fc23a exact labelled resources/ACME stack/scratch schema cleaned safely.
- Promtool PASS29 alerts+tests/config/28 dashboard rules. Diff whitespace/local
  doc links PASS. Report test-docs/2026-10-19-installation-recovery-and-early-access-report.md.
  Logs /tmp/previa-f56-*.log; all jobs finished. No live GitHub mutations claimed.

## Environment / next steps
- Developer DB intentionally not migrated or processes stopped. Owner stops old
  server/worker, make migrate-up to45+River007, then restarts both.
- New disposable previa-f56-tests-postgres:55439,previa_ux_test,userprevia,
  passwordprevia-test; root45/River7, isolated schemas. Leave container intact.
  Old previa-member-ux-test-postgres no longer exists.
- GOCACHE=/tmp/previa-go-build GOFLAGS=-buildvcs=false; lintcache
  /tmp/previa-golangci-lint. DB/TLS/browser/Docker require socket sandbox approval.
- Every shell command prefixes /home/ajaykathwatee/.local/bin/rtk. Do not edit
  technical-docs/inside-previa.md. CWD lowercase aliases original uppercase path.
- Preserve deployment205ae68: OVH VPS2, host-built images, Cloudflare DNS/R2,
  Sentry,Tailscale,lazydocker, independent minute encrypted deletion journal.
