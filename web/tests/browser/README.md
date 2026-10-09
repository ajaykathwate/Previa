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
