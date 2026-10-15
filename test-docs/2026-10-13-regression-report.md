# Regression cycle — 2026-10-13

The existing fixes were committed first as `8764e0c` on `ui-improvements`.
This cycle then exercised the full application test suite, browser scenarios,
real Docker execution, authorized GitHub feedback, and the production stack.
The follow-up changes make regression tests safe alongside a running development
worker and update stale verification assumptions. **240 of 242 documented cases
are verified; two production acceptance cases remain incomplete.**

## Results

| Check | Result | Local evidence |
| --- | --- | --- |
| Full generated build, then final `go build ./...` | Passed | `/tmp/previa-regression-build.log`, `/tmp/previa-regression-build-final.log` |
| Full Go suite: race detector, shuffled order, uncached, PostgreSQL integration | 1,428 test/subtest passes; 81 packages considered; no failures or races | `/tmp/previa-regression-final-race.json` |
| Chromium browser suite | All 52 scenarios passed | `/tmp/previa-regression-browser.log` |
| Selected concurrency and recent regression tests, ten repetitions | 200 passes across seven packages | `/tmp/previa-regression-repeat.json` |
| Legacy-worker migration 0029, rollback/reapply and capability tests, ten repetitions | 50 passes | `/tmp/previa-regression-migration.json` |
| Atomic runtime capacity admission, ten repetitions | All ten passed | `/tmp/previa-regression-capacity.json` |
| Real Docker builders, runtime, routing, housekeeping and both end-to-end journeys | All five packages passed, 39 test/subtest passes, no failures or races | `/tmp/previa-regression-real-final.json` |
| Actual GitHub App installation access and exact-commit source preparation | Both passed on `ajaykathwate/previa.live` PR #4 | `/tmp/previa-regression-github.json` |
| Authorized real GitHub reporting | Passed; proof comments removed and original preview comment preserved | `/tmp/previa-regression-reporting-live.json` |
| Final `go vet`, `golangci-lint` | Passed; lint reports zero issues | `/tmp/previa-regression-vet-final.log`, `/tmp/previa-regression-lint-final.log` |
| Go vulnerability check | No reachable or imported-package vulnerabilities; one required-module advisory is unreachable | `/tmp/previa-regression-vuln.log` |
| CI image vulnerability gate, all three release images | Passed; no fixable HIGH/CRITICAL findings | `/tmp/previa-regression-trivy-{server,worker,egress}.log` |
| Prometheus rules, configuration and dashboard checks; actionlint | Passed | `/tmp/previa-regression-observability.log`, `/tmp/previa-regression-workflows.log` |
| Fresh server, worker and egress release images | All built successfully | `/tmp/previa-regression-release-final.log` |
| Production Compose smoke, including encrypted backup and restore | All 40 checks passed | `/tmp/previa-regression-smoke-final.log` |

The baseline Go command intentionally skips 39 opt-in test/subtest events.
The browser, Docker, source, App-access and reporting runs exercise these
separately; a baseline green result alone is not evidence for them. Packages with
no tests are reported as such rather than counted as tested features. `/tmp`
logs are local run artifacts and are not included in the repository.

## Failures found and corrections

1. **End-to-end forwarding shared development policy.** A test worker on an
   isolated database still used the global `previa-egress` gateway. It overwrote
   the development worker's ports and approvals, and the development worker's
   periodic refresh erased the test's SSH approval. Each end-to-end test now
   receives a named gateway, uses that identity for probes, forwarding, metrics
   and cleanup, and removes its own container and outward network. The normal
   worker still uses the shared gateway. Endpoint-selection tests prevent a named
   gateway from selecting or detaching the shared proxy.
2. **Capacity test assumed an empty Docker host.** Global admission correctly
   counted an existing preview, so the test left room for zero candidates. Its
   fixture now adds existing reservations to its budget before racing two
   requests, preserving the assertion that exactly one starts. The production
   capacity policy is unchanged. The corrected test passed ten consecutive runs.
3. **Production smoke expected old schema and alert counts.** It expected
   migration 20 and 24 alerts. It now reads the required schema version and alert
   count from the checkout (currently 29 and 27), including upgrade/restore
   assertions. Internal-listener exposure is checked on the smoke containers,
   allowing development listeners on 9090/9091 to stay running.
4. **The GitHub fake handled comments for only the base PR.** It now records the
   owning PR for each comment and separates base and fork listing, editing and
   deletion. A contract test and the backend journey verify independent fork
   feedback.
5. **Live reporting required removal of real preview feedback.** The proof now
   uses a unique test marker and refuses to edit comments outside that scope.
   Its rehearsal starts with an existing preview comment and proves preservation
   through recovery and cleanup. The owner-authorized real run also preserved
   the existing comment. GitHub retains three completed checks named
   `Previa preview (live proof)`; the test does not merge or close the PR.

The first failed runs are retained in `/tmp/previa-regression-real.json` and
`/tmp/previa-regression-smoke{,-green}.log`. Intermediate compilation and lint
findings in new fixtures were corrected before the final checks.
The final Docker run completed both end-to-end journeys: approved outbound SSH
worked, fork connections were refused, a failed setup preserved the earlier
database, and teardown removed every test artifact. No test gateway containers
or networks and no production smoke containers remained afterwards.

## Product behavior covered

The Go and browser suites cover authentication and return URLs, installation and
repository access, manual/automatic build controls, webhook validation and
deduplication, configuration and archive safety, build-secret and fork gates,
timeouts and quotas, preview publication and private access, build logs and App
Output, live updates, backend services, approved outbound access, report recovery,
teardown and reconciliation, idle sleep/wake, retention, and owner actions.

Real Docker checks exercise BuildKit isolation and limits, secret mounts, fork
cache separation, hardened runtime behavior and MySQL, HTTPS and private auth,
route fencing, router/network restoration, close-during-publication and cleanup.
The end-to-end journeys exercise successful and failed rebuilds, automatic
coalescing, a reporting outage, PostgreSQL/Redis setup and processes, fresh
databases, fork isolation, outbound refusal, and full teardown.

The production smoke runs real release binaries and migrations behind Traefik,
validates ingress and internal exposure, readiness and metrics, all configured
alerts and Grafana provisioning, hardening, restarts, graceful worker shutdown,
tag upgrade/rollback, encrypted backup, restoration and paused builds after
restore. The optional older-dump restore branch was not run; migration 0029's
legacy-row upgrade and rollback/reapply were exercised separately.

## Live state and remaining limits

During the cycle, the development worker remained ready with execution,
routing, reporting, housekeeping, discovery and private-preview capabilities.
The webhook tunnel reported a ready connection. PR #4's public preview returned
HTTPS 200. The merged PR #3's URL returned HTTP 302 to the unavailable page.

At the final check after the session interruption, Cloudflare had expired the
temporary webhook tunnel (`Unauthorized: Tunnel not found`, no ready connections).
The expired process was stopped. Automatic approval review initially rejected
creating a replacement public tunnel pending explicit authorization. The owner
then approved restarting it and updating the existing development App webhook.
The replacement `https://stores-sandy-pendant-smile.trycloudflare.com` tunnel
reported a ready connection, its signed ping returned HTTP 204, and the updated
App webhook URL was verified by reading it back. Only the URL was patched;
the webhook secret was preserved. Worker readiness and both preview URL checks
still passed. Quick tunnels remain temporary and need a webhook URL update when
replaced. Earlier GitHub reporting proofs remain valid evidence for their runs.
The recovery follow-up also passed `go build ./...` and the webhook package's
tests with the race detector.

Two documented acceptance cases remain incomplete: the complete disposable-PR
journey on a deployed production host (`TC-E2E-LIVE-001`) and real host setup,
object-storage backup, restore and upgrade (`TC-OPS-HOST-001`). Actual App access,
source preparation and feedback passed, but do not establish those full hosted
journeys. A real authenticated private-registry pull was not performed; the
registry credential gates and redaction are covered by automated tests.

These results establish the behavior exercised in this environment. They do not
guarantee every possible repository, external outage, browser or host condition.
No new production configuration or migration is introduced by this follow-up;
development setup and test instructions were updated with the verification
changes. No architecture or UI-theme deviation was made.
