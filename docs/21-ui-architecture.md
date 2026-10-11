# 21 · UI architecture

How the dashboard is built: server-rendered pages, progressive enhancement, the
visual system and the browser behaviours layered on top. Screen behaviour from a
user's point of view is in the
[user guide](../technical-docs/user-guide.md).

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
  is absent. The public home page describes only what works today, at the level
  a user needs: no roadmap, no internal stack or hosting details.
- **Strict Content-Security-Policy.** Every page is served with a per-request nonce
  policy: the theme boot script carries the nonce (and passes it to the style it
  creates), `ui.js` gives it to the speculation rules it injects, and HTMX is
  configured (`htmx-config` meta) not to inject styles or evaluate code. No inline
  event handlers, inline styles or `eval`. The browser suite runs under the same
  policy and fails on any violation ([23](23-observability-and-hardening.md)).
- **Presentation never reaches into the backend.** Templates receive display data;
  they never query, read sessions or authorize.

## Ownership and dependencies

| Location | Responsibility |
| --- | --- |
| `internal/<feature>` | Routes, handlers, authorization, queries, jobs. |
| `web/pages/<feature>` | Full pages, HTMX fragments and page-owned view models. |
| `web/layouts` | `Document` (HTML shell, theme boot script, assets), `Base` (public pages), `App` (authenticated shell). |
| `web/components` | Brand, sidebar, header, command palette, confirmation dialog, repository tabs, conversation primitives (avatars, messages, typing indicator), icons, navigation keys; copied templUI primitives. |
| `web/styles` | Tailwind entry (`app.css`) and handwritten CSS modules. |
| `web/static` | HTMX, compiled CSS, `ui.js`; embedded by `web/assets.go`. |

Dependencies point one way: feature handler → page package → layouts/components.
Use import aliases (`authpages`, `dashboardpages`, `prpages`) where names collide.
Ownership of routes: `internal/dashboard` (home, overview, session, tools, FAQ),
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
- **Brand:** `components.BrandMark` in the UI; `web/static/brand` holds the
  favicon (SVG following the tab theme, PNG fallback, Apple touch icon, also served
  at `/favicon.ico`) and downloadable logo PNGs ([web/static/README.md](../web/static/README.md)).
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
- **Navigation groups:** Workspace (Overview, Repositories, Starter kits), Delivery
  (Builds, Environments), System (Developer tools), Help (FAQ).
  `layouts.App` receives an explicit `components.NavItem`; titles never decide the
  active item.
- **Header trail.** The header names only the previous level and the current page
  (`layouts.AppTrail` with a `components.Crumb`): "Repositories / repository-1",
  "repository-1 / #42 Title", "#42 Title / Build logs". The previous level is the
  page's one link back and keeps that page's filters; top-level pages show
  "Workspace / Overview" with no link. Below 650px the label folds into a back
  arrow. Pages under a repository open with a shared hero
  (`components.RepositoryHero`: owner, name, one line of context).
- **Repository tabs.** `components.RepositoryTabs` switches a repository between
  **Pull requests** (`/repositories/{id}`), **Secrets** (`/repositories/{id}/secrets`)
  and **Outbound** (`/repositories/{id}/outbound`) as an underlined tab bar. Each is
  a full page, so the switch works without JavaScript and view transitions keep it
  smooth. PR detail pages do not show the tabs.
- **Dedicated pages.** `/repositories`, `/repositories/{id}`,
  `/repositories/{id}/pull-requests/{number}` and its `/logs` page are separate,
  bookmarkable pages. PR detail and every Builds/Environments row link to the logs.
  The header trail preserves filters (`q`, `access`, `page` for the
  directory; `pr_q`, `pr_state`, `pr_page` for PRs). Old query-style bookmarks
  redirect after ownership checks.

Making full-page navigation feel instant, without giving up MPA semantics:

| Technique | Behaviour | Fallback |
| --- | --- | --- |
| Cross-document view transitions | Content fades and lifts in; the sidebar stays still. Opted in by the inline script, so off without JavaScript and with reduced motion. | Normal navigation. |
| Hover prefetch (Speculation Rules) | Workspace links are prefetched on hover. Excludes `/`, auth, installation, queue, webhook and static paths, and HTMX-driven links. | Normal navigation. |
| Command palette (Ctrl/⌘ K) | Filterable list of pages (including the FAQ), repository filters, theme switch, session, home and "Log out…" (which opens the normal confirmation). Every entry also exists as a link. | Sidebar links. |

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
- Live refreshes come from the event stream ([20](20-realtime-updates.md)). A hidden
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
status card and its step tracker from `lines` and `state`, follows output only
while the reader is at the bottom, pauses in hidden tabs, marks error-looking
lines), `initBuildProgress` (on a PR page whose build is in flight, follows the
attempt's log stream to name the current step and its latest line; closes on
swaps and hidden tabs; a stream that cannot open leaves the server's rendering),
`initElapsed` (counts `time[data-elapsed]` up every second), `initRelativeTimes` (rewrites
`time[data-relative]` as "3 minutes ago" every 30 s and after swaps, keeping the
exact UTC time as the title), `initCopyButtons` (`data-copy` text or a log list's
lines to the clipboard, confirmed for two seconds), `initSecretEditor` and
`initBulkDelete` (the Secrets tab, below), `initStory` and `initFAQ` (the
conversations, below), `initOverview` (the Overview, above). A `js-enabled` class on `<html>` scopes
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

A status card beside a **Details** panel. The card answers "what is happening with
this preview?": an icon and headline in the state's tone (Preview is live,
Building, Build failed, Blocked by merge conflicts, No preview yet, ...), an
elapsed clock while work is in flight, the preview address with **Copy**, and every
action in one row (**Open preview** first when one is live, then
Build/Retry/Rebuild, Tear down or **Cancel build** when nothing is deployed, and
**Build logs**, which reads **Follow the build log** while a build runs). Build
buttons show a spinner and **Starting…** and are disabled while their request is
on its way. When building is blocked, the build action is shown disabled
(`aria-describedby` points at the notice) and the notice names the reason and the
fix, for example a worker without `PREVIEW_BASE_DOMAIN`.

- **Step tracker** (`pages.Tracker`, `progress.go`): Queued → Get code → Build image →
  Start preview → Publish → Live, shown while a request is queued, running or
  publishing, and after a failure (the step it stopped at is marked). The server
  places the request from its state; while it runs, `initBuildProgress` reads the
  attempt's log stream and moves the tracker by the runtime's own progress lines
  (`DetectStage` in Go and `lineStage` in `ui.js` apply the same rules), with the
  latest output line under it. Each step has a screen-reader state.
- **Failures** (`diagnosis.go`): a failed build gets a plain-language title, one
  sentence of context and numbered **What to do next** steps, derived from the
  stored failure category and, for source failures, the runtime's own reason
  (missing App permission, no `.previewrc.yaml`, rate limits, unreachable GitHub,
  Dockerfile and config problems, size and archive limits). It is flagged with a
  badge and the tracker, not a full red card. **Technical details** (collapsed)
  holds the exact reason, the recorded sentence, the category, the commit and the
  last twelve lines of the attempt's own output (fenced by generation and revision,
  so never another attempt's) with error lines highlighted. Builds, Environments
  and the Overview show the same titles, with the recorded sentence as the hover
  title.
- **Details panel:** the latest commit and its mergeability, the build request (who
  or what asked, when it started and finished, how long it took, whether it is for
  an earlier commit, the failure title), the deployed preview (published and expiry
  times) and the environment (managed services, docs, outbound). Times are relative
  with the exact UTC time as their title. Below 1100px it follows the card.

## Overview

`web/pages/dashboard/index.templ`, styled by `web/styles/overview.css`. Every number
comes from `dashboardpages.Summary`; nothing is simulated.

- **Header:** worker status (connected, builds paused, cannot build, none running),
  how fresh the numbers are ("Updated 3 min ago", counted from page load) and a
  refresh link.
- **Blocked notice** first when no running worker can build, with the fix and a link
  to the FAQ answer; then **setup steps** with a progress bar until every step is done,
  the next step highlighted with its action.
- **Delivery pipeline:** open pull requests, waiting or queued, building and live
  previews as connected stages, each with its count, its share of open pull requests
  and a link; stages with work in progress are marked **Now**; a lane below names
  failed requests. All counts are over open pull requests, and the page says that one
  can count twice (a live preview keeps serving while a newer commit builds).
- **Stat tiles:** linked repositories, open pull requests, live previews (with
  coverage) and requests that need attention, each linking to where it comes from.
- **Side column:** preview coverage (live previews over open pull requests, an SVG
  ring) and linked repositories (available versus unavailable).
- **Latest activity** (full width): the newest build requests and live previews as
  two tabs (ARIA tabs with arrow keys; the choice is kept in `sessionStorage`), each a
  table: pull request, commit (or the served commit), status and when (or expiry);
  failed requests name the failure in plain words. Each row is a `<details>` that
  expands to how it was requested, eligible, started and finished times and
  duration, why it waits or failed with the first next step, and links to the pull
  request and its build logs. Preview rows add publication, expiry and a lifetime
  bar, with **Open** and **Copy** beside the row. Below 760px the columns fold under
  the title.
- The worker's state shows only in the header badge and, when nothing can build, the
  blocked notice; the per-stage worker panel was removed to keep the page about
  pull requests.
- Proportions are SVG attributes (`width`, `stroke-dasharray`), so the CSP needs no
  inline styles. `initOverview` adds the tabs, lifetime bars and the updated time;
  without JavaScript both activity lists show and rows still expand.

## Public home page

`web/pages/home` (styles in `web/styles/home.css`), in reading order: a hero with an
example pull request beside its running preview (labelled **Example**), **Watch**
(the 30-second story video: `preload="none"` with a poster frame so nothing
downloads until the visitor presses play, never autoplay, a "Read the story
instead" transcript, and a large play button that `initFilm` adds over the native
controls), **Why Previa** (reviewing before and with Previa, and who it helps), **How it works**,
**Features** (build, run, review, control), **Security**, **Built in the open**
(only what is available today), the FAQ and a call to action. Icons sit on tinted
`.tile-icon` tiles that read in both themes. Copy stays user-level: what Previa
does for a pull request, never how it is built or hosted; Previa is presented as a
hosted service, free during early access.

## Conversations: How it works and the FAQ

The home page explains Previa as a conversation between **Kai**, a maintainer, and
**Previa**, which answers from its brand tile. The same FAQ also has its own workspace
page. The building blocks are `components.PreviaAvatar`, `PersonAvatar`, `PreviaMessage`,
`AskerMessage` and `Typing` (`web/components/conversation.templ`); styles live in
`web/styles/conversation.css`.

- **How it works** (`web/pages/home/story.*`, `#how-it-works`): "From pull request to
  preview, in seven steps", one turn per step (Notice, Check, Build, Run, Share,
  Report, Clean up; anchors keep their earlier ids, such as `#story-route` for Share).
  Each answer carries an example card (pull request, checks, build log, preview,
  link, PR comment, cleanup) tagged **Example**, so nothing reads as live data. With JavaScript, `initStory` turns the transcript into a fixed-height chat:
  **Ask as Kai** adds the next question, Previa "types" for under 1.5 s, then answers
  (no pause with reduced motion); the stage rail and **Show the whole conversation**
  jump ahead and move focus to the turn; **Start over** resets. Turns enter the
  `role="log"` region as new nodes, so screen readers hear them. A `#story-<stage>`
  link opens the conversation at that stage. Without JavaScript the whole transcript is
  shown and the rail stages are anchor links.
- **FAQ** (`web/pages/faq`): 22 questions in four topics (Getting started, Builds and
  previews, Security and secrets, Your code and data), as Go data (`Questions`,
  `Topics`) with answers as templ components, so counts and anchors cannot drift. Each
  question is a native `<details>` whose summary is the asker's bubble: outlined until
  asked, then filled with the asker's avatar, Previa's answer below. `initFAQ` adds topic
  filters, search across questions and answers with a polite result count, a short
  typing pause before an answer, and `#faq-<id>` links that open their answer. Without
  JavaScript the questions still open natively and the filters stay hidden. The home page
  asks as Kai (`faq.Guide`); the workspace page `/faq` (`faq.Page`, sidebar **Help → FAQ**)
  asks as the signed-in owner, with the account avatar. Signed-out visitors to `/faq` are
  redirected to `/#faq`.
- **Keeping it true:** the answers are static, user-level copy describing what works
  today. Change an answer in the same change that alters the behaviour it describes.

## Secrets tab

Write-only by design. The list and editor sit beside a **How secrets reach your
previews** panel (below 1100px it follows them); the Outbound tab uses the same
layout for **How outbound connections work**. The **Stored secrets** table lists names and update times,
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

`web/pages/builds/logs.templ` renders a hero (repository, PR number and title, a link
back to the pull request), a status card (badge, one-sentence summary that is a
polite `role="status"` region, commit, connection indicator, the step tracker,
notices) and a dark terminal-style output panel: an ordered list in its own scroll
area (so following output never moves the page), monospace, with per-line anchor
links and the runtime's own lines marked with `›` in the info tone. Without JavaScript it is a complete page: the newest 500 lines plus
**Earlier lines**, **Later lines** and **Refresh**; the indicator says **Live
updates off** and the follow control stays hidden. Notices are cloned from a
server-rendered `<template>` so live updates look identical to the first render.
Error-looking lines (`error`, `failed`, `fatal`, `panic` as whole words) are tinted;
the text is unchanged. **Copy** puts the lines shown on the clipboard and
**Download** (`…/logs/download`) sends the attempt's whole output as a text file.
Styles live in `web/styles/logs.css`. See [08](08-build-logs.md).

## Starter kits

`/starter-kits` (`internal/starterkits`, templates in `web/pages/starterkits`, styles
in `web/styles/kits.css`, behaviour in `ui.js` `initStarterKit`) writes a `Dockerfile`
and `.previewrc.yaml` for one of nine stacks (Next.js, React + Vite (static), Node.js
API, Django, FastAPI, Ruby on Rails, Go, Spring Boot, Laravel) or **Own Dockerfile**
(only `.previewrc.yaml`). The page is two columns that fit one screen: the choices in
one compact card beside a sticky editor, with the guidance in a row underneath. The
choices and the editor share their own wrapper (`.kit-main`), so the editor stays in
view only beside the choices and never slides over the guidance.

- **Choices.** Stacks are a row of chips (monogram and name) with the chosen stack's
  kind and language under them. Services and extras are switch rows: PostgreSQL and
  Redis with their version as a segmented control in the same row, migrations and a
  background worker with ready-made commands or **My own command…**, and a docs link.
  **Advanced** holds the port, health check path, preview size and a stack detail
  (such as the Django project package).
- **One form, swapped whole.** It is a plain GET form: every change sends it with
  `hx-get` and swaps `#kit` (`hx-select`), with `#kit-status:innerHTML` out of band so
  screen readers hear "Files updated: …"; typing waits 500 ms. The address follows the
  form (`hx-replace-url`), so a kit is shareable. Without JavaScript, **Update files**
  reloads the page. The hidden `from` field tells the server the stack changed: the
  new stack's defaults are taken, keeping services, the switches and the size.
- **Editor.** The files are tabs (`role="tablist"`, arrow keys, Home and End), like a
  code editor: a bar with the file's purpose, **Reset**, **Copy** and **Download**,
  line numbers, and a status bar with the checks. Each file is a `<textarea>` over a
  highlighted copy of its text (`<pre aria-hidden>`, coloured by `ui.js`), so typing,
  selection, undo and screen readers stay native. Editing marks the tab with a dot
  and the bar with **Edited**. Edits survive a change of choices (kept in memory and
  put back on `htmx:afterSwap`, before paint) until **Reset**, which restores the
  newest generated file. Without JavaScript the tabs stay hidden and both files show
  as plain, still editable text areas.
- **Checked like a build.** `starterkits.Generate` corrects and reports bad choices;
  the server parses the YAML with `repoconfig.ParseFor`, the build's own parser
  (`internal/app/starterkits.go`), against the versions the running worker offers
  (Previa's default catalog when none that builds is running). An edited
  `.previewrc.yaml` is posted to `POST /starter-kits/check` 400 ms after typing stops
  and the status bar shows the result, each problem with its key and, where the
  parser knows it, its line. Problems with choices also show beside their field.
- **Copy and Download use the editor's text.** **Copy** reads the text area
  (`data-copy-from`). **Download** submits the file's own form, `POST
  /starter-kits/download/{dockerfile|previewrc}` with the CSRF token; the server
  sends the text back as an attachment with LF line breaks (at most 64 KiB). Browsers
  drop a downloaded file's leading dot, so **Next steps** says to rename
  `previewrc.yaml`.
- **HTMX settling.** HTMX settles the `class` of elements with an id back to the
  server's after a swap, so the editor keeps its own state in data attributes
  (`data-edited`, `data-enhanced`) and sets the tabs up on swap, before paint.
- **Runtime rules built in.** Dockerfiles keep caches and storage in `/tmp`, bind
  `0.0.0.0` and expose the chosen port. Spring Boot and Laravel get a small
  `preview-env` entrypoint that maps `DATABASE_URL`/`PG*` and `REDIS_URL` to the
  names the framework reads; setup and processes call it too, since they replace the
  entrypoint. **Check in your app** lists what the code itself must do, and **Need
  something else?** covers secrets, other databases (via Outbound), plain settings,
  more workers and seed data.

## Preview unavailable page

`web/pages/routing/unavailable.templ` is the public page the router redirects unknown
and removed preview hostnames to ([12](12-routing-and-https.md)). It uses the base
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
  `site.css` imports `base`, `home`, `conversation`, `workspace`, `overview`,
  `command`, `repositories`, `logs`, `surfaces`, `confirmation` and `motion`
  modules. `surfaces.css` holds the shared page pieces (header trail, heroes, tabs,
  two-column sections, tracker, diagnosis, busy buttons, terminal panel, stat
  tiles, activity table).
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
JavaScript, the tracker following progress lines), failed builds (plain title, next
steps, collapsed technical details, Retry showing **Starting…** then the queued step),
running builds (the tracker following the log, the clock, the latest line), build
requests (one per click, no second Build while queued), preview
teardown confirmed in a modal (PR page and Environments rows, with and without
JavaScript), unavailable actions, Builds/Environments content, the How it works
conversation (one question at a time, stage jumps, start over, reduced motion, stage
links) and the FAQ (topics, search, empty state, `#faq-…` links, the workspace page and
palette, signed-out redirect), the Overview (empty and busy workspaces, pipeline
shares, coverage, tabs with arrow keys and session memory, the activity table and
its plain-language failures, expandable rows, preview
lifetime, both lists without JavaScript), reduced motion and no-JavaScript flows. See [02](02-development-setup.md#browser-regression-checks-optional).

## Decisions and trade-offs

| Decision | Why | Trade-off |
| --- | --- | --- |
| Server-rendered templ + HTMX | One binary, no frontend build service, works without JS. | Complex client state is harder; fine for an operational dashboard. |
| Dedicated pages over a master/detail panel | Bookmarkable URLs, simple history, clear focus; chosen after trying a panel layout. | More navigations, softened by view transitions and prefetch. |
| View transitions + prefetch, not `hx-boost` | Smooth navigation while keeping real document loads and no-JS behaviour. | Effects only in supporting browsers. |
| Command palette as a layer over links | Faster navigation for keyboard users without a second navigation model. | Duplicate entry points to keep in sync. |
| System fonts, no CDNs | Self-hosted, private, fast first paint. | Less typographic control. |
| Starter kits generated on the server from one form | Every kit passes the build's own parser; works without JavaScript; one address per kit. | A round trip per change (small, and replaced if superseded). |
| Starter kit editor as a text area over a highlighted copy | Native typing, selection, undo and screen reader support; no editor library to ship or keep in sync with the CSP. | Highlighting is simple (keys, strings, comments), not a full grammar. |
| SSE invalidation, refetch via GET | No polling; authorization stays in ordinary handlers ([20](20-realtime-updates.md)). | One connection per open page. |
