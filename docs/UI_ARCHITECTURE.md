# Presentation architecture

Accepted scheduling policy and the recommended repository → PR → build → preview
screen flow are in [BUILD_POLICY_AND_USER_FLOW.md](BUILD_POLICY_AND_USER_FLOW.md).
Manual-default mode controls and dedicated repository/PR pages are implemented.
Automatic intent follows a five-minute quiet period; real preview execution remains
unavailable. See [APP_USER_FLOW.md](APP_USER_FLOW.md) for current screen behavior.

The server renders Templ pages and serves embedded assets. HTMX is loaded locally;
existing forms retain their ordinary HTTP behavior. There is no React application,
Node.js runtime dependency, or frontend service. Optional browser tests use Node.js
only as development tooling.

## Ownership and dependencies

| Location | Responsibility |
| --- | --- |
| `internal/<feature>` | Routes, handlers, application decisions, repositories, jobs, authentication/authorization. |
| `web/pages/<feature>` | Full pages and feature-specific HTMX fragments when needed. |
| `web/layouts` | Shared HTML shell, stylesheet/script references and content slots. |
| `web/components` | Application-specific composition and copied reusable component subpackages. |
| `web/styles` | Handwritten Tailwind entry CSS and theme tokens. |
| `web/static` | Browser assets, including generated CSS and local HTMX. |
| `web/assets.go` | Embeds only static assets and returns the static HTTP handler. |

Dependencies point from feature handlers to page packages, then to layouts and
components. Presentation never imports `internal` feature packages, queries data,
reads sessions, or authorizes actions. Handlers pass primitive values today;
larger screens can define view models in their page package. Avoid shared DTO
packages until actual reuse calls for one. Use aliases such as `authpages` and
`dashboardpages` to distinguish backend and presentation imports.

`/repositories` renders only the repository directory. `/repositories/{id}` and
`/repositories/{id}/pull-requests/{number}` render dedicated pages owned by
`internal/pullrequests`. Source and PR selection use ordinary page links;
breadcrumbs preserve directory and PR filters. Earlier query-selection URLs
redirect after ownership checks through a small consumer-defined navigator.
HTMX uses `repository-results` for directory search/pagination,
`pull-request-results` for PR search/pagination, and `repository-detail` for sync
polling and policy saves. Only the requested fragment is rendered. Every query
rechecks installation ownership; `Vary` separates full/partial representations.
Navigation values are whitelisted and mutations never accept arbitrary return
URLs. Last synced, Sync Pull Requests and the Manual-default Automatic switch
remain at the top of the repository/PR page; enabling requires modal confirmation.
Desktop search/filter controls share a row; tablet/mobile controls stack. Pages
have bounded scrolling regions and motion respects reduced-motion preferences.

The root `web` package stays focused on assets; it does not aggregate page packages.
The public home page is in `web/pages/home`; authenticated dashboard/session pages
are in `web/pages/dashboard`, and auth error pages are in `web/pages/auth`.
Successful OAuth redirects to `/dashboard`; logout returns to `/`. Overview,
Repositories, Builds, Environments and Developer tools are separate authenticated
HTTP pages. `internal/installations` owns `/repositories`; `internal/dashboard` owns overview, session and tools. `internal/pullrequests`
owns dedicated repository/PR pages, discovery/policy actions and ownership-scoped current
Builds/Environments queries. Their page wrappers reuse the shared activity presentation;
view models contain display data, never backend types. Build execution remains disabled.

`layouts.Document` owns the HTML document and local asset references. `layouts.Base`
adds public navigation for standalone pages; `layouts.App` composes the shared
navbar, sidebar, mobile workspace navigation, and authenticated content slot.
Authentication remains in handlers/middleware, never in templates.

`web/styles/site.css` imports `base.css`, `home.css`, `workspace.css`,
`repositories.css`, `confirmation.css`, and `motion.css`. Each owns its base and responsive rules;
`theme.css` centralizes the approved palette and fonts without changing their values.
The existing pipeline still produces one embedded stylesheet. Duplicate selector
declarations are consolidated rather than overridden by later blocks. Reduced
motion disables animations and transforms. Decorative workflow illustrations are
explicitly labeled as concepts; dashboard identity and queue results use actual
server values. No invented metrics or activity are displayed.

`layouts.App` receives an explicit `components.NavItem`; page titles do not decide
which navigation item is active. Overview, Repositories, Environments, Builds, Developer tools and no-selection
are explicit values. Sidebar/mobile links use real URLs, not dashboard anchors.

`web/static/js/ui.js` has private initializers for motion, navigation, confirmation
dialogs, account menus, and queue forms. It remains one plain browser script.
It progressively enhances native account menus with outside-click
and Escape dismissal and the existing queue form with inline pending/success/error
feedback. Workspace request events preserve filters across independent swaps,
share a replacement request slot for selections/PR filters/policy/refresh, and
retain content on errors. Polls use the same slot with drop behavior; they pause
while PR search is focused or policy is open. Dialog bindings are initialized after
HTMX loads as well as on first load. A `js-enabled` root class scopes fallback CSS
so fragment parsing cannot expand the workspace with hidden noscript content.
An expired HTMX session returns `HX-Redirect` to sign-in for full-document navigation.
Logout remains a normal CSRF-protected POST. The script adds no application
dependency or frontend service; without it, menus and HTTP forms remain usable.

## Styling and reused components

- **Official Tailwind CSS v4.1.18**, independent of the component library. The
  standalone CLI installer pins the release and checks platform-specific SHA-256
  digests from GitHub release assets. Linux/WSL2 and macOS x64/arm64 are supported.
- **Stable templUI v1.13.2** supplies copied button, card, alert and input source.
  This is a community shadcn-style Go/Templ project, not official React shadcn/ui
  or the newer shadcn-templ v2 beta. Templ stays at v0.3.1020.
- Component imports use local helpers and pinned tailwind-merge-go v0.2.0. The
  optional input password-toggle feature/scripts/icons are omitted; inputs use
  native browser behavior. No additional component JavaScript is required.
- The MIT license and precise adaptations are recorded in
  [component provenance](../web/components/README.md). Component source is owned
  locally; upstream fixes require reviewed source updates. Ongoing v1 support
  and automatic migration to v2 are not assumed.
- Theme tokens come from the same release. Shared components handle styling and
  variants; pages compose them without introducing wrapper hierarchies.
- Use complete class strings. Tailwind scans layouts/pages/components explicitly,
  including handwritten Go helpers; it cannot infer dynamically assembled classes.

## Generation and development

1. `make assets` installs the pinned CLI on first use and compiles/minifies
   `web/styles/app.css` into `web/static/css/app.css`.
2. `make templates` runs pinned Templ for `web/`; `make generate` also runs sqlc.
   `make watch-templates` uses Air polling to regenerate templates without running
   the HTTP server or loading application configuration. Use it instead of
   `make dev` when only editing templates; avoid running both watchers together.
3. Build/test/lint/server targets run both before compilation, so the server embeds
   the current CSS. `make run-worker` generates Go code but does not require CSS
   tooling. The worker does not serve a UI.
4. `make dev` runs Air. Each rebuild compiles CSS, generates templates, rebuilds
   and restarts the server. Generated Templ/dbgen output, compiled CSS, and tool
   binaries are excluded from watching to avoid loops. Refresh the browser after
   rebuilds; browser auto-refresh is not configured.

`.templ` source and copied components are committed. Generated `*_templ.go` stays
beside its source and is ignored; compiled CSS and downloaded CLI binaries are also
ignored. Use the Make targets on a fresh checkout. Direct server compilation needs
assets and Go generation first. No CI or application Dockerfile currently exists;
future builds must perform these steps before compiling the server.

All run targets retain automatic local `.env` loading and exported overrides.
See [development setup](DEVELOPMENT_SETUP.md) for commands and expected results.

## Approved visual direction

Preserve the warm paper, terracotta, and forest palette and system-sans/Georgia
typography throughout the application. Theme changes require user approval. Reuse
shared theme tokens and existing components rather than adding unrelated themes.

The dashboard is an operational workspace: a sticky navbar, fixed desktop sidebar,
and collapsible mobile navigation. Repositories, Environments, and Builds link to
dashboard sections. Repository linking is available when the GitHub App is configured;
its installation list uses handler-provided display models. Without App configuration
the connection button explains its disabled state. Environments/builds remain upcoming. The
existing queue diagnostic lives in a collapsed Developer tools section; navigation
to its anchor opens it. No counters, repositories, build history, or environment
status are fabricated.

Home-page sections reveal once on viewport entry using IntersectionObserver. The
hero pauses when offscreen or the tab is hidden. Without JavaScript, unsupported
observers, or with reduced motion enabled, content remains visible. Menu/form
feedback stays in the small local script; no animation dependency is added.

## Confirmation dialogs and generated-code diagnostics

Confirmation decisions open in a shared native `<dialog>` over the current page.
`components.ConfirmationDialog` owns accessible labeling and the shell; callers
supply actions. `LogoutConfirmation` supplies the CSRF-protected logout form.
The authenticated layout receives the CSRF token from its feature handler and
includes the dialog on dashboard and session pages. Presentation never reads
sessions itself. Cancel initially receives focus; Escape, close, backdrop, and
Cancel dismiss without a request. Focus returns to the account-menu summary.
Confirm submits the existing POST; successful logout returns to the home page.
Legacy GET `/auth/logout` redirects to `/dashboard?confirm=logout`, which opens
the same modal. It never ends a session on GET. Native command buttons work
without JavaScript in supporting browsers; the local script supports other
modern browsers and deep links. No modal dependency was added.

Go/editor tooling resolves Templ functions from generated `*_templ.go`, not
directly from `.templ` files. Undefined component names or stale argument-count
diagnostics can indicate missing/stale generated code or language-server state.
Run `make generate`, then `go build ./...`. For template-only work, run
`make templates` once or `make watch-templates`. `make dev` already regenerates
on changes. If the build passes but diagnostics remain, reload the editor
workspace/restart its Go and Templ language servers, and check it uses this
repository and its required Go toolchain. Do not hand-edit generated Go files.

## Repeatable browser verification

`web/tests/browser` contains an opt-in Go HTTP fixture and Playwright v1.58.2 checks.
Run `make browser-install` once with Node.js 20+, then `make test-browser`. Ordinary
build/test/lint commands require no Node.js and skip the browser fixture. Air excludes
this directory so installed dependencies and screenshots do not trigger rebuilds.
The fixture uses real authentication/session/CSRF middleware with an in-memory
session store and fake GitHub/user/queue providers. It needs no application server,
Postgres, OAuth credentials, or `.env`; it never enters the production application.
Checks cover responsive/sticky navigation, mobile page navigation, modal focus/dismissal,
logout/session deletion, CSRF failures, configured installation-list responsiveness,
queue feedback, motion and no-JavaScript
behavior. See [browser test instructions](../web/tests/browser/README.md).

## Repository browsing

`internal/installations/queries.sql` scopes every count/list to the logged-in
user's installation ownership. The page uses a fixed 10-row limit, stable
case-insensitive name/ID ordering, a literal case-insensitive substring search
(up to 100 characters), and all/available/unavailable access filters. Suspended
or inactive installations cannot count as available. Count and list share a
read-only repeatable-read transaction; empty results have one empty page and
requests beyond the last page redirect to the last valid page before rendering.
Search submissions reset to page 1; pagination preserves query/filter values.
No application-side full repository list is used to render pages.

Overview reads actual aggregate counts only. The repositories page has its
connection action at the top, differentiated empty and no-match states, explicit
access badges, a displayed record range, and bounded page-number navigation.
The action changes from Install on a repo to Manage connections once an
installation is linked. It still uses the existing verified redirect flow;
there is no new management API or automatic synchronization. Linking returns
to Repositories with a session-backed, one-use confirmation notice.
All pages reuse the sticky header, sidebar, logout modal, palette and fonts.

Directional arrows use shared SVG components (`Arrow`, `ArrowLeft`,
`ArrowUpRight`) with the navbar's rounded stroke style and inherited colour.
Use these instead of Unicode/emoji arrows to avoid platform-specific coloured
glyphs or boxed icons. Outward link arrows point diagonally up/right; previous
and next page controls retain their respective horizontal direction.

Repository browsing fills the available workspace height. The results region
scrolls internally; the compact heading, filters, sticky column headings and
pagination remain visible. HTMX sends a debounced GET after 300ms of search input
or an access-filter change, replacing only `repository-results`. Search always
starts at page 1, preserves input focus and updates the URL without creating
a history entry for every keystroke. Pagination uses ordinary links enhanced
with HTMX; concurrent requests share replacement synchronization to discard
older requests. Failed searches preserve current rows and display retry feedback.
An empty query restores all owned repositories within the selected access filter.
Full GET forms/page links remain usable without JavaScript. Partial responses
retain the same authentication/ownership checks and vary by HX request/target headers.

The repository workspace uses a centered, capped 1440px content boundary with
balanced desktop/mobile padding and independent scroll regions. Live search has no
Search/Reset buttons; clearing the field restores results and Enter remains
the ordinary GET-form fallback. Search focus uses a subtle forest border
rather than the detached outer ring. This is now the shared text-input, textarea
and select focus style in `base.css`; copied input components defer to it. Invalid
fields keep their error borders, and buttons/radios retain their keyboard cues.

Repository access uses native radio choices styled as segmented tabs:
Available (default), Unavailable and All. Tab choices send the same filtered
GET request and reset pagination; native arrow-key selection and the Enter
search fallback remain usable without JavaScript. Explicit `access=all` stays
in pagination URLs so it cannot silently revert to the Available default.
Previously linked repositories remain stored with access flags; that retention
behaviour is unchanged. Available excludes removed access and inactive/suspended
installations, while All intentionally includes the retained history.
