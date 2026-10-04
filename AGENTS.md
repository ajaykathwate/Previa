# Agent instructions for this repository

## Source of truth
- README.md: requirements and tech stack.
- DESIGN.md: per-feature design (requirement, complexity, stack, implementation,
  edge cases) and the folder structure (Section 20).
Follow these documents. Do not introduce architecture, libraries, or folder
structures that contradict them without flagging the conflict first.

## Scope discipline
Only build what the current task describes. Do not preemptively scaffold
features from later phases. If a task's DESIGN.md section references another
unbuilt feature, stub it behind a minimal interface rather than building it.

## Go conventions
- Follow standard Go idioms and `gofmt`/`goimports` formatting; run
  `go vet` and `golangci-lint run` before considering a task done.
- Package by feature per DESIGN.md Section 20: a feature's handler, service,
  repository, routes, and jobs live together in its own package.
  Presentation is the exception: templates live in `web/pages/<feature>`, shared
  shells in `web/layouts`, components in `web/components`, CSS source in
  `web/styles`, and served assets in `web/static`. Presentation must not import
  backend features; handlers pass display data. Do not reintroduce global
  handlers/, routes/, or store/ layers.
- Keep files flat inside a feature package; don't add handler/, service/
  subfolders inside one feature.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`), don't discard.
- Naming: standard Go naming (MixedCaps, no underscores, short receiver
  names, exported only what needs to be exported).

## UI consistency
- Preserve the approved "signal" theme: graphite neutrals, lime signal accent,
  semantic status tones, light/dark palettes in `web/styles/theme.css`, and
  system-sans UI with monospace for commits/branches/labels. Use shared theme
  tokens and presentation patterns; ask before changing the visual theme.
- Confirmation prompts must open as accessible modals over the current screen,
  not navigate to a separate confirmation page. Keep mutations CSRF-protected.
- Label unbuilt features clearly. Do not invent metrics/activity or use dead links.

## Design principles
- Apply SOLID and standard design patterns only where they solve a real
  problem this codebase has. Do not add a pattern (factory, strategy,
  builder, etc.) speculatively "for flexibility" if there's one concrete
  implementation and no evidence a second is coming.
- No god classes/files: if a file is doing HTTP handling, business logic,
  and data access all at once, split it along those lines per the feature
  layout in DESIGN.md.
- Prefer small, consumer-defined interfaces over large ones. A feature
  should define the interface it needs from another feature, not import
  that feature's concrete types.

## Every task
- When configuration, prerequisites, setup commands, migrations, development
  tooling, or startup/verification behavior changes, update
  `docs/DEVELOPMENT_SETUP.md` in the same task. Keep instructions consistent with
  `.env.example`, config loaders, Makefile, and Air; include commands and expected
  results, and distinguish automatic env loading from manual exports.
- After making changes, run `go build ./...` and existing tests.
  If tests don't exist yet for what you touched, say so rather than
  silently skipping.
- Summarize what you created/changed and why, and flag any deviation
  you made from README.md/DESIGN.md and why.

## Session continuity
- At the start of a session, read `HANDOFF.md` and check `git status`.
- Use the handoff to locate relevant files. Read only the code and the
  README.md/DESIGN.md sections needed for the requested task.
- Verify handoff details against the current files before acting; code and
  the project source-of-truth documents take precedence.
- Before ending a task, update `HANDOFF.md` when project state, decisions,
  verification results, or next steps have changed.
- Keep `HANDOFF.md` under 100 lines. Replace stale details rather than
  appending a conversation log. Never put credentials or secrets in it.
