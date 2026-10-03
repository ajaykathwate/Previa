# Component provenance

Button, card, alert, and input source is copied from **templUI v1.13.2**:
https://github.com/axadrn/shadcn-templ/tree/v1.13.2/components
The upstream module name is `github.com/templui/templui`.
License: [LICENSE.templui](LICENSE.templui) (MIT).

- These are community shadcn-style Go/Templ components, not official React
  shadcn/ui or the shadcn-templ v2 beta.
- Imports point to local component helpers. `utils` contains only the upstream
  helpers needed by these components; class merging uses pinned
  `github.com/Oudwins/tailwind-merge-go v0.2.0`.
- Input retains native input markup and styling. Its optional password-toggle
  feature, icons, and script loader are omitted because no current page uses them.
  Password inputs therefore behave as native inputs without a visibility toggle.
  Upstream input focus-ring classes are omitted; all text fields use the shared
  quiet forest-border focus style in `web/styles/base.css`. Invalid borders retain
  their error colour. Button/radio keyboard indicators remain separate.
- `navbar.templ`, `app_navbar.templ`, and `sidebar.templ` are application-specific
  composition, not copied library code. `confirmation.templ` is an application-owned
  native dialog shell and logout form; it reuses the local button primitive. Native details/summary menus are enhanced
  by `web/static/js/ui.js` for Escape and outside-click dismissal.
- Theme tokens in `web/styles/theme.css` are adapted from the same upstream
  release. Official Tailwind CSS v4.1.18 compiles the CSS independently.

Copied `.templ` source is committed; generated `*_templ.go` stays beside it and
is ignored. Updates are explicit source reviews against a pinned upstream tag,
including local changes. Do not overwrite customized components automatically.
There is no assumption of ongoing v1 support or automatic v2 compatibility.
