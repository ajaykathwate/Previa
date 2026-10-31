# Previa regression test documents

Feature 1 implementation verification is recorded separately in the
[2026-10-14 shared workspace report](2026-10-14-shared-workspaces-report.md),
Feature 2 (guided onboarding, the repository Setup tab) in the
[2026-10-16 guided onboarding report](2026-10-16-guided-onboarding-report.md), and
Feature 3 (native review and feedback, with deployment records and activity) in the
[2026-10-16 review and feedback report](2026-10-16-review-feedback-report.md), and
Feature 5 (ready-to-test sample data, with the reference journeys) in the
[2026-10-16 test data report](2026-10-16-test-data-report.md), and Feature 5B
(persistent test data) with the F1 corrections in the
[2026-10-17 persistent test data report](2026-10-17-persistent-test-data-report.md),
and F3 (workspace usage and finite free allowances) in the
[2026-10-18 usage report](2026-10-18-usage-and-free-limits-report.md),
and F4 (account exits, support/status and restore-safe deletion) in the
[2026-10-19 account lifecycle report](2026-10-19-accounts-support-and-deletion-report.md),
and F5/F6 (missed installation recovery and workspace approval) in the
[2026-10-19 installation recovery and approval report](2026-10-19-installation-recovery-and-early-access-report.md),
and F7 (Google sign-in, explicit identity linking and email Reviewer invitations)
in the [2026-10-20 Google sign-in report](2026-10-20-google-sign-in-report.md).
It includes team access/offboarding, migration, browser and worker-pipeline checks;
the full product regression cycle below remains the earlier baseline.

The [22 October full regression report](2026-10-22-full-regression-report.md) is
the latest cycle across the whole application: a route × role access matrix, a
crawl proving pages offer only what each person can use, a real-Docker role
journey, a live GitHub pass, six fixed defects, and the owner's changes (only
usable controls, Automatic countdowns, recovery of pushes whose webhook was lost).

The focused [20 October build stress report](2026-10-20-build-stress-report.md)
records the build-only aggressive pass: race/shuffle testing, repeated admission
and quota races, fuzzing, build UI and real Docker/worker failure journeys. It does
not claim a new acceptance pass for unrelated features.

These documents are Previa's regression test specification: **242 test cases**
across 16 modules, from signing in to a preview URL being served, reported and
torn down. Each case says what is tested, how, what must happen, and which
automated test or command proves it. A box is ticked only when that evidence
passed in the recorded test cycle.

**Latest cycle: 2026-10-13 — 240 of 240 executed cases passed; two complete
production acceptance cases remain.** The full Go suite, all 52 browser scenarios,
all real Docker suites, authorized GitHub reporting and all 40 production smoke
checks passed. See the [current regression report](2026-10-13-regression-report.md)
for evidence, corrections and remaining limits. The 2026-10-09 record below is
historical.

## Modules

| # | Module | Prefix | Cases | Passed | Not run |
| --- | --- | --- | ---: | ---: | ---: |
| 01 | [Authentication and sessions](01-authentication-and-sessions.md) | AUTH | 16 | 16 | 0 |
| 02 | [Installations and repositories](02-installations-and-repositories.md) | INST | 13 | 13 | 0 |
| 03 | [Webhooks and PR discovery](03-webhooks-and-pr-discovery.md) | HOOK | 19 | 19 | 0 |
| 04 | [Build requests and scheduling](04-build-requests-and-scheduling.md) | BLD | 21 | 21 | 0 |
| 05 | [Source and configuration](05-source-and-configuration.md) | SRC | 14 | 14 | 0 |
| 06 | [Image builds](06-image-builds.md) | IMG | 8 | 8 | 0 |
| 07 | [Preview runtime and backend services](07-preview-runtime-and-backend-services.md) | RUN | 16 | 16 | 0 |
| 08 | [Routing, HTTPS and publication](08-routing-https-and-publication.md) | RTE | 14 | 14 | 0 |
| 09 | [Teardown, cleanup and housekeeping](09-teardown-cleanup-and-housekeeping.md) | TDN | 14 | 14 | 0 |
| 10 | [PR feedback and reporting](10-pr-feedback-and-reporting.md) | RPT | 11 | 11 | 0 |
| 11 | [Secrets and outbound connections](11-secrets-and-outbound-connections.md) | SEC | 12 | 12 | 0 |
| 12 | [Build logs and live updates](12-build-logs-and-live-updates.md) | LOG | 11 | 11 | 0 |
| 13 | [Frontend and user experience](13-frontend-and-user-experience.md) | UI | 28 | 28 | 0 |
| 14 | [Security and ingress](14-security-and-ingress.md) | SECU | 15 | 15 | 0 |
| 15 | [Operations and release](15-operations-and-release.md) | OPS | 16 | 15 | 1 |
| 16 | [End to end: pull request to preview URL](16-end-to-end-pipeline.md) | E2E | 14 | 13 | 1 |
| | **Total** | | **242** | **240** | **2** |

Modules 01–12 follow the product's features in pipeline order. Module 13 looks at
everything from a person's point of view, module 14 at the edges every request
crosses, module 15 at running and releasing Previa, and module 16 walks the whole
journey from a pull request to its preview URL.

## How the documents are organized

Each module document has the same shape:

- A header naming the module, its prefix, the code it covers and its feature docs.
- A table of its **sub-modules** (test suites in the usual terminology).
- One section per sub-module listing its **test cases** in this form:

```text
- [x] **TC-AUTH-SIGN-001 · Signing in creates a fresh, rotated session**
  Type: Positive · Priority: P1 · Level: Integration
  Steps: what is done (the test data is in the steps)
  Expected: what must happen
  Evidence: `internal/auth` › `TestLoginRotationProtectedRouteAndLogout`
```

| Field | Meaning |
| --- | --- |
| ID | `TC-<MODULE>-<SUB-MODULE>-<NNN>`. IDs are stable: new cases take the next number; a removed case's ID is not reused. |
| Type | Positive (the intended path), Negative (invalid or refused input), Edge (boundaries, races between events, odd states), Security, Resilience (failures and recovery), Concurrency, Usability, Accessibility, Performance. A case may have two. |
| Priority | P1 = security, data integrity or the core PR-to-preview journey; P2 = important behaviour; P3 = minor. |
| Level | Unit · Integration (PostgreSQL) · Integration (assembled server: every route and middleware exactly as `RunServer` builds them) · Browser (Chromium under the production CSP) · System (real Docker, BuildKit, Traefik) · End-to-end (the worker's own pipeline on real Docker with a fake GitHub over TLS) · Static · Acceptance (real infrastructure). |
| Evidence | The automated test(s) or command(s) whose pass is the case's result. `pkg › TestName/subtest` is a Go test; `browser › *title*` is a scenario in [`web/tests/browser/ui.test.cjs`](../web/tests/browser/ui.test.cjs); a command is a `make` target or tool. |
| Box | `[x]` = executed in the latest cycle and every piece of evidence passed. `[ ]` = not executed; the case says why. |

## Test levels and how to run them

The commands below are the whole regression. Set up the machine with
[02 · Development setup](../docs/02-development-setup.md) first. Database tests use
`TEST_DATABASE_URL`, which should point at a disposable database migrated with
`make migrate-up` (Goose 46). The tests create isolated schemas; they do not erase
the development database. Direct Go commands require exported test settings;
Make's integration targets load `.env` as documented in development setup.

| Level | Command | What it needs |
| --- | --- | --- |
| Static | `go build ./...`, `make lint`, `make vuln`, `make observability-check`, actionlint on `.github/workflows` | Go; Docker for promtool and actionlint |
| Unit and integration | `TEST_DATABASE_URL=… go test -race -count=1 ./...` | PostgreSQL 17 |
| Browser | `UI_BROWSER_EXECUTABLE=/path/to/chromium make test-browser` (`UI_TEST_PATTERN=<regex>` reruns matching scenarios only) | Node.js 20+, Playwright 1.58.2 (`make browser-install`) |
| System | `make test-builder`, `make test-runtime`, `make test-routing`, `make test-housekeeping` | Docker with rootless BuildKit support, PostgreSQL |
| End-to-end | `make test-e2e` | As System; about 6 minutes |
| Release and smoke | `make release-images PREVIA_VERSION=x` then `make prod-smoke PREVIA_VERSION=x` | Docker, `age` |
| Acceptance (live) | `make test-source`, `make test-reporting`, a deployed host | `TEST_GITHUB_*` for an authorized disposable PR, a real host and domain |

To record a new cycle: run everything above, keep the logs, tick only the cases
whose evidence passed, update the module table and the cycle section below, and
list any defect with the case that now guards it.

## Historical cycle: 2026-10-09

### Environment

| Item | Version |
| --- | --- |
| Commit under test | `2e9cf53` plus the cycle's fixes and new tests (uncommitted at the time of the run) |
| Host | Windows with WSL2 (kernel 6.18.33.1), Docker Desktop, Docker Engine 29.8.1 |
| Go | 1.26.9 (raised from 1.26.8 during the cycle, see D-03) |
| PostgreSQL | 17.11 (Compose), a disposable database migrated to Goose 20 and River 007 |
| Browser | Chromium 154 (snap) driven by Playwright 1.58.2 on Node.js 26.6 |

### Results by suite

| Suite | Command | Result |
| --- | --- | --- |
| Build | `go build ./...` | Pass |
| Static analysis | `make lint` | Pass, 0 issues |
| Known vulnerabilities | `make vuln` | Pass after D-03, 0 affecting the code |
| Alert rules and dashboards | `make observability-check` | Pass |
| Workflows | actionlint 1.7.7 | Pass, no findings |
| Unit and integration | `go test -race -count=1 -v ./...` | Pass, 35 packages, no data races |
| Browser | `make test-browser` | Pass, 43 of 43 scenarios |
| Image builds | `make test-builder` | Pass |
| Preview runtime | `make test-runtime` | Pass |
| Routing and HTTPS | `make test-routing` | Pass |
| Housekeeping | `make test-housekeeping` | Pass |
| End-to-end | `make test-e2e` | Pass, both journeys |
| Release images | `make release-images` | Pass, 3 images (the first attempt failed on a stalled download from GitHub, see Observations) |
| Production smoke | `make prod-smoke` | Pass, 40 checks after D-04 (the first run stopped at check 17) |

### Added in this cycle

The regression looked for gaps before running and added tests to close them:

- **The assembled server** (`internal/app` › `TestServerRoutesRegression`, 14
  scenarios). Until now the server's routes and ingress controls were tested piece
  by piece; this drives them exactly as `RunServer` builds them against a migrated
  database: security headers and nonces, the session cookie, foreign hosts, sign-in
  redirects, CSRF and cross-site refusals, body limits, the sign-in rate limit,
  webhook signatures and envelopes, delivery deduplication including 20 concurrent
  copies, static and operational exposure, and a burst of 200 concurrent requests.
- **Request resilience in the browser** (six scenarios): slow searches, a double
  click under a slow response, refused, failed and offline actions, a request that
  never answers, a forged CSRF token, and a throttled "slow 3G" network with a
  page-weight budget.
- **Accessibility** (one scenario auditing 13 pages).
- **Hostile data**: SQL and LIKE syntax in searches against PostgreSQL
  (`TestHostileSearchAndTitlesAreInertData`) and script-bearing GitHub data through
  the real templates (`TestGitHubDataRendersAsText`).
- A browser harness option, `UI_TEST_PATTERN`, to rerun selected scenarios.

### Defects found in this cycle

| ID | Severity | Found by | Defect | Fix | Guarded by |
| --- | --- | --- | --- | --- | --- |
| D-01 | Low (usability) | TC-AUTH-ACC-003 | An in-page action refused for a failed CSRF check (or a session error) showed "Could not update this view. Check your connection or sign in again" instead of its real reason, because the refusal came back as an HTML error page. | Refusals of in-page (htmx) requests are now plain text, which the page shows as is; page requests still get the error page. (`internal/auth/handler.go`) | TC-AUTH-ACC-003 |
| D-02 | Low (defence in depth) | TC-UI-SAFE-001 | The "View on GitHub" link rendered the pull request's stored address without checking its scheme, so a `javascript:` address would have become a clickable script link. The address comes from signed GitHub payloads, so exploiting it needed a compromised GitHub or webhook secret. | The link is rendered only for `https://` addresses, the rule commit links already used. (`web/pages/pullrequests`) | TC-UI-SAFE-001 |
| D-03 | High (security, release-blocking) | TC-SECU-DEP-001 | govulncheck reported 13 reachable vulnerabilities published since the previous run, in Go 1.26.8's `net/http`, `crypto/tls`, `html/template`, `os` and `net/textproto` and in `golang.org/x/net` v0.58.0 (GO-2026-6599 to GO-2026-6617, including an HTTP/2 server crash). CI's vulnerability gate would have failed. | Go 1.26.9 (`go.mod`, `tools/go.mod`, the digest-pinned `golang:1.26.9-bookworm` builder image) and `golang.org/x/net` v0.60.0; every suite was rerun on the new toolchain. | TC-SECU-DEP-001 |
| D-04 | Medium (CI-blocking) | TC-OPS-REL-004 | Introduced by the D-03 fix: the production smoke test expected `previa_build_info{go_version="go1.26.8"}` as a literal, so with images built on Go 1.26.9 its "build info and state gauges exported" check failed (16 checks had passed). CI runs this smoke test, so the next push would have failed. | The smoke test reads the Go version from the Dockerfile's pinned `GO_IMAGE`, so a toolchain bump updates both together; it stops with a clear message if the pin cannot be read. (`deploy/production/smoke.sh`) | TC-OPS-REL-004 |

### Not run in this cycle

| Case | Why |
| --- | --- |
| TC-RPT-LIVE-001 | Writing reports to a real pull request needs a GitHub App installation and `TEST_GITHUB_*` for an authorized disposable PR. |
| TC-E2E-LIVE-001 | The journey against github.com, a real domain and Let's Encrypt needs those credentials and a deployed host. |
| TC-OPS-HOST-001 | Setup, backups to object storage, restore and upgrade on a real host need that host. |

These are the pending live checks of [26 · Release report](../docs/26-release-report.md);
everything else in the release checklist is covered above.

### Observations (not defects)

- A sign-in start that hits the rate limit shows a plain-text "Too many requests.
  Try again shortly." page with `Retry-After`; it is correct but unstyled.
- The first `make release-images` attempt failed after 27 minutes: the Tailwind
  CSS download from GitHub inside the image build stalled and then broke
  (`curl: (18) HTTP/2 stream … not closed cleanly`). The rerun downloaded it in
  under a minute. `deploy/install-tailwind.sh` has no retry or time limit, so a
  flaky network can hang a release or CI build instead of retrying or failing fast.
