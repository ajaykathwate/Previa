# Presentation architecture

The server renders Templ pages and serves embedded assets. HTMX is loaded locally;
existing forms retain their ordinary HTTP behavior. There is no React application,
Node.js dependency, or frontend service.

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

The root `web` package stays focused on assets; it does not aggregate page packages.
The public home page is in `web/pages/home`; authenticated dashboard/session pages
are in `web/pages/dashboard`, and auth error pages are in `web/pages/auth`.
Successful OAuth redirects to `/dashboard`; logout returns to `/`. No future
repository, build, or environment screens are scaffolded.

`layouts.Document` owns the HTML document and local asset references. `layouts.Base`
adds public navigation for standalone pages; `layouts.App` composes the shared
navbar, sidebar, mobile workspace navigation, and authenticated content slot.
Authentication remains in handlers/middleware, never in templates.

`web/styles/site.css` defines the landing-page and application-shell composition,
responsive layouts, hover transforms, and CSS entrance/ambient animations. Reduced
motion disables animations and transforms. Decorative workflow illustrations are
explicitly labeled as concepts; dashboard identity and queue results use actual
server values. No invented metrics or activity are displayed.

`web/static/js/ui.js` progressively enhances native account menus with outside-click
and Escape dismissal and the existing queue form with inline pending/success/error
feedback. Logout remains a normal CSRF-protected POST. The script adds no application
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
clearly labeled upcoming sections on the existing dashboard; no future routes or
backend features are created. Repository connection is visibly unavailable. The
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
