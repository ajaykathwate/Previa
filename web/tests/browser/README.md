# Optional browser regression checks

This is development tooling, not an application dependency or deployed service.
Playwright is locked to **1.58.2**; use Node.js **20+**.

From the repository root:

```sh
make browser-install
make test-browser
```

The first command installs locked npm dependencies and Chromium. The second
generates assets/templates and runs nine browser scenarios against a temporary
Go HTTP server, which closes automatically. Expected: nine passes, no JavaScript
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

To capture ignored screenshots:

```sh
UI_BROWSER_SCREENSHOTS=1 make test-browser
```

Files appear in `web/tests/browser/artifacts/`. These are inspection artifacts,
not committed snapshot baselines. `UI_BROWSER_EXECUTABLE=/absolute/path/to/chromium`
optionally selects an existing compatible browser; the default uses Playwright's
installed Chromium. Neither variable is application configuration.

If Chromium reports missing Linux system libraries, install its OS dependencies
(this may request administrator privileges):

```sh
npm exec --prefix web/tests/browser -- playwright install-deps chromium
```

If the browser executable is missing, rerun `make browser-install`. Both Air
configurations exclude this directory so dependencies/artifacts do not trigger
application reloads. Installed packages and screenshots are Git-ignored.
