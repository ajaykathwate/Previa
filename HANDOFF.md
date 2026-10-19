# Project handoff
Last updated: 2026-10-19

## Current state
- Branch mvp-complete; F4 included in this single owner-requested commit.
  Previous HEAD205ae68; F3 commit f0fc89b. No push/deployment.
- F0–F4 engineering complete. F5 missed-uninstall polling and F6 early access
  remain out of scope. Owner manual acceptance and policy review precede launch.
- User supplied support email in ignored local .env. Operator IDs deliberately
  empty; comments explain comma-separated numeric GitHub USER IDs, not logins,
  App IDs or installation IDs. Never commit .env or expose its other contents.

## F4 implementation and decisions
- Migration0043 / SchemaVersion43: durable lifecycle operations, revocation
  tombstones, encrypted-journal metadata/events, support/audit/incidents/probes.
  Rollback refuses after lifecycle/revocation data exists; forward-fix then.
- internal/accounts coordinates feature-owned fences/cleanup/erasure contracts.
  Repository/installation disconnect and workspace deletion deny access/new work
  immediately, accept route tombstones, cancel intents/baselines, then retry
  physical runtime/source/baseline/cache removal. Parents wait for children.
  Completion requires physical cleanup AND independent journal upload ACK.
- Explicit repository/installation revocation IDs survive erased product rows;
  stale webhook inserts/updates cannot reconnect them. App uninstall is separate.
- Personal deletion requires transfer/finished deletion of owned workspaces;
  epoch/deleted_at revoke sessions immediately. Profile/avatar/support erased;
  shared attribution remains Deleted member. Seven-day hashed progress receipt.
- Auth checks epoch and original session on requests/live streams. End other
  sessions retains current browser. Private grants also bind to a one-way SCS
  session digest, so ordinary logout/idle expiry revoke them. Restore discards
  old SCS sessions and advances all epochs.
- internal/support: persisted bounded requests, immutable env operator allowlist,
  audited reads/resolutions/incidents and safe cleanup references. Defaults:
  16KiB,5 requests/user/UTC day,resolved retention90d. Independent daily counters
  survive workspace/message erasure; empty operator IDs deny everyone.
- Real Docker/router probes30s; stale90s Unknown. Public policies describe actual
  behavior. Contact/operator name/limits/retention configurable in env.
- Optional Disconnected comments update only existing marked App-owned comments;
  normal report lease, <=3 attempts/10min, permission loss stops immediately.
  GitHub outage cannot prevent local fences/teardown. Configured App ID fallback
  permits execution-worker startup during provider identity-read failure.
- Journal default1MiB/90d, backup30d; unfinished/unacknowledged never expire.
  Host age/rclone publisher every minute via dynamic-IP maintenance journal
  service; upload then ACK under channel lock. Restore stops server/worker/router,
  fetches independent latest, checks identity/time/size/version, replays missing
  events before traffic, clears generated route table and restarts builds paused.
  Pre-F4 dumps lacking journal identity are refused. Off-host backup required.
- DESIGN27/docs34, docs02/env/README/spec/production/runbook/user guide and roadmap
  updated. Test report: test-docs/2026-10-19-accounts-support-and-deletion-report.md.

## Final verification
- Commit preparation: build, ordinary Go tests, vet and lint PASS (0 issues).
  Tests reran outside the socket-restricted sandbox. DB/Docker/browser opt-ins
  were not repeated; full implementation verification is recorded below.
- Build, gofmt, git diff --check PASS. Vet/golangci-lint PASS0 issues.
- All Go packages pass across full run plus fresh reruns: environments444.194s
  with30m aggregate deadline; workspaces112.528s. Initial aggregate timeout and
  concurrent fixture-import artifact are documented in the report.
- Browsers PASS: broad55,workspaces14,setup5,review6,accounts5. JS on/off and CSP;
  new screens captured1440/390 light/dark; representative screenshots reviewed.
- Real TestData451.872s PASS:11 attempts,2 baselines,243.770071s metered; explicit
  disconnect/ACK with complete physical, source, baseline and cache removal.
- Real backend393.14s, ordinary e2e137.74s, reference189.222s PASS. Runtime343.909s,
  routing261.820s,housekeeping28.756s PASS. Reference reran alone after its global
  volume-delta assertion saw concurrent smoke volumes.
- All3 release images f4-local built; encrypted production smoke PASS45 checks,
  restore52s, including missing-journal refusal and post-backup deletion replay.
  ShellCheck0.10 PASS. No live GitHub mutation or live host/R2 recovery claimed.
- Logs /tmp/previa-f4-*.log; all verification jobs finished. Go cache
  /tmp/previa-go-build; lint cache /tmp/previa-golangci-lint. GOFLAGS=-buildvcs=false
  avoids slow Git metadata under WSL. Every shell command uses rtk prefix.

## Next steps / environment
- Development DB was41 and was deliberately NOT migrated. Stop old Air/server
  and worker, run make migrate-up (43 + River), then restart both.
- Production: install/run minute deletion-journal timer with configured encrypted
  off-host backup channel; otherwise deletion deliberately remains pending.
- Disposable DB container previa-member-ux-test-postgres:55439,previa_ux_test;
  leave intact. Isolated test schemas; no developer data erased.
- Preserve deployment205ae68: one OVH VPS2, host-built images, Cloudflare DNS/R2,
  Sentry, Tailscale SSH, lazydocker. No live deployment or extra infrastructure.
- Do not edit technical-docs/inside-previa.md. No theme/library/architecture
  deviation. CWD lowercase Windows path aliases original uppercase path.
