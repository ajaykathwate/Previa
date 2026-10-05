# Previa documentation

Read these in order. Each builds on the previous ones; the first three are enough to
run and use Previa, the rest explain how and why it works.

| # | Document | Read it to learn |
| --- | --- | --- |
| 01 | [System overview](01-system-overview.md) | What exists today, architecture, code map, key decisions, trade-offs, glossary. |
| 02 | [Development setup](02-development-setup.md) | Configure, migrate, run, test and inspect locally. |
| 03 | [User guide](03-user-guide.md) | Every working screen, from sign-in to build policy, building and tearing down previews, secrets and build logs. |
| 04 | [GitHub sign-in and App installation](04-github-sign-in-and-app.md) | Why two GitHub registrations, OAuth sessions, verified repository linking. |
| 05 | [Webhooks and PR discovery](05-webhooks-and-pr-discovery.md) | Signed ingestion, deduplication, event ordering, discovery, mergeability. |
| 06 | [Source and configuration](06-source-and-configuration.md) | Fresh upstream verification, exact-commit archives, safe extraction, `.previewrc.yaml`. |
| 07 | [Repository secrets](07-repository-secrets.md) | Encrypted values, key rotation, fork withholding, collision rule, redaction. |
| 08 | [Build logs](08-build-logs.md) | Current-attempt logs, epochs, limits, redaction, live streaming and retention. |
| 09 | [Image builds](09-image-builds.md) | Isolated BuildKit builders, the egress proxy, caches, limits and what was verified. |
| 10 | [Preview runtime](10-preview-runtime.md) | Hardened, isolated preview containers: secrets, readiness, capacity, cleanup and what was verified. |
| 11 | [Routing and HTTPS](11-routing-and-https.md) | Stable preview hostnames, the fenced route controller, Traefik, wildcard certificates, origins and convergence limits. |
| 12 | [Teardown and cleanup](12-teardown-and-cleanup.md) | Close, revocation and local teardown; route removal; ownership-verified cleanup that survives crashes and revoked access. |
| 13 | [PR feedback](13-pr-feedback.md) | The living PR comment and exact-commit check runs: states, identity, ordering limits, failures, permissions and the live proof. |
| 14 | [Housekeeping, expiry and capacity](14-housekeeping-and-capacity.md) | Recovery jobs, expiry, orphan cleanup with grace periods, PR reconciliation, pruning, build and preview capacity, dry runs. |
| 15 | [Build policy and build intent](15-build-policy-and-intents.md) | Manual vs Automatic, observed state vs authorized intent, idempotency. |
| 16 | [Build scheduling](16-build-scheduling.md) | Deadlines, wake-ups, atomic admission, handoff, recovery. |
| 17 | [Durable publication design](17-durable-publication-design.md) | The decision record for publishing safely across PostgreSQL, Docker, routing and GitHub. |
| 18 | [Generation ordering](18-generation-ordering.md) | The SQL fences and races that make newer work always win. |
| 19 | [Realtime updates](19-realtime-updates.md) | Live dashboard updates with triggers, LISTEN/NOTIFY and SSE. |
| 20 | [UI architecture](20-ui-architecture.md) | Templ/HTMX structure, theme, navigation, progressive enhancement. |
| 21 | [Operating the MVP](21-mvp-operations.md) | What server and worker do, the capability and configuration matrix, queues and draining, running the full pipeline, what is proven and the live proofs still required. |
| 22 | [Observability and hardening](22-observability-and-hardening.md) | Logs, metrics, readiness, dashboards and alerts; ingress controls, production mode, the kill switch and dependency checks. |
| 23 | [Incident runbook](23-incident-runbook.md) | What to do for each alert: outages, backlogs, failures, disk, backups, certificates, key rotation. |
| 24 | [Production deployment](24-production-deployment.md) | Release images, host setup, GitHub and DNS, staged certificates, backups and restore, upgrades and rollback, CI and releases, the smoke test. |
| 25 | [Release report](25-release-report.md) | Evidence for each release acceptance item: what passed, how, and what is still pending. |
| 26 | [Roadmap](26-roadmap.md) | Prompts 1–12 and the release checklist. |

Related files outside this folder:

- [README.md](../README.md): product requirements, user flow and technology choices.
- [technical-docs/](../technical-docs/README.md): go-to references: the tech stack with every URL and dashboard, the end-user guide with a manual test checklist, and Mermaid class and database diagrams.
- [DESIGN.md](../DESIGN.md): per-feature design and the package-by-feature layout (Section 20).
- [AGENTS.md](../AGENTS.md) and [HANDOFF.md](../HANDOFF.md): working rules and current state for contributors and coding agents.
- [web/tests/browser/README.md](../web/tests/browser/README.md) and [web/components/README.md](../web/components/README.md): browser tests and component provenance.

When behaviour, configuration or decisions change, update the matching document in
the same change. A new document takes its place in the reading order (later numbers
shift up) and a row in this table; keep every relative link resolving.
