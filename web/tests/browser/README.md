# Optional browser regression checks

This is development tooling, not an application dependency or deployed service.
Playwright is locked to **1.58.2**; use Node.js **20+**.

From the repository root:

```sh
make browser-install
make test-browser
```

The first command installs locked npm dependencies and Chromium. The second
generates assets/templates and runs browser scenarios against a temporary
Go HTTP server, which closes automatically. Expected: all scenarios pass, no JavaScript
errors, and a successful Go test. Ordinary `make test` skips this opt-in suite.

The fixture uses real routes, templates, session middleware, OAuth state handling,
CSRF and logout with in-memory sessions and fake GitHub/user/queue providers.
It does not load `.env`, contact GitHub, or need Postgres. These tests do not replace
real OAuth/database integration checks. Fixture code is in `_test.go` only.

Coverage includes public/protected access; responsive and sticky navigation;
mobile section links; modal scroll preservation, focus trapping/restoration and
dismissals; logout and session invalidation; CSRF rejection; queue pending,
duplicate and error feedback; reveal/offscreen behavior; reduced motion; and
native confirmation/content without JavaScript in Chromium; and the configured
installation list with long repository names at mobile/desktop sizes. The latter
uses a test-only authenticated presentation path, never a production route.
Repository/PR cases also verify dedicated-page navigation, filter-preserving
breadcrumbs/history, aligned desktop controls, legacy bookmarks, failed searches,
Automatic confirmation/cancel/retry and in-place sync on list/detail pages.
Signed-out PR Build logs links return to that PR after OAuth. External automatic
build changes disable queued/running controls through SSE, with deferred updates
resuming after a confirmation closes.
Command palette keyboard access, filtering, navigation and logout hand-off, and
the persisted light/dark theme toggle are also covered, as are live updates without
polling, the repository Secrets tab (write-only saving, replacing and modal
deletion over the real secrets handler, keyring and CSRF with an in-memory store),
and build logs over the real log handler, live stream and hub with an in-memory
store: live append, following, reset for a new attempt, script-looking output
rendered as text, and paging without JavaScript. Test-only `/__fixture/logs/*`
routes change that store and publish like the database trigger. PR actions run
against fixture build and teardown endpoints: one request per click with no second
Build while queued, teardown confirmed in a modal on the PR page and per row on
Environments (with and without JavaScript, focus returning to the trigger), no
actions for an unavailable repository, and Builds/Environments showing addresses,
waits and failures. The real handlers' authorization, CSRF, idempotency and
revision checks are covered by `internal/pullrequests` tests against PostgreSQL.

Resilience scenarios intercept requests in the browser to show what a person sees
when things go wrong: slow searches (progress, the newest result wins), a double
click under a slow response (one request), refused, failed and offline actions
(an explanation, nothing changed, the control usable again), a request that never
answers (the 10-second timeout and a retry), a forged CSRF token, and a "slow 3G"
network through Chrome DevTools emulation with a 512 KiB page-weight budget. An
accessibility audit checks 13 pages for a language, title, single `h1`, `main`
landmark, named controls, labelled fields, image `alt` text and unique IDs. The
cases and their place in the full regression are in [test-docs/](../../../test-docs/README.md).

To capture ignored screenshots:

```sh
UI_BROWSER_SCREENSHOTS=1 make test-browser
```

Files appear in `web/tests/browser/artifacts/`. These are inspection artifacts,
not committed snapshot baselines. `UI_BROWSER_EXECUTABLE=/absolute/path/to/chromium`
optionally selects an existing compatible browser; the default uses Playwright's
installed Chromium. `UI_TEST_PATTERN='<regex>'` runs only the scenarios whose
titles match. None of these variables is application configuration.

If Chromium reports missing Linux system libraries, install its OS dependencies
(this may request administrator privileges):

```sh
npm exec --prefix web/tests/browser -- playwright install-deps chromium
```

If the browser executable is missing, rerun `make browser-install`. Both Air
configurations exclude this directory so dependencies/artifacts do not trigger
application reloads. Installed packages and screenshots are Git-ignored.


Shared-team flows also have a database-backed suite:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-workspace-browser
```

`internal/workspaces/browser_test.go` creates a temporary schema and serves real
session/CSRF/workspace handlers with fixture GitHub responses. Four scenarios in
`workspaces.test.cjs` cover layouts, access confirmation, workspace creation and
switching, JavaScript-free invitation consent, and ownership handover. Screenshots
use the existing `UI_BROWSER_SCREENSHOTS=1` setting and ignored artifacts folder.

The workspace suite also checks direct Members/Settings navigation, username
search, separate invitations, switcher alignment, profile edits, PNG upload and
removal, stale forms, narrow layouts, and no-JavaScript saves against PostgreSQL.
Its fixture timeout is five minutes. `UI_BROWSER_SCREENSHOTS=1` includes settings,
logo, invitation and switcher screenshots in the ignored artifacts directory.

The repository Setup tab has its own PostgreSQL-backed suite:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-setup-browser
```

`internal/onboarding/browser_test.go` migrates a temporary schema and serves the
real session, CSRF, public-ID, workspace, pull request and setup handlers, with live
updates from PostgreSQL `LISTEN`/`NOTIFY` through the realtime hub. Test-only routes
stand in for the worker's GitHub reads, a pushed commit, a build and a deployment.
Five scenarios in `onboarding.test.cjs` cover the Overview's role-aware next steps,
an owner's whole path (check, problem with its fix, push, recheck, build, publish,
open in a new tab, live completion), the pull request picker as a menu of links
(Escape, closing before navigation, a phone-width menu), the Developer and Reviewer
views, and checking without JavaScript. `UI_BROWSER_SCREENSHOTS=1` writes
`setup-*.png` captures in light, dark and phone layouts.

Pull request review pages have their own PostgreSQL-backed suite too:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-review-browser
```

`internal/reviews/browser_test.go` migrates a temporary schema and serves the real
session, CSRF, public-ID, workspace, pull request and review handlers with live
updates through `LISTEN`/`NOTIFY`. Test-only routes stand in for a worker
publishing the next deployment (after a pushed commit), a teammate replying and
another change waking the page. Deployment 1 ran a setup command with a database,
and the review service seals test access with a test keyring.
Six scenarios in `reviews.test.cjs` cover:
- a Developer writing instructions in place (Escape cancels);
- an Owner seeing the setup facts, opening the Reset test data confirmation in place (Escape closes it) and adding test access in place; a Reviewer revealing the password, which a live refresh keeps and a reload drops, with *Last revealed by* shown;
- a Reviewer's whole conversation: a finding with its page and details, the finding header opening its own page, a Ctrl/⌘+Enter reply, an in-place edit, removal in the shared modal, resolving and reopening;
- a live reply and a new deployment arriving while a draft is unsent (the draft stays; the version is chosen on purpose);
- a Developer moderating with a required reason from the pull request page's Review tab;
- the whole flow without JavaScript, a test-access reveal included (run with reduced motion, since the site's smooth scrolling never lets the test robot's own scrolling settle without scripts).

Every scenario fails on any CSP violation. `UI_BROWSER_SCREENSHOTS=1` writes `review-*.png` captures (light, dark and phone, grown to the full content height).

A repository's Test data tab has its own PostgreSQL-backed suite:

```sh
set -a; . ./.env; set +a
export TEST_DATABASE_URL="$DATABASE_URL"
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-test-data-browser
```

`internal/sampledata/browser_test.go` migrates a temporary schema and serves the
real session, CSRF, public-ID, workspace, pull request (its event stream) and Test
data handlers, with live updates through `LISTEN`/`NOTIFY`. A test-only route
stands in for the worker: the waiting refresh starts, then succeeds with a
PostgreSQL 17 baseline and a short log. Three scenarios in `testdata.test.cjs`
cover an Owner turning Test data on in a modal (Cancel first), following the
refresh live to *Ready*, saving a switch and the baseline branch in place (a bad
name explained beside the field, a live refresh never replacing the field while
it is being typed in), confirming Refresh baseline and reading the refresh's build
log; the Developer's read-only view and the Reviewer sent to Previews; and turning
Test data off in a modal. Every scenario fails on any CSP violation.
`UI_BROWSER_SCREENSHOTS=1` writes `test-data-*.png` captures at 1440 and 390 px in
light and dark, checking that a phone never scrolls sideways.

Account exits, support, status and policies use another PostgreSQL-backed suite:

```sh
export TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB'
UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-accounts-browser
```

`internal/accounts/browser_test.go` creates an isolated schema and serves real
auth, session/CSRF, workspace, account and support handlers with fixture GitHub
authentication. Five scenarios cover persisted/escaped support messages, operator
incidents, public status/policies, in-place typed disconnect confirmation, durable
cleanup progress and ending another browser's session. Mutations also run with
JavaScript disabled; CSP violations fail the suite. `UI_BROWSER_SCREENSHOTS=1`
records account, support, operator, policy, progress and modal views at 1440/390
in both themes. No live GitHub mutations occur.
