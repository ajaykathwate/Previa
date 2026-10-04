# Project handoff
Last updated: 2026-10-04

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 17 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06 source, 07 secrets, 08 logs, 09 builds, 10 runtime ... 17 roadmap).
  Update the matching doc with every change; new docs take their reading-order slot.
- docs/17-roadmap.md: Prompts 1-5 done; next is Prompt 6 (stable HTTPS
  routing and fenced blue/green publication, internal/routing), then 7-12.
- Contracts to preserve: docs/06 source, 07 secrets, 08 logs, 09 builds, 10 runtime,
  11 intents, 12 scheduling, 13 publication, 14 ordering, 15 realtime. Live hosted
  GitHub webhook/mergeability proof still pending (docs/05).
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports.
- Preserve the "signal" theme, modal confirmations, server-rendered navigation and
  ordinary GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 13 and River 007 (no new
  migration in Prompt 5). The dev DB was last seen at Goose 12: run make migrate-up.
- Live-schema tests (migration constraints, auth, queue) need the DB at the latest
  migration; others create isolated schemas.
- Worker: BUILD_TIMEOUT; optional BUILD_* limits, BUILDKIT_IMAGE, EGRESS_IMAGE (was
  BUILD_EGRESS_IMAGE) and PREVIEW_* (ALLOW_ROOT, WRITABLE_ROOTFS, TMPFS_SIZE, PIDS,
  STARTUP_TIMEOUT, MEMORY_BUDGET, CPU_BUDGET, MAX_PER_REPOSITORY, OCI_RUNTIME),
  validated at start, unused until Prompt 10. Worker runs build_log_prune.
- make egress-image (was build-egress-image), make test-builder, make test-runtime
  (real Docker, ~2-3 min each), make test-source, make secrets-reencrypt.
  Browser tests here need UI_BROWSER_EXECUTABLE=/snap/bin/chromium.
- Docker Desktop 29.8 (WSL2). previa-egress container/network stay running after
  real tests; the old previa-build-egress container/image were removed here.

## Prompts 1-4
- Verification + exact-SHA source (06); encrypted secrets, fork withholding,
  Provider.ForContainer, streaming redaction (07); current-attempt build logs with
  epochs, live SSE page, retention (08); isolated rootless BuildKit per build with
  per-source caches and limits (09).

## Prompt 5: preview runtime (done; not wired to the worker)
- Owner decisions: previews reach only public 80/443 via the shared egress proxy;
  strict UID 65532 + read-only root with server-wide opt-outs; host-derived budget
  (60% memory, 75% CPU, 10 per repo, CPU weight 512, OOM adj 500); optional
  PREVIEW_OCI_RUNTIME for gVisor (unverified here).
- Refactor: egress proxy moved to internal/platform/egress (Proxy, Probe, Gateway)
  and became previa-egress (cmd/egress, deploy/egress) for builds and previews;
  shared labels and Exec in internal/platform/dockerx; builds.NewBuilder takes the
  gateway.
- runtime.Runtime implements environments.Runtime: TargetStore recheck (open, same
  generation/SHA, running revision, access, installation) -> source.Prepare ->
  builds.Build -> Release -> recheck -> secrets.ForContainer (forks/unknown heads
  untrusted) -> admit -> network previa-pr-<id> (internal, egress attached) ->
  hardened container -> readiness from the egress container (HTTP 200-399 on
  health_check_path, else TCP; 120 s) -> output capture -> Artifact. Failures
  remove the candidate; crash loop after 3 restarts.
- Resources lists previews+images, orphan images, builder leftovers;
  RemoveContainer/RemoveImage verify labels (not_owned otherwise), not-found =
  removed; Health(containerID) for recovery/UI.
- Prompt 6: router joins previa-pr-<id> networks (public entrypoints only), never the
  control plane; publication decides activation. Prompt 10: wire Runtime into
  BuildWorker with buildlogs.Store via a LogOpener and the secrets keyring on the
  worker; provision environment rows before claims.

## Verification (2026-10-04, Prompt 5)
- make build; make lint 0 issues; go test ./... on a throwaway schema (Goose 13 +
  River 007): all pass. -race: runtime, builds, egress, dockerx, config pass.
- make test-builder (after the gateway refactor): all pass.
- make test-runtime: all pass. From a running preview: direct internet, Docker
  bridge service, another preview, host, metadata blocked (direct and via proxy),
  external DNS unresolvable, public site via proxy reached; UID 65532, read-only
  root, CapEff 0, NoNewPrivs, no IPv6; secret redacted in logs, withheld for forks;
  unhealthy/crash/cancel cleaned; opt-outs; capacity; foreign objects refused.
- Not run: browser suite (no UI change); gVisor; worker dispatch; hosted webhooks.
