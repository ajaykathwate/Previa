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
Only existing auth error/logout and dashboard home/protected pages are implemented.
Successful OAuth login/callback remain redirects. No future screens or interactive
features are scaffolded.

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
2. `make generate` runs pinned Templ and sqlc generation from the repository root.
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
