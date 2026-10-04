# Project handoff
Last updated: 2026-10-04

## Start here
- README.md and DESIGN.md are source of truth; read task-relevant sections only.
- docs/README.md indexes 14 numbered docs in reading order (01 overview, 02 setup,
  03 user guide, 06 source/config, 07 secrets ... 14 roadmap). Update the matching
  doc with every change.
- docs/14-roadmap.md: Prompts 1-2 done; next is Prompt 3 (bounded,
  redacted current-build logs + resumable SSE), then 4-12.
- Contracts to preserve: docs/06 verification/source, 07 secrets, 08 intents,
  09 scheduling, 10 publication, 11 ordering, 12 realtime. Live hosted GitHub
  webhook/mergeability proof still pending (docs/05).
- Backend features flat under internal/<feature>; construction in internal/app;
  presentation in web/ with view models and no backend imports.
- Preserve the "signal" theme, quiet input focus, modal confirmations,
  server-rendered navigation, live search and ordinary GET/POST fallbacks.

## Run and configuration
- make build/test/lint generate assets/templates/sqlc; lint = go vet + golangci-lint.
- PostgreSQL 17 via Compose; make migrate-up applies Goose 12 and River 007.
  Migrate before DB tests: migration-constraint, auth and queue tests use the live
  schema (rolled back); the rest create isolated schemas.
- make run-server/run-worker/dev autoload .env; exported overrides win.
- Server: DATABASE_URL, HTTP/OAuth/session, optional five-setting App group,
  optional GITHUB_WEBHOOK_SECRET, optional SECRETS_ENCRYPTION_KEYS +
  SECRETS_ACTIVE_KEY_VERSION (both or neither; malformed keyring stops startup).
- Worker: ping concurrency, BUILD_TIMEOUT, optional App ID/key pair. It does not
  read the secrets keyring yet; it will need the same keyring from Prompt 5.
- make test-source (opt-in real archive check), make secrets-reencrypt (rotation),
  make test-webhook, make test-github-app. All load .env; no credentials as args.
- Browser tests here need UI_BROWSER_EXECUTABLE=/snap/bin/chromium (Playwright's
  bundled Chromium is not installed).

## Prompt 1: verification and source (done, not wired into execution)
- environments.UpstreamVerifier + UpstreamSnapshot (consumer-owned) implemented by
  pullrequests.SyncService.VerifyUpstream: capture install revision, repo
  observation_version and PR generation; bounded GitHub read (GetByID + PR GET)
  with no transaction; re-lock install->repo->PR; changed => ErrUpstreamChanged.
  Observation committed even on refusal; closed/conflicts/checking/inaccessible/
  unavailable fail closed (checking schedules a recheck). Never authorizes;
  carries event-authorized Automatic intent across a recorded status change.
- internal/source: base repo resolved by numeric ID, tarball for full lowercase SHA,
  credential-free download from allowlisted https host (codeload.github.com, <=3
  redirects), git global header must declare the SHA. Extraction: one top dir,
  canonical local paths, dirs/files/in-tree relative symlinks only (links created
  last, writes via os.Root), byte/entry/path/time caps. 0700 workspaces with a
  marker written first; Workspaces.List/Remove find crash leftovers. Config and
  Dockerfile read from the same tree via repoconfig.Parse and os.Root.
- Real check 2026-10-04 (PR #5, same-repo): header declared the head SHA, extracted
  safely, config outcome missing_file (repo has no .previewrc.yaml); cleaned up.
- Prompt 10 must call VerifyUpstream before RequestBuild and before claimBuild,
  compare snapshot.Matches(request), then source.Preparer.Prepare/Release.

## Prompt 2: repository secrets (done; runtime injection in Prompt 5)
- Migration 12: env_secrets.key_version/updated_at, name and size checks.
- AES-256-GCM, random nonce, AAD = repo ID + name + key version. Keyring env vars
  above; rotation = add version, set active, restart, make secrets-reencrypt
  (optimistic, repeatable, prints counts), remove old key only at zero.
- /repositories/{id}/secrets tab (internal/secrets handler, web/pages/secrets):
  owner-only, CSRF, write-only (names + update time), replace by name, delete via
  modal, limit 100, PREVIA_ prefix reserved, revoked repos delete-only,
  unconfigured server explains itself. RepositoryTabs on the PR list page.
- Worker contract: secrets.Provider.ForContainer(repo, Source{Base,Head}, yamlEnv):
  untrusted (fork/unknown head) decrypts nothing; trusted => secret overrides YAML
  (user decision), notice by name; any decrypt failure fails closed. ContainerEnv:
  Environ for container config only (never build args/context/layers), Wipe.
- secrets.Redactor: exact values + per-line parts, split-safe streaming Writer,
  chunking-independent output, min 4 bytes, max 512 patterns; limits documented.
  Prompt 3 log sink must wrap output with ContainerEnv.Redactor(platform creds).
- Not implemented by design: maintainer approval for forks; lazy re-encryption
  (DESIGN 16 said lazily; explicit command chosen and documented).

## Earlier work still relevant
- SSE live updates: migration 11 triggers -> NOTIFY -> realtime hub -> owner-checked
  /repositories/<id>/events with resumable cursors; no polling (docs/12).
- Webhooks: 2 MiB HMAC-verified ingress outside session/CSRF; atomic delivery receipt
  + PR/lifecycle + intent; source-ordered snapshots; revocation sets cleanup_pending.
- Discovery: open-only, River pr_discovery (2 workers, 45s, <=100/page), revision
  fences; mergeability rechecks with generation/SHA fences; Build disabled (409/503).
- Build/teardown/wake/publication workers remain unregistered; no fake runtime.

## Verification (2026-10-04, Prompts 1-2)
- Each commit checked in a clean worktree: make build (no generated drift), make lint
  0 issues, go test ./... with TEST_DATABASE_URL on the Goose 12 dev DB all pass.
  -race on source, secrets, pullrequests, realtime, environments, config pass.
  Migration 12 up/down verified in a throwaway schema.
- Browser 24/24 including the Secrets tab scenario. Real source check passed.
- Not run: hosted webhook delivery; worker/runtime use of verification, source and
  secrets (later prompts).
