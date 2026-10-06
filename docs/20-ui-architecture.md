# 20 · UI architecture

How the dashboard is built: server-rendered pages, progressive enhancement, the
visual system and the browser behaviours layered on top. Screen behaviour from a
user's point of view is in [03-user-guide.md](03-user-guide.md).

## Principles

- **Server-rendered first.** Go `templ` renders every page; HTMX (local copy, v2.0.11)
  swaps fragments; one plain script (`web/static/js/ui.js`) adds enhancements. No
  SPA, frontend service or Node.js runtime.
- **Works without JavaScript.** Links, GET forms, POST forms and native `<dialog>`
  confirmations all function with scripting off.
- **Honest UI.** Only real saved state is shown: no invented metrics, activity,
  health or URLs. A control that would apply but cannot work now (no worker that
  can build, no GitHub access, conflicts, builds paused) is shown **disabled**, with
  a notice beside it that says why and what fixes it, so owners can find the action
  and learn how to enable it. A control that does not apply (building a closed PR)
  is absent. Unbuilt features are labelled ("Coming next" on the home page).
- **Strict Content-Security-Policy.** Every page is served with a per-request nonce
  policy: the theme boot script carries the nonce (and passes it to the style it
  creates), `ui.js` gives it to the speculation rules it injects, and HTMX is
  configured (`htmx-config` meta) not to inject styles or evaluate code. No inline
  event handlers, inline styles or `eval`. The browser suite runs under the same
  policy and fails on any violation ([22](22-observability-and-hardening.md)).
- **Presentation never reaches into the backend.** Templates receive display data;
  they never query, read sessions or authorize.

## Ownership and dependencies

| Location | Responsibility |
| --- | --- |
| `internal/<feature>` | Routes, handlers, authorization, queries, jobs. |
| `web/pages/<feature>` | Full pages, HTMX fragments and page-owned view models. |
| `web/layouts` | `Document` (HTML shell, theme boot script, assets), `Base` (public pages), `App` (authenticated shell). |
| `web/components` | Brand, sidebar, header, command palette, confirmation dialog, repository tabs, icons, navigation keys; copied templUI primitives. |
| `web/styles` | Tailwind entry (`app.css`) and handwritten CSS modules. |
| `web/static` | HTMX, compiled CSS, `ui.js`; embedded by `web/assets.go`. |

Dependencies point one way: feature handler → page package → layouts/components.
Use import aliases (`authpages`, `dashboardpages`, `prpages`) where names collide.
Ownership of routes: `internal/dashboard` (home, overview, session, tools),
`internal/installations` (`/repositories`), `internal/pullrequests` (repository and
PR pages, Builds, Environments, the event stream), `internal/secrets` (a
repository's Secrets tab).

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
  Environments), System (Developer tools).
  `layouts.App` receives an explicit `components.NavItem`; titles never decide the
  active item.
- **Repository tabs.** `components.RepositoryTabs` switches a repository between
  **Pull requests** (`/repositories/{id}`) and **Secrets** (`/repositories/{id}/secrets`).
  Both are full pages, so the switch works without JavaScript and view transitions
  keep it smooth. PR detail pages do not show the tabs.
- **Dedicated pages.** `/repositories`, `/repositories/{id}`,
  `/repositories/{id}/pull-requests/{number}` and its `/logs` page are separate,
  bookmarkable pages. PR detail and every Builds/Environments row link to the logs.
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
| `#repository-detail` | Sync, policy save, live refresh; on a PR page Build, Retry, Rebuild and the teardown dialog's form | Server re-renders the whole panel. Without JavaScript the same forms post and redirect back (post/redirect/get). |

- Every partial response repeats authentication and ownership checks; responses vary
  on `HX-Request`/`HX-Target`. An expired session returns `HX-Redirect` to sign-in.
- Requests to one target share a replacement slot so older responses are discarded.
  Failed requests keep current content and show retry feedback.
- Live refreshes come from the event stream ([19](19-realtime-updates.md)). A hidden
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
`initCommandPalette`, `initRepositorySearch` (request/feedback/focus handling),
`initLiveUpdates` (repository EventSource lifecycle), `initBuildLogs` (the log
page's EventSource: appends lines as text nodes, clears on `reset`, updates the
status card from `state`, follows output only while the reader is at the bottom,
pauses in hidden tabs, marks error-looking lines), `initRelativeTimes` (rewrites
`time[data-relative]` as "3 minutes ago" every 30 s and after swaps, keeping the
exact UTC time as the title), `initCopyButtons` (`data-copy` text or a log list's
lines to the clipboard, confirmed for two seconds), `initSecretEditor` and
`initBulkDelete` (the Secrets tab, below). A `js-enabled` class on `<html>` scopes
CSS that only makes sense with scripting. No third-party JavaScript besides HTMX.

## Confirmations

Decisions open a shared native `<dialog>` (`components.ConfirmationDialog`) over the
current page: labelled title and description, Cancel focused first, Escape/close/
backdrop dismiss without a request, focus returns to the trigger. The caller
supplies the CSRF-protected form. Logout, enabling Automatic builds, deleting a
secret or the selected secrets, and tearing a preview down or cancelling an
unfinished build (on a PR page; per row on Environments) use it.
Native `commandfor`/`command` attributes open dialogs without JavaScript in
supporting browsers.

## Pull request page

One status panel answers "what is happening with this preview?": a headline with a
tone (Preview is live, Building, Build failed, Blocked by merge conflicts, No
preview yet, ...), the preview address with **Copy**, and every action in one row
(**Open preview** first when one is live, then Build/Retry/Rebuild, Tear down or
**Cancel build** when nothing is deployed, and **Build logs**). When building is
blocked, the build action is shown disabled (`aria-describedby` points at the
notice) and the notice names the reason and the fix, for example a worker without
`PREVIEW_BASE_DOMAIN`. A failed build quotes the last twelve lines of its own output
(fenced by generation and revision, so never another attempt's), with error lines
highlighted. Three lifecycle cards follow: the latest commit (linked to GitHub) and
its mergeability, the build request (who or what asked, when it started and
finished, how long it took, whether it is for an earlier commit), and the deployed
preview (published and expiry times). Times are relative with the exact UTC time as
their title. The back link returns to the repository's PR list with its filters.

## Overview

A getting-started checklist appears only until every step is done. When no worker
can build, the pipeline panel moves to the top and says exactly what fixes it.
Delivery counts come first, then the five newest live previews (Open and Copy) and
build requests, then the pipeline and the source counts (repositories, open pull
requests, installations, unavailable access).

## Secrets tab

Write-only by design. The **Stored secrets** table lists names and update times,
each with Replace and Delete; checkboxes and **Delete selected** remove several
after one confirmation (the checkboxes belong to the dialog's form, so this works
without JavaScript). The editor below adds or replaces several values at once, one
name and value per line like a key-value table, plus a **Paste or import a .env
file** section. With JavaScript, **Add another line** and × manage lines, pasting
several `NAME=value` lines into a name splits them into lines, a chosen .env file is
read in the browser (never uploaded on its own), **Move into lines above** turns
pasted text into lines to review, names that exist say "Replaces the stored value",
and duplicate, reserved or incomplete lines are flagged before submit. Without
JavaScript the page renders spare lines and the server parses pasted text with the
same rules (`secrets.ParseDotenv`). The server saves everything in one transaction
(all or nothing; the limit counts new names only). A rejected save re-renders the
names with the reason but never a value or the pasted text. See
[07](07-repository-secrets.md).

## Build log page

`web/pages/builds/logs.templ` renders a status card (badge, one-sentence summary that
is a polite `role="status"` region, commit, connection indicator, notices) and an
output panel: an ordered list in its own scroll area (so following output never moves
the page), monospace, with per-line anchor links and system notices in the info
tone. Without JavaScript it is a complete page: the newest 500 lines plus
**Earlier lines**, **Later lines** and **Refresh**; the indicator says **Live
updates off** and the follow control stays hidden. Notices are cloned from a
server-rendered `<template>` so live updates look identical to the first render.
Error-looking lines (`error`, `failed`, `fatal`, `panic` as whole words) are tinted;
the text is unchanged. **Copy** puts the lines shown on the clipboard and
**Download** (`…/logs/download`) sends the attempt's whole output as a text file.
Styles live in `web/styles/logs.css`. See [08](08-build-logs.md).

## Preview unavailable page

`web/pages/routing/unavailable.templ` is the public page the router redirects unknown
and removed preview hostnames to ([11](11-routing-and-https.md)). It uses the base
layout's signed-out navigation, answers 404, is mounted outside the session/CSRF
middleware (it reads and sets no cookie), sends `Cache-Control: no-store`,
`X-Robots-Tag: noindex` and `Referrer-Policy: no-referrer`, and never names the host
that was requested. Its paragraph uses the shared `.standalone-copy` style.

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
  `logs`, `confirmation` and `motion` modules.
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
updates without polling, the command palette, theme persistence, the repository
Secrets tab (write-only saving, replacing, modal deletion), build logs (live append,
following, reset for a new attempt, inert script-looking output, paging without
JavaScript), build requests (one per click, no second Build while queued), preview
teardown confirmed in a modal (PR page and Environments rows, with and without
JavaScript), unavailable actions, Builds/Environments content, reduced motion and
no-JavaScript flows. See [02](02-development-setup.md#browser-regression-checks-optional).

## Decisions and trade-offs

| Decision | Why | Trade-off |
| --- | --- | --- |
| Server-rendered templ + HTMX | One binary, no frontend build service, works without JS. | Complex client state is harder; fine for an operational dashboard. |
| Dedicated pages over a master/detail panel | Bookmarkable URLs, simple history, clear focus; chosen after trying a panel layout. | More navigations, softened by view transitions and prefetch. |
| View transitions + prefetch, not `hx-boost` | Smooth navigation while keeping real document loads and no-JS behaviour. | Effects only in supporting browsers. |
| Command palette as a layer over links | Faster navigation for keyboard users without a second navigation model. | Duplicate entry points to keep in sync. |
| System fonts, no CDNs | Self-hosted, private, fast first paint. | Less typographic control. |
| SSE invalidation, refetch via GET | No polling; authorization stays in ordinary handlers ([19](19-realtime-updates.md)). | One connection per open page. |
