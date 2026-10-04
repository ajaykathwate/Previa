# Project handoff
Last updated: 2026-10-04

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 16 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06 source, 07 secrets, 08 build logs, 09 image builds ... 16 roadmap).
  Update the matching doc with every change; new docs take their reading-order slot.
- docs/16-roadmap.md: Prompts 1-4 done; next is Prompt 5 (hardened
  Docker candidates and readiness, internal/runtime), then 6-12.
- Contracts to preserve: docs/06 verification/source, 07 secrets, 08 logs, 09 builds,
  10 intents, 11 scheduling, 12 publication, 13 ordering, 14 realtime. Live hosted
  GitHub webhook/mergeability proof still pending (docs/05).
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports.
- Preserve the "signal" theme, modal confirmations, server-rendered navigation and
  ordinary GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 13 and River 007.
  The dev DB was last seen at Goose 12: run make migrate-up, restart server/worker.
- Live-schema tests (migration constraints, auth, queue) need the DB at the latest
  migration; others create isolated schemas.
- Worker: BUILD_TIMEOUT plus optional BUILD_CPUS/MEMORY/PIDS/DISK_BUDGET/
  MAX_IMAGE_SIZE/MIN_FREE_DISK, BUILDKIT_IMAGE, BUILD_EGRESS_IMAGE (validated at start,
  unused until Prompt 10). It always runs the build_log_prune periodic job.
- make build-egress-image (needed before builds), make test-builder (real Docker,
  ~4 min), make test-source, make secrets-reencrypt. Browser tests here need
  UI_BROWSER_EXECUTABLE=/snap/bin/chromium.
- Docker Desktop 29.8 (WSL2) is available; previa-build-egress container/network stay
  running after builder tests (docs/02 shows how to remove them).

## Prompts 1-2
- environments.UpstreamVerifier via pullrequests.SyncService.VerifyUpstream (fresh,
  fenced, fail-closed); internal/source exact-SHA archive, hardened extraction,
  crash-discoverable workspaces, config from the same tree.
- internal/secrets: AES-256-GCM keyring + rotation, owner Secrets tab, fork withholding,
  secret-over-YAML, Provider.ForContainer, streaming Redactor (now holds back only a
  tail that could start a known value, so paused output shows at once).

## Prompt 3: build logs (done)
- Migration 13: build_log_streams (one per environment: epoch, gen/rev, state,
  counters) and build_logs epoch/kind; trigger raises buildlog:<env>.
- environments.claimBuild resets the stream (epoch+1) then clears lines in the
  admission tx; Runtime.Build(ctx, request, Attempt{EnvironmentID, LogEpoch}).
  Service.WithBuildLogs(store) finishes the stream when execution returns.
  No environment row => Attempt{} (no log); rows are provisioned in Prompts 6/10.
- buildlogs.Store.Open(Binding, redactor, Limits) -> Writer: strip controls, redact,
  split (8 KiB), cap (5 MiB / 20k lines, marker), batch, epoch-fenced append,
  Done() on supersede, lost lines counted and marked. Retention 14 days.
- /repositories/{id}/pull-requests/{n}/logs + /events: owner-only, tail 500 lines with
  paging, SSE ids "<epoch>.<next>", reset on new attempt, caps 256/8 per user, 10 s
  frame timeout. Linked from PR detail and Builds/Environments rows.

## Prompt 4: image builds (done; not wired to the worker)
- Owner decisions: fresh rootless BuildKit container per build; public internet
  only via the Go egress proxy (80/443, public addresses, vetted dial); cache per
  base+source repository (DESIGN 9 refinement); Balanced limits.
- builds.Builder.Build(ctx, Job{PR, gen, rev, SHA, base/head repo, ContextDir,
  Dockerfile, Logs}) -> Image{Ref, ID, Size, Reused}; typed failures (builds.Error).
  Resources(pr)/Remove(artifact) for cleanup. Tag previa/pr-<id>:g<g>-r<r>-<sha12>.
- Builder hardening: cap-drop ALL + SETUID/SETGID, read-only root, owned tmpfs,
  seccomp/AppArmor unconfined + unmasked paths (rootless requirement), cgroup limits,
  no socket/host paths. Client v0.33.0 (v0.33.1 needs Go 1.26.8), daemon v0.33.1
  pinned by digest and pulled by Docker when missing.
- Prompt 5 must: build via builds.Builder with a buildlogs.Writer as Logs, release
  the source workspace, then start the candidate; wire Resources into cleanup.

## Verification (2026-10-04, Prompts 3-4)
- make build; make lint 0 issues; go test ./... against a throwaway schema at
  Goose 13 + River 007: all pass except one run where
  TestOlderBuildFinishesAfterNewerResult hit its 2 s operation lease under full
  parallel load (passed in the other full run and 10/10 alone).
- -race: buildlogs, builds, environments, pullrequests, realtime, config, secrets,
  queue pass. Fixed a pre-existing test race in realtime stream_test (WriteTimeout
  set after the server started); 30/30 under -race now.
- Migration 13 down/up verified in a throwaway schema; migration-constraint test
  covers 0013 and passes at Goose 13. Browser 26/26 (two new log scenarios).
- make test-builder from a cold image cache: all pass; probes from a build step:
  direct internet, Docker-bridge service, host, metadata blocked (direct and via
  proxy); no socket, no credentials; public site via proxy reached.
- Not run: hosted webhook delivery; runtime/worker use of logs and builder.
