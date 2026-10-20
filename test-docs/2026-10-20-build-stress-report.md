# Build flow · aggressive verification

Date: 20 October 2026. Branch `mvp-complete`, base `75f5681`.
The owner requested a fresh aggressive pass restricted to build functionality.
**All executed build-focused checks passed. No product defects or data races were
detected.** This task adds regression/fuzz tests and evidence; application code is
unchanged.

## Conditions exercised

| Build area | Conditions and expected safety properties |
| --- | --- |
| Requests and policy | Manual/Automatic, quiet period/reset, Build now override, explicit Retry/Rebuild, duplicate submissions, stale form revision, latest SHA, closed/merged PR, fork policy and merge conflicts |
| Authorization and lifecycle | Current GitHub source permission, Reviewer/technical-role boundaries, pending workspace approval, revoked actor/repository/installation, disconnect during work; no unauthorized execution or publication |
| Scheduling and capacity | Concurrent PR requests, shared workspace limits, global slots, fairness, host headroom, low disk, paused builds, exhausted quota and preview slots; waits preserve request/log state |
| Recovery and ordering | Worker shutdown, expired lease, lost wake-up, exhausted retries, duplicate physical attempt, newer revision/generation, close/reopen and delayed external publication; stale work cannot replace live work |
| Source and configuration | Exact archive SHA, redirects/auth boundaries, malformed/truncated/oversized archives, traversal/symlinks/overlap, context isolation, LFS/submodules, strict YAML/types/duplicates/size, limits and effective hashes |
| Image execution | Success/cache/reuse, trusted/fork/baseline cache separation, registry restrictions, build-secret mounts/redaction, malformed Dockerfile, failed step, OOM, image/disk/output limits, timeout/cancellation and cleanup |
| Runtime/services | Hardened containers, non-root/resource/network limits, PostgreSQL/MySQL/Redis, per-candidate credentials, setup/seed/processes, startup health/crash/cancellation, capacity and atomic candidate admission |
| Persistent data | Baseline encryption/size/checksum, refresh/coalescing/recovery, kept data, cross-PR isolation, both reset sources, incompatible schema fallback, broken migration preserving live data, forks and merge refresh |
| Publication and teardown | Stable HTTPS URL, successful swap, failed activation preserves prior preview, concurrent close/supersession, controller/router restart recovery, private access, orphan cleanup and other instances left alone |
| Build experience | Build/Retry/Rebuild controls, duplicate/slow requests, unavailable/blocked/failing/running builds, queue status, build logs/output, setup check and test-data controls; JS and no-JS where covered |

## New tests

- `internal/environments/burst_test.go`: two workspaces, 16 simultaneous build
  submissions and four global/workspace limit combinations. Covers legacy safety
  caps and actual free-plan entitlements. Runtime admissions and ledger reservations
  must exactly match permitted executing work; capacity waits never run or reserve
  allowance. Each group is repeated five times: **40 bursts / 640 submissions**.
- `internal/repoconfig/fuzz_test.go`: arbitrary YAML with varying operator CPU,
  memory and timeout ceilings. Accepted configurations remain bounded, have a valid
  SHA-256 identity and parse deterministically. Includes backend and malformed seeds.
- `internal/source/workspace_failure_test.go`: real unavailable OS workspace handle,
  file/directory/symlink failures, retry classification, safe messages, no partial
  writes and successful retry. Three cases repeated five times, all passed.
- `internal/source/fuzz_test.go`: arbitrary archive paths and symlink targets,
  traversal, NUL, long names and multiple roots. Validation is exercised without
  extracting attacker-generated files onto disk.

## Results

| Suite | Result |
| --- | --- |
| 19 build/support packages, race detector + shuffled order + coverage | Pass, 420 top-level and 239 subtest results; opt-in skips accounted for separately |
| Assembled build capability/capacity/source checks, three repetitions | Pass, 15 test executions |
| Multi-workspace admission bursts, five repetitions | Pass, 20 bursts / 320 simultaneous submissions |
| Free-plan ledger admission bursts, five repetitions | Pass, 20 bursts / 320 simultaneous submissions; no waiting-work ledger reservations |
| Six quota/cancellation/replay cases, ten repetitions | Pass, 60/60 test executions |
| Build-specific approval/revocation/publication fences, three repetitions | Pass, 27 top-level and 30 subtest executions |
| Build reporting and upstream token/error handling, race detector + shuffled order | Pass, 48 test/subtest results; live App/reporting checks excluded |
| Generation/revision/close/reopen/recovery races, five repetitions | Pass, 40/40 test executions |
| Source workspace I/O failure and recovery, five repetitions | Pass, 15 subcases |
| Configuration fuzz campaign, 60 seconds with race detection | Pass, 21,498 executions |
| Archive-path fuzz campaign, 60 seconds with race detection | Pass, 12,833 executions |
| Build-related browser scenarios | Pass, 19/19 |
| Setup browser | Pass, 5/5 |
| Persistent-data browser | Pass, 3/3 |
| Real BuildKit, race detector + shuffled order | Pass, all 6 top-level cases and 8 limit/failure subcases (271.05 s command) |
| Real runtime/services, race detector + shuffled order | Pass, 11 top-level cases and 5 subcases (298.83 s command) |
| Real routing/publication, race detector + shuffled order | Pass, 8/8 top-level cases (225.44 s command) |
| Real housekeeping/resource cleanup, race detector + shuffled order | Pass, 2/2 top-level cases (22.48 s command), including other-instance isolation |
| Four real worker build journeys, race detector + shuffled order | Pass, 4/4 (752.62 s command): backend 273.95 s, persistent data 285.43 s, references 111.37 s, standard 75.54 s |
| Build, vet and lint | Pass, final vet/lint 0 issues after all added tests |

The five sequential real-Docker stages passed **31 top-level tests and 13
subcases**, including all four composed worker journeys. All additional stress
repetitions passed. There were no application fixes to make during this pass.

Raw evidence is local under `/tmp/previa-build-audit/`: JSON Go test logs,
coverage profile, browser logs, fuzz output and stage exit records. Docker suites
run sequentially to preserve resource-delta assertions. PostgreSQL fixtures use
isolated schemas in the existing disposable database at schema 46 / River 007.
The final Docker inventory contained only the two pre-existing running services
(database and egress); no running test containers remained. Fixture assertions
also checked image, network and volume cleanup. The development database and user
processes were left untouched. The initial
package-local coverage profile is 63.6%; it excludes opt-in Docker/browser runs
and calls made by other packages, so it is not a whole-pipeline coverage score.
The main pass skips 38 opt-in test/subtest results. Docker and selected browser
suites run separately; live source/reporting/App checks and the usage-settings
browser remain deliberately outside this build-only pass.
Coverage review prompted the additional source-workspace failure test. A syntax
typo while authoring that test was corrected before its successful runs; no
application behavior was changed.

## Reproduction

Export `TEST_DATABASE_URL` for a disposable database; direct Go commands do not
load `.env`. Browser prerequisites and optional `UI_BROWSER_EXECUTABLE` are in
[development setup](../docs/02-development-setup.md#focused-build-stress-and-fuzz-checks).

```sh
go test -race -shuffle=on -count=1 -p=2 -timeout=40m \
  ./internal/environments ./internal/builds ./internal/source ./internal/repoconfig \
  ./internal/runtime ./internal/sampledata ./internal/usage ./internal/pullrequests \
  ./internal/housekeeping ./internal/buildlogs ./internal/appoutput ./internal/webhooks \
  ./internal/secrets ./internal/outbound ./internal/routing ./internal/onboarding \
  ./internal/platform/queue ./internal/platform/workers ./internal/platform/egress
go test -race -shuffle=on -count=5 -run '^TestBurstAdmissions' ./internal/environments
go test -race -shuffle=on -count=5 -run '^TestUnavailableSourceWorkspace' ./internal/source
go test -race -run '^$' -fuzz '^FuzzBuildConfiguration$' -fuzztime=60s -parallel=2 ./internal/repoconfig
go test -race -run '^$' -fuzz '^FuzzArchivePathContainment$' -fuzztime=60s -parallel=2 ./internal/source
UI_TEST_PATTERN='build|Build|rebuild|Rebuild|queue feedback|unavailable repository|Previews show|secrets editor|outbound destinations|logs|App output' make test-browser
make test-setup-browser test-test-data-browser
make test-builder test-runtime test-routing test-housekeeping test-e2e
```

To reproduce real suites with race/shuffle enabled, set the matching `TEST_BUILDER`,
`TEST_RUNTIME`, `TEST_ROUTING`, `TEST_HOUSEKEEPING` or `TEST_E2E` flag and invoke
`go test -race -shuffle=on -count=1` with the package and pattern used by its Make
target. Normal package tests intentionally skip opt-in Docker/browser/live tests;
those are accounted for separately above.

## Boundaries

This is a build-focused verification pass, not a claim to enumerate every possible
program, infrastructure failure or future race interleaving. GitHub faults are
simulated by controlled TLS fixtures; no live PR writes are performed. Traefik uses
local test ACME, not production DNS/Let's Encrypt. Live GitHub source/reporting and
production-host acceptance remain separate; external credentials are not assumed.
The race detector instruments the Go test processes; separately compiled worker
and application fixture subprocesses retain the harness's normal build flags.
Concurrency invariants are also asserted through database and real-resource state.
No unrelated account/profile/support or review-authoring browser suites are run.
