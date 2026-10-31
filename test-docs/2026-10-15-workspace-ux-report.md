# Workspace usability refinement · 2026-10-15

## Delivered behavior

- Members and Workspace settings are separate destinations in the sidebar,
  switcher and command palette. Workspace selection is no longer a prerequisite
  detour to find members. A workspace is one team with many people/repositories;
  people can belong to multiple workspaces. There are no nested teams/channels.
- The switcher uses an aligned SVG chevron, role capsules, full-name tooltip,
  current-selection indicator and authenticated logos. Its menu stays within the
  viewport and above page content; native details, keyboard and no-JS use remain.
- Members has searchable, paginated Members/Invitations views, real counts,
  repository scope chips, clear Invite/Manage access actions and contextual empty
  states. Role and grants are edited in one modal. Ownership nomination selects
  its target inside the confirmation modal, keeping required-field validation usable.
- Settings contains editable name/description, logo upload/removal, ownership and
  actual workspace information. Developers/Reviewers cannot open Settings.
  Invalid profiles retain form context; stale profiles return 409 with current
  values. Mutations preserve CSRF protection and progressive enhancement.
- Previews is the only preview listing. Reviewers get a read-only presentation of
  the existing scoped query at `/previews`, with no source/build/log controls.
  Authorized legacy `/workspaces/{id}/previews` bookmarks redirect there. Existing
  serving links survive replacement/failure; inaccessible repositories lose links.

## Implementation and setup

Migration 0031 adds description/profile revision and a separate small-logo table.
Profile writes serialize on the workspace and recheck active Owner/Admin status.
They use an independent revision, preserving private-preview authorization and
manual build requests. Logos accept actual PNG/JPEG up to 192 KiB and 512 × 512;
re-encoding removes metadata. Only active members can retrieve the PNG, with
private/no-store responses. No new application dependency, asset service or
configuration is introduced. Shared tokens preserve the signal theme.

Run `make migrate-up`, build, and restart server/worker before using the change;
new binaries require Goose 31 (River 007). Air does not migrate automatically.
The development database was not migrated during this task. Migration 0031 Down
removes profile data; reapplying Up cannot restore removed descriptions/logos.
See [setup](../docs/02-development-setup.md#workspace-profile-and-navigation-update-0031).

README.md, DESIGN.md, setup and workspace flow documentation have been updated.
No architectural deviation or future-feature scaffold was introduced. Native
reviews and account lifecycle remain the existing Features 3/6 boundaries; their
unbuilt controls are omitted from workspace settings.

## Verification

| Check | Result |
| --- | --- |
| Generated assets, templ and sqlc; formatting | Passed |
| `go build ./...`, `go vet ./...` | Passed |
| Pinned `golangci-lint run` | 0 issues |
| Existing full Go suite with PostgreSQL | 1,481 test/subtest passes, 40 opt-in skips, no failures |
| Race detector: workspaces, auth, pullrequests, app | All four packages passed |
| Existing browser suite, production CSP | 55/55 passed |
| Workspace browser suite, PostgreSQL/sessions/CSRF | 7/7 passed, including final layering and logo checks |
| Existing navigation/accessibility after layering fix | 6/6 passed |

New backend tests exercise profile authorization, independent/stale revisions,
manual-request preservation, bounded image decoding/re-encoding, membership-only
logo reads, multipart CSRF/origin rejection, validation context, literal search,
pagination, invitation repository choices and migration Down/Up defaults.
Browser scenarios cover both themes, narrow layouts, switcher alignment/layering,
creation/switching, modal access editing, role controls, reviewer scope, invitation
consent without JavaScript, name/description/logo persistence/removal, stale forms,
no-JS saves and ownership handover. Ignored screenshots are under
`web/tests/browser/artifacts/workspace-*.png`; layouts were visually inspected.

Verification uses a disposable PostgreSQL 17 instance, separate from development.
Docker was unavailable in this WSL session; official packages were extracted under
`/tmp` without system installation. The first database's C locale caused one
existing Unicode-search test to fail; the final database uses C.UTF-8. An initial
broad race run was terminated before completion without a reported test failure;
full ordinary tests and affected-package race tests provide the completed checks.
The temporary instance and extracted packages were stopped/removed after checks.
GitHub identity/permission responses are fixtures. No live GitHub writes, Docker
runtime rebuilds, deployment or production migration was performed.
