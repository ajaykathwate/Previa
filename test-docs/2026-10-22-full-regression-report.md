# Full regression · roles, access and the build-to-preview flow

Dates: 21–22 October 2026. Branch `mvp-complete`, base `0195b73`. The owner asked
for a fresh regression of the whole application, step by step from sign-in, with
the most weight on the build → preview flow and on roles and access. Decisions
taken with the owner before and during the cycle: simulated **and** live GitHub;
defects are fixed with a regression test; missing coverage is added as tests.

**Result: every suite passes after the fixes below.** The cycle found and fixed
six defects (one release-blocking security update, one production data race),
added three cross-cutting regression tests and, at the owner's request during the
live pass, made two product changes: pages show only what a person can use, and
Automatic builds count down and recover pushes whose webhook was lost.

## What was run

| Suite | Command | Result |
| --- | --- | --- |
| Build, vet, lint | `make build`, `make lint` | Pass, 0 issues |
| Known vulnerabilities | `make vuln` | Pass after D-01: 0 reachable (the known unreachable `openpgp` advisory remains) |
| Unit and integration, race + shuffle | `go test -race -shuffle=on -count=1 -p=3 ./...` | Pass: 53 packages, 701 top-level tests and 1,181 subtests, 0 data races (opt-in Docker/browser/live tests run separately) |
| Role and access matrix (new) | `TestEveryRouteRespectsEveryRole` | Pass: 186 route/target rows × 8 people, database fingerprint per person, sidebar per person |
| Only usable controls (new) | `TestPagesOfferOnlyWhatPeopleCanUse` | Pass: every page each of 8 people can open; 0 disabled controls, refused forms or bouncing links |
| Browser (Chromium) | the eight `make test-*browser` targets | Pass: workspace 14, setup 5, review 6, test data 3, usage 4, accounts 7, Google 3; main suite 54/55 per full run, the one failure always a known timing-sensitive repository-page scenario (see Observations), each passing alone |
| Real Docker: images, runtime, routing, housekeeping | `make test-builder test-runtime test-routing test-housekeeping` | Pass: 6 + 8 subcases, 11 + 5 subcases, 8, 2 |
| Real Docker: worker journeys | `make test-e2e` (now five journeys) | Pass: standard, backend, reference, test data and role journeys (552 s); usage reconciles in each |
| Release images and production smoke | `make release-images` / `make prod-smoke` | Pass: three images built from `golang:1.26.10-bookworm@sha256:fe3da92a…`; 33 smoke checks including build info and rollback (backup drill skipped: `age` not installed) |
| Live GitHub pass | see below | Pass, with the limits listed |

## New tests

- **`internal/app/access_matrix_test.go` · `TestEveryRouteRespectsEveryRole`.** The
  server exactly as `RunServer` assembles it (a fake GitHub injected as its App),
  every route the router serves, as eight people: signed out, Owner, Admin, a
  granted Developer, a granted Developer GitHub no longer lets read the code, a
  granted Reviewer, a removed member and another workspace's owner; repository
  routes for a granted and an ungranted repository, with valid forms. Each answer
  must match `accessMatrix`; afterwards the database must match the person's
  fingerprint (refused requests change nothing) and the sidebar the person's
  navigation. A route added without classification fails the test.
- **Same file · `TestPagesOfferOnlyWhatPeopleCanUse`.** Crawls every page each
  person can open and fails on a disabled control, a form the server refuses
  them, or a link that is refused or bounces them.
- **`internal/app/e2e_roles_test.go` · `TestRealRoleJourney`** (in `make test-e2e`).
  The core flow through the server's own HTTP routes while the worker builds on
  real Docker, BuildKit and Traefik: sign-in redirect; a signed `pull_request`
  webhook opens the PR (Manual builds nothing); Reviewer, Developer without
  GitHub access, removed member and outsider are refused Build; the granted
  Developer builds; the PR report says Ready; logs for the Developer, not the
  Reviewer; the Reviewer opens the preview and records a finding on Deployment 1;
  a pushed commit is rebuilt at the same address as Deployment 2; the Reviewer
  resolves the finding there; the Owner tears down (no container, image or network
  left); closing the PR keeps the review readable and refuses builds.
- Countdown, quiet-period headline, missed-push scheduling and concurrency tests
  listed with their defects and changes below.

## Defects found and fixed

| ID | Severity | Found by | Defect | Fix | Guarded by |
| --- | --- | --- | --- | --- | --- |
| D-01 | High (security, release-blocking) | `make vuln` | GO-2026-6809: Go 1.26.9's `crypto/x509` trusted compromised intermediate certificates; reachable from Previa's TLS clients. CI's vulnerability gate would fail. | Go 1.26.10 in `go.mod`, `tools/go.mod` and the digest-pinned `golang:1.26.10-bookworm` builder image. | `make vuln`; release images and smoke |
| D-02 | High (production stability) | Race detector on the parallel access matrix | Every page render merges button classes through `tailwind-merge-go` v0.2.0, whose shared cache was unsynchronized: concurrent requests corrupted its list (a nil-pointer panic or hang under load). | v0.2.3 (upstream fixes v0.2.2/v0.2.3). The 21 class lists the app really merges were compared under both versions: identical class sets. | `web/components/utils` · `TestTwMergeIsSafeForConcurrentRenders` (46 race reports on v0.2.0) |
| D-03 | Low | Access matrix | `POST /workspaces/{id}/delete` answered **503** "temporarily unavailable" (and logged an error) for a non-owner, a wrong confirmation or a stale revision: account-lifecycle errors were not translated. | Composition-root adapter maps them to the workspace feature's 404/409. | Access matrix row |
| D-04 | Medium (UX) | Live pass | A pending (early-access) workspace's Owner was offered **Install on GitHub**; clicking it showed a bare plain-text 403 page. | Pages and plain forms go to the Early access status page; in-page requests keep the short reason; Repositories offers no button while pending. | `TestPendingWorkspaceRefusalsExplainThemselves`; accounts browser |
| D-05 | Low (UX) | Live pass | Reviewers saw **Early access** in the sidebar; its page told them they could connect repositories and linked to Repositories, which bounces Reviewers. | Hidden from Reviewers; `/early-access` sends them to Previews. | Matrix navigation; usability crawl |
| D-06 | Medium (product) | Live pass | In Automatic mode a new commit whose webhook was lost (GitHub does not retry) was recorded by housekeeping or Sync but never built. | Approved design change (below). | `TestConfirmedNewCommitIsScheduledLikeAPush` |

## Product changes requested during the live pass

- **Pages show only what a person can use** (`docs/21` *Only what a person can
  use*). Role-based "no" hides navigation, buttons and links; a temporary "not
  now" is a short status with the reason and the fix; the only disabled control
  left is a busy button while its request is sent, and a destructive dialog's
  confirm until its confirmation text is typed. Changed: PR page (blocked or
  running builds, Sync while syncing, Automatic and private-preview switches),
  Repositories (pending, not configured, pagination ends, empty states), Test data
  for Developers, Setup steps, Outbound and Secrets forms, the test-access
  password without sealing, Starter-kit services the server cannot run, "Open your
  dashboard" links (land on Previews for a Reviewer, the workspace chooser without
  a workspace) and Developers whom GitHub no longer lets read the code (no build,
  sync, teardown or logs). Reviewers no longer see **View on GitHub**.
- **Automatic builds count down** (`docs/16`). PR page: *New commit `b8d09e7`
  builds in 4:32*, that another push restarts the wait, that the preview keeps
  serving meanwhile, **Build now**; PR list, Builds and Overview count down too; a
  push during the wait restarts the countdown live; never below 0:00; `role="timer"`.
  Guarded by `TestAutomaticBuildCountsDownAndRestartsOnNewCommits` (two real signed
  webhooks), `TestHeadlineCountsDownTheQuietPeriod`, `TestTimeLeftCountsDownAndStopsAtZero`.
- **Missed pushes are recovered** (`docs/05`, `docs/15`, `docs/16`). Housekeeping's
  15-minute confirmation (`ConfirmUpstream`) and Sync Pull Requests
  (`SyncArgs.Confirm`) treat a new commit on a tracked open PR as the lost push:
  Automatic schedules it after the quiet period; never for forks; Manual only
  records it. Connection discovery and the check before a manual build never start
  work, so connecting a repository or enabling Automatic still builds nothing.

## Live GitHub pass

Real GitHub OAuth and App, `ajaykathwate/previa-live-test`, a fresh database
(`previa_live`), the local Traefik router with the trusted `*.dev.previa.live`
certificate, webhooks through a temporary Cloudflare tunnel, and a second GitHub
account. Verified live: GitHub sign-in; workspace creation pending early access;
operator approval at `/operator/early-access`; App installation and repository
connection; PR discovery; Owner build to a trusted-HTTPS preview; PR comment and
check run; Automatic policy; Reviewer invitation by GitHub username, acceptance
in a private window, Reviewer sidebar and refusals, a finding on Deployment 1;
a stale redelivered `opened` event ignored without regressing the PR; a commit
whose webhook was lost found by housekeeping, counted down, built automatically
and published as Deployment 2 at the same URL (content identical to GitHub's
`3303454`), with all 13 report versions delivered.

Not done live (covered by `TestRealRoleJourney` on real Docker): resolving on
Deployment 2, teardown through the UI and closing the PR. Google sign-in is not
configured on this machine, so it was verified only by its simulated suites. The
test preview was removed locally at the end; its PR comment still reads Ready.

## Observations (not product defects)

- **Temporary tunnels drop.** The quick tunnel died at 12:38 when this machine
  briefly lost network (the same minute an installation reconciliation failed
  closed on "App identity could not be verified") and later dropped idle QUIC
  connections; GitHub logged `502 failed to connect to host` for those deliveries.
  Production webhooks go to the real host; D-06 covers what GitHub does not retry.
- **Test-runner capacity.** Running every package's database tests at once
  (`-p` = CPUs) exhausted PostgreSQL's lock table (`SQLSTATE 53200`); `-p=3`, as
  documented, passes. Eight schemas orphaned by an interrupted run were dropped.
- **A machine restart interrupted the routing suite**; its fixtures were removed by
  label before rerunning (now documented in development setup).
- **Starter-kit browser test race.** It typed a new port while the previous
  refresh was still in flight; it now waits for that refresh, like its other steps.
- **Timing-sensitive repository-page browser scenarios.** *Slow searches show
  progress…*, *A request that never answers…* and (until hardened here) *Dedicated
  repository and PR pages preserve filters…* each fail now and then when run after
  other scenarios, and pass alone (3/3, 5/5). An A/B over 15 runs found the same
  flakiness with and without the new countdown script (2 and 4 failures), so it
  predates this cycle: an earlier scenario's live refresh or request can collide
  with their precise request timing. The relative-time check now waits for the
  text; the other two should be hardened the same way (recommended follow-up).
- `internal/installations/service.go` is not `gofmt`-formatted at the base commit
  (unchanged here; the linter does not flag it).

## Deviations from README/DESIGN

- The discovery rule "never creates automatic build intent" is narrowed (D-06),
  with the owner's approval; `docs/05`, `docs/15` and `docs/16` say so.
- The previous UI rule "show a blocked control disabled with its reason" is
  replaced by the owner's "only what a person can use" (`docs/21`, `docs/29`, README).
- `internal/app/server.go` gained a test seam: `newServerFeaturesWithApp` accepts
  the GitHub App, so the assembled server can run against a fake GitHub. Production
  wiring is unchanged.

## Reproduction

Export a disposable `TEST_DATABASE_URL` (Goose 46, River 007); see
[development setup](../docs/02-development-setup.md#role-and-access-matrix).

```sh
go test -race -shuffle=on -count=1 -p=3 -timeout=60m ./...
go test -race -count=1 -run '^(TestEveryRouteRespectsEveryRole|TestPagesOfferOnlyWhatPeopleCanUse)$' ./internal/app
make test-browser test-workspace-browser test-setup-browser test-review-browser \
  test-test-data-browser test-usage-browser test-accounts-browser test-google-browser
make test-builder test-runtime test-routing test-housekeeping test-e2e   # sequentially
make lint vuln
```
