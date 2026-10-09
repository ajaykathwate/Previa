# 06 · Image builds

**Module:** IMG · **Code:** `internal/builds` · **Feature docs:**
[09 · Image builds](../docs/09-image-builds.md)

Building the pull request's Dockerfile in an isolated, rootless BuildKit sandbox.
The Dockerfile is attacker-controlled, so the build must reach nothing but the
public internet through the egress proxy, and must stay inside its limits.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Building and caching | TC-IMG-BLD | 3 |
| Isolation | TC-IMG-ISO | 2 |
| Limits and failures | TC-IMG-LIM | 3 |

## 1. Building and caching (TC-IMG-BLD)

- [x] **TC-IMG-BLD-001 · A build produces a labelled image and reuses its cache**  
  Type: Positive · Priority: P1 · Level: System (real Docker, BuildKit)  
  Steps: Build a commit; rebuild a new revision; retry the same revision.  
  Expected: The image carries Previa's labels and no proxy settings; the rebuild uses the cache under a new tag; a retry reuses the revision's image; two revision images exist.  
  Evidence: `internal/builds` › `TestRealBuilderBuildsCachesReusesAndLabels`

- [x] **TC-IMG-BLD-002 · Image identities and cache scopes are distinct where they must be**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Derive image references and cache scopes for two revisions, same-repository PRs, a fork and an unknown head; validate malformed jobs.  
  Expected: Revisions never share an image; same-repository PRs share a cache; a fork and an unknown head get their own; invalid jobs are refused.  
  Evidence: `internal/builds` › `TestImageIdentityAndCacheScopes`; `internal/builds` › `TestJobValidation`

- [x] **TC-IMG-BLD-003 · A builder that cannot start says why**  
  Type: Resilience · Priority: P2 · Level: Unit  
  Steps: Start a builder whose daemon exits at once; cancel while waiting for readiness.  
  Expected: The failure includes the exit code and a bounded excerpt of the startup log; cancellation is reported as cancellation.  
  Evidence: `internal/builds` › `TestStoppedBuilderReportsStartupDiagnostics`; `internal/builds` › `TestBuilderReadinessPreservesCancellation`

## 2. Isolation (TC-IMG-ISO)

- [x] **TC-IMG-ISO-001 · Build steps reach only the public internet, through the proxy**  
  Type: Security · Priority: P1 · Level: System (real Docker, BuildKit)  
  Steps: Run a Dockerfile that probes the internet directly, a service on the host network directly and through the proxy, the Docker host, cloud metadata, the Docker socket and worker credentials.  
  Expected: Every probe is blocked except a public destination through the proxy.  
  Evidence: `internal/builds` › `TestRealBuilderIsolatesBuildSteps`

- [x] **TC-IMG-ISO-002 · Fork builds never share a cache; concurrent builds never share one either**  
  Type: Security · Priority: P1 · Level: System (real Docker, BuildKit)  
  Steps: Build the same Dockerfile for a trusted PR and a fork; run two builds of one scope at once.  
  Expected: The fork does not reuse the trusted cache and gets its own volume; the second concurrent build runs without the cache instead of sharing it.  
  Evidence: `internal/builds` › `TestRealBuilderSeparatesForkCachesAndSharesNone`

## 3. Limits and failures (TC-IMG-LIM)

- [x] **TC-IMG-LIM-001 · Every limit is enforced and every failure is classified**  
  Type: Negative · Priority: P1 · Level: System (real Docker, BuildKit)  
  Steps: Build a malformed Dockerfile; a failing step; one exceeding memory, image size and disk budget; one exceeding the timeout; cancel one; produce huge output.  
  Expected: Each fails with its own code and a log line saying why; the timeout is enforced promptly; no sandbox or image is left behind.  
  Evidence: `internal/builds` › `TestRealBuilderEnforcesLimitsAndClassifiesFailures`

- [x] **TC-IMG-LIM-002 · Failure messages and exports are bounded**  
  Type: Edge · Priority: P2 · Level: Unit  
  Steps: Classify build failures; summarize a very long error; write past the export cap; parse `df` output.  
  Expected: Stable codes and bounded summaries; the export stops at the cap and the reader sees the failure; free disk is parsed or refused.  
  Evidence: `internal/builds` › `TestFailureClassificationAndMessages`; `internal/builds` › `TestCappedWriterStopsTheExportAtTheLimit`; `internal/builds` › `TestParseDFAvailable`

- [x] **TC-IMG-LIM-003 · Low disk makes a build wait, not fail**  
  Type: Resilience · Priority: P1 · Level: Integration + System  
  Steps: Admit a build when free disk is below the floor; clear disk.  
  Expected: The request waits with reason "disk"; housekeeping prunes caches when a build waits for disk; the build then runs.  
  Evidence: `internal/environments` › `TestRuntimeCapacityRefusalWaitsInsteadOfFailing`; `internal/housekeeping` › `TestHousekeeperPrunesWhenABuildWaitsForDiskAndRegistersItsJobs`
