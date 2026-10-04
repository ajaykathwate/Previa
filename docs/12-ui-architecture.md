# 12 · UI architecture

How the dashboard is built: server-rendered pages, progressive enhancement, the
visual system and the browser behaviours layered on top. Screen behaviour from a
user's point of view is in [03-user-guide.md](03-user-guide.md).

## Principles

- **Server-rendered first.** Go `templ` renders every page; HTMX (local copy, v2.0.11)
  swaps fragments; one plain script (`web/static/js/ui.js`) adds enhancements. No
  SPA, frontend service or Node.js runtime.
- **Works without JavaScript.** Links, GET forms, POST forms and native `<dialog>`
  confirmations all function with scripting off.
- **Honest UI.** Only real saved state is shown. Unbuilt features are labelled
  ("Coming next", "Soon"); no invented metrics, activity, health or URLs.
- **Presentation never reaches into the backend.** Templates receive display data;
  they never query, read sessions or authorize.

## Ownership and dependencies

| Location | Responsibility |
| --- | --- |
| `internal/<feature>` | Routes, handlers, authorization, queries, jobs. |
| `web/pages/<feature>` | Full pages, HTMX fragments and page-owned view models. |
| `web/layouts` | `Document` (HTML shell, theme boot script, assets), `Base` (public pages), `App` (authenticated shell). |
| `web/components` | Brand, sidebar, header, command palette, confirmation dialog, icons, navigation keys; copied templUI primitives. |
| `web/styles` | Tailwind entry (`app.css`) and handwritten CSS modules. |
| `web/static` | HTMX, compiled CSS, `ui.js`; embedded by `web/assets.go`. |

Dependencies point one way: feature handler → page package → layouts/components.
Use import aliases (`authpages`, `dashboardpages`, `prpages`) where names collide.
Ownership of routes: `internal/dashboard` (home, overview, session, tools),
`internal/installations` (`/repositories`), `internal/pullrequests` (repository and
PR pages, Builds, Environments, the event stream).

## Visual system: the "signal" theme

- **Palette:** graphite neutrals with a lime signal accent, defined as tokens in
  `web/styles/theme.css`. Semantic tones (`tone-ok`, `-info`, `-warn`, `-danger`,
  `-merged`, `-neutral`) colour badges and icons; `pullrequests.Tone()` maps saved
  states to them.
- **Light and dark:** tokens switch under `prefers-color-scheme: dark` or an explicit
  `<html data-theme>`. The header toggle stores the choice in `localStorage`
  (`previa-theme`); an inline script in `Document` applies it before first paint.
- **Type:** system sans-serif for UI; system monospace for commits, branches and
  labels. No external fonts or CDNs, keeping the app self-hostable and private.
- **Rules:** use the shared tokens and components; ask before changing the theme
  (recorded in `AGENTS.md`). Text inputs use a quiet focus style (border change, no
  outline or glow); buttons and links keep visible focus rings.

## Shell and navigation

- **Fixed-height shell.** Sidebar and header stay put; `.app-content` is the single
  scroll region, so window scroll stays at zero and toolbars can be sticky.
- **Responsive sidebar.** Full grouped sidebar at 1180px and wider; an icon rail
  between 900 and 1180px; below 900px a drawer opened from the header (a native
  `<details>`, closed by link click, backdrop or Escape).
- **Navigation groups:** Workspace (Overview, Repositories), Delivery (Builds,
  Environments), System (Developer tools), plus non-link "Coming next" items.
  `layouts.App` receives an explicit `components.NavItem`; titles never decide the
  active item.
- **Dedicated pages.** `/repositories`, `/repositories/{id}` and
  `/repositories/{id}/pull-requests/{number}` are separate, bookmarkable pages.
  Breadcrumbs and Back links preserve filters (`q`, `access`, `page` for the
  directory; `pr_q`, `pr_state`, `pr_page` for PRs). Old query-style bookmarks
  redirect after ownership checks.

Making full-page navigation feel instant, without giving up MPA semantics:

| Technique | Behaviour | Fallback |
| --- | --- | --- |
| Cross-document view transitions | Content fades and lifts in; the sidebar stays still. Opted in by the inline script, so off without JavaScript and with reduced motion. | Normal navigation. |
| Hover prefetch (Speculation Rules) | Workspace links are prefetched on hover. Excludes `/`, auth, installation, queue, webhook and static paths, and HTMX-driven links. | Normal navigation. |
| Command palette (Ctrl/⌘ K) | Filterable list of pages, repository filters, theme switch, session, home and "Log out…" (which opens the normal confirmation). Every entry also exists as a link. | Sidebar links. |

## HTMX contracts

| Target | Updated by | Notes |
| --- | --- | --- |
| `#repository-results` | Directory search (300 ms debounce), access tabs, pagination | URL replaced, not pushed per keystroke; input keeps focus. |
| `#pull-request-results` | PR search, state tabs, pagination | Same behaviour, independent query keys. |
| `#repository-detail` | Sync, policy save, live refresh | Server re-renders the whole panel. |

- Every partial response repeats authentication and ownership checks; responses vary
  on `HX-Request`/`HX-Target`. An expired session returns `HX-Redirect` to sign-in.
- Requests to one target share a replacement slot so older responses are discarded.
  Failed requests keep current content and show retry feedback.
- Live refreshes come from the event stream ([11](11-realtime-updates.md)). A hidden
  `[data-live-refresh]` element refetches `#repository-detail` with the current
  URL's filters. While PR search is focused or the policy modal is open, a refresh
  is deferred, then run when the user leaves the field or closes the modal.
- After a user-triggered results swap, the view scrolls back under the sticky
  toolbar if the results' top had scrolled out of sight.

## `ui.js`

One IIFE of small, independent initializers: `initMotion` (home reveal, hero pause
offscreen), `initTheme`, `initNavigation` (drawer dismissal, platform shortcut label),
`initPrefetch`, `initConfirmationDialogs` (focus trap, backdrop close, focus return,
`?confirm=logout` deep link), `initAccountMenus`, `initQueueForms`,
`initCommandPalette`, `initRepositorySearch` (request/feedback/focus handling) and
`initLiveUpdates` (EventSource lifecycle). A `js-enabled` class on `<html>` scopes
CSS that only makes sense with scripting. No third-party JavaScript besides HTMX.

## Confirmations

Decisions open a shared native `<dialog>` (`components.ConfirmationDialog`) over the
current page: labelled title and description, Cancel focused first, Escape/close/
backdrop dismiss without a request, focus returns to the trigger. The caller
supplies the CSRF-protected form. Logout and enabling Automatic builds use it.
Native `commandfor`/`command` attributes open dialogs without JavaScript in
supporting browsers.

## Repository directory queries

`internal/installations` scopes every count and list to the signed-in owner's
installations: 10 rows per page, stable case-insensitive name/ID order, a literal
case-insensitive substring search (up to 100 characters) and Available/Unavailable/
All filters (suspended or inactive installations never count as available). Count
and list share a read-only repeatable-read transaction; pages beyond the last
redirect to the last valid page. The PR list follows the same 10-row pattern.

## Styling pipeline

- **Tailwind CSS v4.1.18** standalone CLI, checksum-pinned. `make assets` compiles
  `web/styles/app.css` into `web/static/css/app.css` (ignored, embedded at build).
  `site.css` imports `base`, `home`, `workspace`, `command`, `repositories`,
  `confirmation` and `motion` modules.
- **templUI v1.13.2** (MIT) supplies copied button, card, alert and input source;
  provenance and local changes are in [web/components/README.md](../web/components/README.md).
  Use complete class strings so Tailwind can find them.
- `make templates` regenerates templ code, `make generate` also runs sqlc,
  `make watch-templates` regenerates on save, `make dev` rebuilds and restarts the
  server. Generated `*_templ.go`, compiled CSS and tool binaries are ignored.

## Browser tests

`web/tests/browser` runs Playwright 1.58.2 against an in-process Go fixture with the
real auth, session, CSRF and realtime stream code and fake GitHub/queue providers.
It covers responsive layout and overflow, navigation and the drawer, modal focus and
dismissal, logout, CSRF, live search, pagination, Automatic confirmation, live
updates without polling, the command palette, theme persistence, reduced motion and
no-JavaScript flows. See [02](02-development-setup.md#browser-regression-checks-optional).

## Decisions and trade-offs

| Decision | Why | Trade-off |
| --- | --- | --- |
| Server-rendered templ + HTMX | One binary, no frontend build service, works without JS. | Complex client state is harder; fine for an operational dashboard. |
| Dedicated pages over a master/detail panel | Bookmarkable URLs, simple history, clear focus; chosen after trying a panel layout. | More navigations, softened by view transitions and prefetch. |
| View transitions + prefetch, not `hx-boost` | Smooth navigation while keeping real document loads and no-JS behaviour. | Effects only in supporting browsers. |
| Command palette as a layer over links | Faster navigation for keyboard users without a second navigation model. | Duplicate entry points to keep in sync. |
| System fonts, no CDNs | Self-hosted, private, fast first paint. | Less typographic control. |
| SSE invalidation, refetch via GET | No polling; authorization stays in ordinary handlers ([11](11-realtime-updates.md)). | One connection per open page. |
