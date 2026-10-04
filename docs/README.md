# Previa documentation

Read these in order. Each builds on the previous ones; the first three are enough to
run and use Previa, the rest explain how and why it works.

| # | Document | Read it to learn |
| --- | --- | --- |
| 01 | [System overview](01-system-overview.md) | What exists today, architecture, code map, key decisions, trade-offs, glossary. |
| 02 | [Development setup](02-development-setup.md) | Configure, migrate, run, test and inspect locally. |
| 03 | [User guide](03-user-guide.md) | Every working screen, from sign-in to build policy. |
| 04 | [GitHub sign-in and App installation](04-github-sign-in-and-app.md) | Why two GitHub registrations, OAuth sessions, verified repository linking. |
| 05 | [Webhooks and PR discovery](05-webhooks-and-pr-discovery.md) | Signed ingestion, deduplication, event ordering, discovery, mergeability. |
| 06 | [Repository configuration](06-repository-configuration.md) | The `.previewrc.yaml` contract, resource tiers, validation errors. |
| 07 | [Build policy and build intent](07-build-policy-and-intents.md) | Manual vs Automatic, observed state vs authorized intent, idempotency. |
| 08 | [Build scheduling](08-build-scheduling.md) | Deadlines, wake-ups, atomic admission, handoff, recovery. |
| 09 | [Durable publication design](09-durable-publication-design.md) | The decision record for publishing safely across PostgreSQL, Docker, routing and GitHub. |
| 10 | [Generation ordering](10-generation-ordering.md) | The SQL fences and races that make newer work always win. |
| 11 | [Realtime updates](11-realtime-updates.md) | Live dashboard updates with triggers, LISTEN/NOTIFY and SSE. |
| 12 | [UI architecture](12-ui-architecture.md) | Templ/HTMX structure, theme, navigation, progressive enhancement. |
| 13 | [Roadmap](13-roadmap.md) | The 12 remaining tasks to real previews, and the release checklist. |

Related files outside this folder:

- [README.md](../README.md): product requirements, user flow and technology choices.
- [DESIGN.md](../DESIGN.md): per-feature design and the package-by-feature layout (Section 20).
- [AGENTS.md](../AGENTS.md) and [HANDOFF.md](../HANDOFF.md): working rules and current state for contributors and coding agents.
- [web/tests/browser/README.md](../web/tests/browser/README.md) and [web/components/README.md](../web/components/README.md): browser tests and component provenance.

When behaviour, configuration or decisions change, update the matching document in
the same change. New documents take the next number and a row in this table.
