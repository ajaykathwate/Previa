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
  repository, routes, and templates live together in its own package.
  Don't reintroduce layered folders (global handlers/, routes/, store/).
- Keep files flat inside a feature package; don't add handler/, service/
  subfolders inside one feature.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`), don't discard.
- Naming: standard Go naming (MixedCaps, no underscores, short receiver
  names, exported only what needs to be exported).

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
- After making changes, run `go build ./...` and existing tests.
  If tests don't exist yet for what you touched, say so rather than
  silently skipping.
- Summarize what you created/changed and why, and flag any deviation
  you made from README.md/DESIGN.md and why.