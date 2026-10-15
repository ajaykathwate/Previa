# 22 · Operating the MVP: capabilities, configuration and live proofs

With Prompt 10 every piece is connected: a pull request goes from its exact commit
through configuration, an isolated image build and a hardened, healthy candidate to
an HTTPS route, a PR comment and check run, live logs, expiry and teardown. This
document says what each process does, what each capability needs and what happens
when it is missing, how the work is queued and stopped, how to run the whole
pipeline locally or on staging, what is proven by which test, and every live proof
still required before a public rollout. Observability and hardening are in
[23](23-observability-and-hardening.md), incidents in [24](24-incident-runbook.md),
production deployment in [25](25-production-deployment.md) and the release evidence
in [26](26-release-report.md). Nothing here authorizes public production use before
the pending live checks pass ([27](27-roadmap.md)).

## Who does what

| Process | Does | Never does |
| --- | --- | --- |
| Server (`make run-server`) | Dashboard, sign-in, webhooks, owner actions: records PR observations, build requests (with their durable wake-ups), teardowns (with their durable route removals) and report work; reads fresh PR state from GitHub before a build request; reads which workers can build. | Builds, runs or routes anything; touches Docker. |
| Worker (`make run-worker`) | Everything that executes, composed at start from real, validated dependencies (below); publishes what it can do in `worker_capabilities` every 30 seconds and removes its row on shutdown. | Holds human sign-in or session credentials. |

## Capability and configuration matrix

| Capability | Needs (worker unless noted) | When it is missing |
| --- | --- | --- |
| Build-log retention | The database | Always runs. |
| PR discovery and mergeability | `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_PATH` | Logged as unavailable; sync requests wait. |
| **Builds and previews** (source → config → image → candidate → route) | The App settings; `PREVIEW_BASE_DOMAIN` with `APP_BASE_URL`, `ROUTER_DYNAMIC_DIR` and the `ROUTER_*` settings (all validated); a reachable Docker Engine; the egress image (`make egress-image`); the router running (`make router-up`) | Without `PREVIEW_BASE_DOMAIN`: no build, wake-up, route, cleanup, report or housekeeping worker is registered; requests and wake-ups wait durably, and the dashboard says no worker can build. With it but without Docker: the worker refuses to start. With the router down: activations retry until it answers; the previous preview keeps serving. |
| Repository secrets in previews | `SECRETS_ENCRYPTION_KEYS`, `SECRETS_ACTIVE_KEY_VERSION` (the same keyring as the server) | Repositories without secrets build normally; a repository with secrets fails closed (`secrets_unavailable`), never starting without them. |
| PR comment and check runs | Part of builds; the App's ID and slug are read from GitHub (`GET /app`) at start | Part of builds. |
| Housekeeping (recovery, expiry, orphans, PR reconciliation, pruning) | Part of builds; `ENV_TTL_HOURS`, `HOUSEKEEPING_*`, `BUILD_CACHE_BUDGET` | Part of builds. |
| Build, Retry, Rebuild and teardown controls (server) | The five server App settings, and a worker with builds seen within 90 seconds | The PR page explains why; the endpoint answers 503 and records nothing. Teardown is recorded without a worker and runs when one starts. |
| Builds paused (kill switch) | `BUILDS_PAUSED=true` | Off by default. When on, builds wait (reason `paused`) and the dashboard refuses new requests saying builds are paused; teardown, cleanup, routing, reports and housekeeping continue ([24](24-incident-runbook.md#execution-kill-switch)). |
| Last opened (visit counts) | Part of builds; the router serving Prometheus metrics (`make router-up`; `ROUTER_METRICS_ADDRESS` in production) | The worker logs `preview visits not read`; pages show no "Last opened"; idle previews never sleep. |
| Idle previews sleep | Part of builds; `PREVIEW_IDLE_SLEEP` | Off by default: previews run until they expire. |
| Private previews | `PREVIEW_ACCESS_URL` on the worker (the server's `/previews/auth` as the router reaches it); the GitHub App on the server for repository permissions | Without the URL a private preview serves nobody (fails closed); without the App only the installation's owner gets in. |
| Live app output | Part of builds | Viewers wait on **Connecting** until a worker with builds starts. |
| Health and metrics | `INTERNAL_ADDR` (server), `WORKER_INTERNAL_ADDR` (worker) | Always on, loopback by default; `/readyz` lists failing dependencies ([23](23-observability-and-hardening.md)). |
| Source workspaces | `SOURCE_WORKSPACE_ROOT` (default `previa-sources` in the system temporary directory; keep it on a Linux filesystem) | Defaulted. |

The worker's startup log states it: `River worker started execution=true
reporting=true housekeeping=true discovery=true build_workers=2`, preceded by a
`capability unavailable` warning with the reason for anything missing.

## Queues, concurrency, timeouts and draining

| Queue | Workers per process | Work and timeout |
| --- | --- | --- |
| `build` | `MAX_CONCURRENT_BUILDS` | One build attempt: `BUILD_TIMEOUT` plus one minute to record it. Builds are also bounded across all workers by the same number (build slots). |
| `teardown` | 2 | Starts a teardown: 3 minutes. |
| `environment_coordination` | 4 | Wake-ups (deadlines, handoffs): 15 seconds. |
| `environment_route` | 2 | Route activation and removal: 2 minutes each (+30 s bookkeeping), under a 4-minute operation lease. |
| `environment_report` | 4 | PR comment and check run: 1 minute (+30 s), under the reporter's 2-minute lease. |
| `environment_cleanup` | 2 | Owner-verified removal of containers, images, networks: 90 seconds (+30 s). |
| `environment_teardown` | 1 | Revocation sweep: 2 minutes. |
| `housekeeping` | 1 | Periodic: recovery every minute (2-minute timeout), resources every 5 minutes (5), PR reconciliation every 15 minutes (5), expiry every 10 minutes (2), pruning hourly (15), record retention hourly (10). |
| `pr_discovery`, `build_log_prune`, `ping` | 2, 1, `MAX_CONCURRENT_BUILDS` | As before. |
| `app_output` | 16 | One follower per watched preview: until nobody watched for a minute, at most 2 hours. |
| `idle` | 4 | `preview_sleep` every 5 minutes (4-minute timeout) when `PREVIEW_IDLE_SLEEP` is set; `preview_wake` per waiting visitor (5 minutes). |
| `route_sync` | 1 | Rewrites the router's file after a repository's previews turn private or public. |

Beside River, the worker runs two loops: the crash watch (Docker events) and the
visit count read (every minute). Both end with the process.

- **Retries.** A permanent failure (invalid configuration, failing Dockerfile, app
  never ready, time limit) ends the request after one attempt and is reported once.
  Transient failures get up to five attempts in all, with backoff. Lack of capacity or
  disk is a wait, not a failure ([15](15-housekeeping-and-capacity.md)). A GitHub
  rate limit waits for GitHub's own delay. None of these spend an attempt.
- **Freshness.** The server reads the PR from GitHub before recording a request; the
  worker reads it again before admitting the build, and the runtime rechecks access
  before secrets. Every read happens outside database locks; a newer commit or a
  close recorded meanwhile supersedes the request.
- **Draining.** On `SIGTERM` the worker first removes its capability row, so the
  server stops counting on it, then lets running jobs finish for 30 seconds and
  cancels the rest. A cancelled build is not a failure: it runs again on the next
  start without spending an attempt. Operations and reports resume from their
  durable rows; the housekeeping recovery job repairs anything a crash lost.

## Running the whole pipeline (local or staging)

1. `make migrate-up` (Goose 20) and `make egress-image`.
2. The routing and `ACME_*` settings in `.env`, then `make router-up`. On one
   machine use `PREVIEW_BASE_DOMAIN=preview.localtest.me` with a placeholder DNS
   token and `ACME_CA_SERVER=https://127.0.0.1:1/directory`, as in the
   [user guide](../technical-docs/user-guide.md#0-before-you-start). With a real
   domain, its wildcard record points at the Docker host and the token is real
   (Let's Encrypt **staging** first, [12](12-routing-and-https.md)).
3. In `.env`, for the worker: the two App settings, `PREVIEW_BASE_DOMAIN`,
   `APP_BASE_URL`, `ROUTER_DYNAMIC_DIR`, the secrets keyring if repositories use
   secrets; optionally `ENV_TTL_HOURS` and the housekeeping settings.
4. `make run-server` and `make run-worker`. **Expected:** the worker logs
   `River worker started execution=true …`; the Overview shows every pipeline stage
   as **Connected**.
5. Link a repository, open a pull request, and on its page choose **Build latest
   commit** (or enable Automatic builds and push). Builds shows it queued and
   building, Build logs stream the output, the PR gets one comment and a check run,
   and the preview answers at `https://<label>.<PREVIEW_BASE_DOMAIN>`.
6. **Tear down preview** (confirmed in a dialog) or closing the PR removes route and
   containers; the comment says so.

Builds do not need a real domain: with `preview.localtest.me` the router serves
Traefik's self-signed certificate (browsers warn; accept it for testing) and every
step above works on one machine. A real domain and DNS token are needed only for
trusted certificates. `make test-e2e` proves the same pipeline automatically. For a production host use the
release images and Compose file instead ([25](25-production-deployment.md)).

## What is proven, and by which test

| Scenario | Proof |
| --- | --- |
| Real source → config → image → healthy candidate → HTTPS route → PR report → logs → teardown | `make test-e2e` (`TestRealEndToEnd`: the worker's own pipeline on real Docker, BuildKit and Traefik, a fake GitHub over TLS; owner teardown and a final PR close each leave no container, image or network) |
| Manual push waits | `TestRealEndToEnd`, `TestSchedulingManualObservationAndClosedTeardown` |
| Automatic bursts coalesce; deadline resets | `TestRealEndToEnd` (only the newest commit's source is fetched), `TestSchedulingAutomaticDeadlineResetOldWakeAndOverride` |
| Mode switch | `TestIntentModeChangeCancelsQueuedButPreservesRunningAndManual`, `TestIntentObservationManualAutomaticAndOverride` |
| Duplicate click; Retry and Rebuild with expected revision | `TestBuildRetryRebuildAreIdempotentAndRevisionChecked`, `TestIntentManualIdempotencyExplicitRetryAndRebuild` |
| Same-commit retry | `TestIntentConcurrentRequestsAndSameSHARetries`, `TestSchedulingSameGenerationRebuildFencesOperationsAndCleanup` |
| Failed rebuild keeps the old preview | `TestRealEndToEnd`, `TestRealRoutingServesStableHTTPSAndKeepsThePreviewThroughAFailedSwap`, `TestFailedRebuildPreservesServingPreview` |
| Fork secret withholding | `TestRealRuntimeWithholdsSecretsFromForks` |
| Mergeability changes | `TestRealGitConflictBlocksManualAndScheduledBuildsUntilResolution`, `TestNullMergeabilityPollsAndFencesConflictResolution`, `TestWorkspaceHTTPAuthorizationCSRFActions` |
| Access changes | `TestRealRoutingRevocationTearsDownWithoutGitHubAccess`, `TestSchedulingWorkerRechecksRevokedOrCancelledAccess`, `TestAdmissionReadsThePRFreshFromGitHub` |
| Fresh GitHub read before a request | `TestBuildUsesTheCommitGitHubReportsNow`, `TestVerifyUpstreamRecordsFreshStateWithoutAuthorizing` |
| Close mid-build | `TestRealRoutingCloseWhileBuildingCleansTheCandidate`, `TestCloseWinsOverInflightBuild` |
| Restart recovery | `TestRealRoutingRecoversAfterControllerAndRouterRestarts`, `TestRecoveryResumesLostJobsLeasesAndExhaustedRetries`, `TestAWorkerShutdownMidBuildIsNoFailure` |
| Permanent failures end at once | `TestPermanentFailuresEndAfterOneAttempt` |
| No worker that can build | `TestBuildIsUnavailableWithoutAWorkerThatCanBuild`, `TestPipelineRegistersOnlyWhatItsDependenciesAllow`, `TestCapabilitiesComeFromLiveWorkersOnly` |
| Cross-owner rejection | `TestActionsRefuseAnotherOwnersPullRequest` (build, Retry, Rebuild and teardown of another owner's real PR: 404, nothing recorded, absent from Builds and Previews), `TestWorkspaceHTTPAuthorizationCSRFActions`, `TestTeardownIsConfirmedOwnerOnlyAndIdempotent` |
| Teardown confirmed in a modal, owner-only, CSRF-protected | `TestTeardownIsConfirmedOwnerOnlyAndIdempotent`, browser tests (with and without JavaScript) |
| Dashboard pages show real state | `TestEnvironmentsAndBuildsShowRealDeliveryState`, browser tests |
| A GitHub outage never blocks publication; reports catch up | `TestRealEndToEnd` (comment and check-run API failing during a build) |
| Execution kill switch | `TestPausedBuildsWaitWithoutTouchingGitHubOrDocker`, `TestPausedWorkersSayWhyNothingBuilds`, `TestBuildIsUnavailableWithoutAWorkerThatCanBuild` |
| Ingress controls, security headers, internal endpoints, redaction, bounded metrics | `internal/platform/httpx`, `logging`, `metrics` tests; browser suite under the CSP |
| Alerts | `make observability-check` (promtool rule unit tests) |
| Production stack, backup and restore, upgrade and rollback, older-schema restore | `make prod-smoke` ([25](25-production-deployment.md#local-staging-smoke-test)) |

## Live proofs still required before a public rollout

Each needs something this environment does not have; none is claimed.

1. **Hosted GitHub webhooks** delivered to a public HTTPS endpoint, and discovery
   against a real installation ([05](05-webhooks-and-pr-discovery.md)).
2. **Real source** of a real PR through `codeload.github.com` (`make test-source`).
3. **PR feedback on a real PR** (`make test-reporting`, with an authorized
   disposable PR in `TEST_GITHUB_*`).
4. **Let's Encrypt staging, then production**, wildcard certificate over real DNS-01
   (a domain and DNS token).
5. **The whole pipeline on the target host** with a real repository: build, publish,
   comment, Retry/Rebuild, expiry, teardown on close and on revocation.
6. **Capacity and disk pressure on the target host's real resources**, with
   `MAX_CONCURRENT_BUILDS` concurrent builds.
7. **gVisor** (`PREVIEW_OCI_RUNTIME=runsc`) if the threat model requires it.
8. **The production stack on the target host**: `setup.sh`, the release images,
   staged certificates, the backup timer with a real object-storage remote, a
   restore rehearsal there, and an upgrade ([25](25-production-deployment.md)).
   `make prod-smoke` rehearses all of it on local staging.
9. **CI and releases on GitHub**: the workflows have not run on GitHub yet, the
   protected `release` environment and its reviewers need configuring, and
   publishing images to GHCR needs a tag pushed by a maintainer.
10. **Alert delivery**: an Alertmanager and a receiver of the operator's choice.

One worker host is supported: source workspaces and the route file are per host.
