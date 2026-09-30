# Project handoff

Last updated: 2026-09-29

## Project

Previa is a self-hosted service intended to build isolated preview environments
for GitHub pull requests. Requirements and selected stack are in `README.md`;
feature behavior, edge cases, and package structure are in `DESIGN.md`.

## Start here

- `README.md` — requirements and technology choices.
- `DESIGN.md` — implementation design; Section 20 describes package layout.
- `DEVELOPMENT_SETUP.md` — local prerequisites, PostgreSQL, migrations, and Go commands.
- `Makefile`, `.env.example`, `deploy/docker-compose.yml` — actual local commands,
  configuration keys, and PostgreSQL service definition.

Read only the sections relevant to the current task. Check `git status` first;
this handoff is a navigation aid, not a substitute for checking the working tree.

## Repository conventions

- Go project; package by feature, shared infrastructure under
  `internal/platform/` (see `DESIGN.md` Section 20).
- Keep scope to the requested feature and follow README/DESIGN as source of truth.
- Go tool dependencies are pinned in `tools/go.mod`; Make targets wrap common
  build, test, lint, generate, run, and migration commands.
- Keep secrets in ignored `.env` or external key files; never commit them.

## Current development setup

- PostgreSQL 17 is provided by `deploy/docker-compose.yml`; the default local
  connection is in `.env.example`.
- Run `make migrate-up` to apply pending migrations.
- Run `make build`, `make test`, and `make lint` for standard Go checks.
- Set `TEST_DATABASE_URL` to the local database URL to run database integration
  tests rather than having them skip.
- Follow `DEVELOPMENT_SETUP.md` for full setup, manual command equivalents,
  startup, shutdown, and troubleshooting.

## Session notes

- No active implementation task is recorded here. Treat the user's latest
  request as authoritative and verify the current code before making assumptions.
- When a task changes project state, decisions, verification results, or next
  steps, refresh this file before handing off. Keep it brief and under 100 lines.
