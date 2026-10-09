# 07 · Preview runtime and backend services

**Module:** RUN · **Code:** `internal/runtime`, `internal/secrets` (provider) ·
**Feature docs:** [10 · Preview runtime](../docs/10-preview-runtime.md),
[11 · Backend previews](../docs/11-backend-previews.md)

Starting the built image as a hardened candidate, with its managed PostgreSQL and
Redis, setup command and processes, and proving it is healthy before anything is
routed to it.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Hardening | TC-RUN-HRD | 4 |
| Isolation and secrets | TC-RUN-ISO | 3 |
| Health and capacity | TC-RUN-CAP | 4 |
| Backend services | TC-RUN-BE | 5 |

## 1. Hardening (TC-RUN-HRD)

- [x] **TC-RUN-HRD-001 · A candidate starts hardened, healthy and unrouted**  
  Type: Positive / Security · Priority: P1 · Level: System (real Docker)  
  Steps: Start a candidate from a built image with a repository secret; inspect the container; remove it.  
  Expected: Non-root user, read-only root, no privileges, ports or host paths, all capabilities dropped, no-new-privileges, limits set; joined only to its own network; not routed; the secret never appears in the log; removal leaves nothing.  
  Evidence: `internal/runtime` › `TestRealRuntimeStartsAHardenedHealthyCandidate`; `internal/runtime` › `TestContainerSpecIsHardened`

- [x] **TC-RUN-HRD-002 · Root images run as an unprivileged user**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Resolve the user for images declaring none, `root`, `0`, `0:0`, `root:staff`, `node` and `1000:1000`, with and without the operator's allowance.  
  Expected: Every form of root (and no user) becomes the unprivileged 65532 unless the operator allows root; named and numeric non-root users are kept.  
  Evidence: `internal/runtime` › `TestRunAsReplacesRootUnlessTheOperatorAllowsIt`

- [x] **TC-RUN-HRD-003 · Operator opt-outs never restore privileges**  
  Type: Security · Priority: P2 · Level: System + Unit  
  Steps: Turn the operator's opt-outs on and start a candidate.  
  Expected: The opt-outs are honoured, but capabilities and privileges are never added back.  
  Evidence: `internal/runtime` › `TestRealRuntimeHonoursOperatorOptOuts`; `internal/runtime` › `TestContainerSpecIsHardened`

- [x] **TC-RUN-HRD-004 · The proxy settings in the environment are authoritative**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Give the app its own `HTTPS_PROXY` and `NO_PROXY=*` alongside ordinary variables.  
  Expected: The proxy variables (both cases) point at the egress proxy, `NO_PROXY` covers only localhost, ordinary variables are kept, and the environment is sorted and deterministic.  
  Evidence: `internal/runtime` › `TestEnvironmentKeepsEgressAuthoritative`

## 2. Isolation and secrets (TC-RUN-ISO)

- [x] **TC-RUN-ISO-001 · A running preview reaches only the public internet through the proxy**  
  Type: Security · Priority: P1 · Level: System (real Docker)  
  Steps: From inside a preview, probe the internet directly, a host service and another preview (directly and through the proxy), the Docker host, cloud metadata and external DNS.  
  Expected: All blocked; only a public site through the proxy is reached; IPv6 is off.  
  Evidence: `internal/runtime` › `TestRealRuntimeDeniesNetworkAccess`

- [x] **TC-RUN-ISO-002 · Forks and unverified sources never receive secrets**  
  Type: Security · Priority: P1 · Level: System + Unit  
  Steps: Start a fork's candidate for a repository with secrets; decide secrets for trusted and untrusted sources; decrypt an undecryptable value.  
  Expected: The fork's container has no secret; trusted sources get secrets over YAML values; an undecryptable value fails closed.  
  Evidence: `internal/runtime` › `TestRealRuntimeWithholdsSecretsFromForks`; `internal/secrets` › `TestProviderWithholdsSecretsFromUntrustedSources`; `internal/secrets` › `TestProviderMergesSecretsOverYAMLForTrustedSources`; `internal/secrets` › `TestProviderFailsClosedOnUndecryptableValues`

- [x] **TC-RUN-ISO-003 · Stale, revoked or foreign work is refused before anything runs**  
  Type: Security · Priority: P1 · Level: Unit + Integration + System  
  Steps: Start a candidate for a stale attempt, a revoked installation, a removed repository and an unverified head; touch another instance's or a foreign container; squat the preview network name.  
  Expected: Stale and revoked targets are refused and secrets are decided only after a fresh recheck; unverified heads are untrusted; foreign objects are never removed; a squatted network is refused.  
  Evidence: `internal/runtime` › `TestCurrentTargetRefusesAnythingStaleOrRevoked`; `internal/runtime` › `TestBuildRefusesStaleAttemptsBeforeAnyWork`; `internal/runtime` › `TestSecretsAreDecidedOnlyAfterAFreshRecheck`; `internal/runtime` › `TestRealRuntimeRefusesForeignObjectsAndFindsLeftovers`

## 3. Health and capacity (TC-RUN-CAP)

- [x] **TC-RUN-CAP-001 · Unhealthy, crashing and cancelled starts fail cleanly**  
  Type: Negative · Priority: P1 · Level: System (real Docker)  
  Steps: Start an app that answers 500, one that crash-loops, and one that stays silent while the build is cancelled.  
  Expected: `unhealthy`, `crashed` and `cancelled` with the reason in the log; no candidate is left running.  
  Evidence: `internal/runtime` › `TestRealRuntimeFailsUnhealthyCrashingAndCancelledStartsCleanly`; `internal/runtime` › `TestObservedHealthDetectsCrashLoops`

- [x] **TC-RUN-CAP-002 · The host's memory budget and per-repository limit are enforced**  
  Type: Edge · Priority: P1 · Level: System + Unit  
  Steps: Start candidates past the memory budget and past the per-repository count, and a swap at the limit.  
  Expected: Refusals are retryable capacity waits; a swap at the limit is allowed.  
  Evidence: `internal/runtime` › `TestRealRuntimeEnforcesCapacity`; `internal/runtime` › `TestAdmitEnforcesRepositoryCountAndHostBudget`

- [x] **TC-RUN-CAP-003 · Concurrent candidates are admitted atomically**  
  Type: Concurrency · Priority: P1 · Level: System + Integration  
  Steps: Start two candidates at once with room for one; take the admission lock while cancelling.  
  Expected: Exactly one starts and one is refused; the lock serializes and survives cancellation.  
  Evidence: `internal/runtime` › `TestRealRuntimeAdmitsConcurrentCandidatesAtomically`; `internal/runtime` › `TestAdmissionLockSerializesAndSurvivesCancellation`

- [x] **TC-RUN-CAP-004 · The target store reads only the current, authorized target**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Read the build target for current, stale and unauthorized PRs; record components.  
  Expected: Only the current authorized target is returned; only served component rows are kept.  
  Evidence: `internal/runtime` › `TestTargetStoreReadsTheCurrentAuthorizedTarget`; `internal/runtime` › `TestRecordComponentsKeepsOnlyServedRows`

## 4. Backend services (TC-RUN-BE)

- [x] **TC-RUN-BE-001 · Each candidate gets its own credentials and connection settings**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Plan two candidates with PostgreSQL and Redis; set `DATABASE_URL` in YAML.  
  Expected: Hostnames and passwords differ per candidate and are registered for redaction; Previa's `DATABASE_URL`, `PG*` and `REDIS_URL` override YAML and secrets with a notice naming them.  
  Evidence: `internal/runtime` › `TestPlanBackendGeneratesPerCandidateCredentials`; `internal/runtime` › `TestPreviaConnectionSettingsWin`

- [x] **TC-RUN-BE-002 · Service, setup and process containers are hardened**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Build the specifications for PostgreSQL, Redis, a setup command and a process.  
  Expected: Services run as their own unprivileged user, read-only, capped, with tmpfs data, only on the services network, no ports; setup and processes run exactly their argv with the app's hardening and their own limits.  
  Evidence: `internal/runtime` › `TestServiceContainersAreHardened`; `internal/runtime` › `TestSetupAndProcessesRunTheirArgvWithTheAppsHardening`

- [x] **TC-RUN-BE-003 · A backend app with PostgreSQL, Redis, a setup command and a worker previews end to end**  
  Type: Positive · Priority: P1 · Level: End-to-end (real Docker, BuildKit, Traefik)  
  Steps: Open a PR of an API that migrates and seeds PostgreSQL and runs a Redis queue worker; build it.  
  Expected: It becomes ready and serves over HTTPS; the worker moves a job from Redis into PostgreSQL; the PR comment names the backend and docs.  
  Evidence: `internal/app` › `TestRealBackendEndToEnd`

- [x] **TC-RUN-BE-004 · A rebuild gets a fresh database; a failed migration keeps the old preview**  
  Type: Edge / Negative · Priority: P1 · Level: End-to-end  
  Steps: Rebuild the backend preview; then push a commit whose migration fails.  
  Expected: The rebuild has a new database identity; the failing build ends `setup_failed` and the earlier preview and its data keep serving.  
  Evidence: `internal/app` › `TestRealBackendEndToEnd`

- [x] **TC-RUN-BE-005 · Services are isolated both ways**  
  Type: Security · Priority: P1 · Level: End-to-end  
  Steps: From PostgreSQL probe the internet, the egress proxy, the host and other previews; from another preview probe the services.  
  Expected: Every probe is refused.  
  Evidence: `internal/app` › `TestRealBackendEndToEnd`
