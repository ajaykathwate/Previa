# Previa documentation

Read these in order. Each builds on the previous ones; the first three are enough to
run and use Previa, the rest explain how and why it works.

| # | Document | Read it to learn |
| --- | --- | --- |
| 01 | [System overview](01-system-overview.md) | What exists today, architecture, code map, key decisions, trade-offs, glossary. |
| 02 | [Development setup](02-development-setup.md) | Configure, migrate, run, test and inspect locally. |
| 03 | [User guide](03-user-guide.md) | Every working screen, from sign-in to build policy, secrets and build logs. |
| 04 | [GitHub sign-in and App installation](04-github-sign-in-and-app.md) | Why two GitHub registrations, OAuth sessions, verified repository linking. |
| 05 | [Webhooks and PR discovery](05-webhooks-and-pr-discovery.md) | Signed ingestion, deduplication, event ordering, discovery, mergeability. |
| 06 | [Source and configuration](06-source-and-configuration.md) | Fresh upstream verification, exact-commit archives, safe extraction, `.previewrc.yaml`. |
| 07 | [Repository secrets](07-repository-secrets.md) | Encrypted values, key rotation, fork withholding, collision rule, redaction. |
| 08 | [Build logs](08-build-logs.md) | Current-attempt logs, epochs, limits, redaction, live streaming and retention. |
| 09 | [Image builds](09-image-builds.md) | Isolated BuildKit builders, the egress proxy, caches, limits and what was verified. |
| 10 | [Preview runtime](10-preview-runtime.md) | Hardened, isolated preview containers: secrets, readiness, capacity, cleanup and what was verified. |
| 11 | [Build policy and build intent](11-build-policy-and-intents.md) | Manual vs Automatic, observed state vs authorized intent, idempotency. |
| 12 | [Build scheduling](12-build-scheduling.md) | Deadlines, wake-ups, atomic admission, handoff, recovery. |
| 13 | [Durable publication design](13-durable-publication-design.md) | The decision record for publishing safely across PostgreSQL, Docker, routing and GitHub. |
| 14 | [Generation ordering](14-generation-ordering.md) | The SQL fences and races that make newer work always win. |
| 15 | [Realtime updates](15-realtime-updates.md) | Live dashboard updates with triggers, LISTEN/NOTIFY and SSE. |
| 16 | [UI architecture](16-ui-architecture.md) | Templ/HTMX structure, theme, navigation, progressive enhancement. |
| 17 | [Roadmap](17-roadmap.md) | The remaining tasks to real previews, and the release checklist. |

Related files outside this folder:

- [README.md](../README.md): product requirements, user flow and technology choices.
- [DESIGN.md](../DESIGN.md): per-feature design and the package-by-feature layout (Section 20).
- [AGENTS.md](../AGENTS.md) and [HANDOFF.md](../HANDOFF.md): working rules and current state for contributors and coding agents.
- [web/tests/browser/README.md](../web/tests/browser/README.md) and [web/components/README.md](../web/components/README.md): browser tests and component provenance.

When behaviour, configuration or decisions change, update the matching document in
the same change. A new document takes its place in the reading order (later numbers
shift up) and a row in this table; keep every relative link resolving.
