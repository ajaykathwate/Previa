# Previa documentation

Read these in order. Each builds on the previous ones; the first two, plus the
[user guide](../technical-docs/user-guide.md), are enough to run and use Previa. The
rest explain how and why it works.

| # | Document | Read it to learn |
| --- | --- | --- |
| 01 | [System overview](01-system-overview.md) | What exists today, architecture, key decisions and trade-offs, hard problems, and where the inventories (code, schema, stack, glossary) live. |
| 02 | [Development setup](02-development-setup.md) | Configure, migrate, run, test and inspect locally. |
| 04 | [GitHub sign-in and App installation](04-github-sign-in-and-app.md) | Why two GitHub registrations, OAuth sessions, verified repository linking. |
| 05 | [Webhooks and PR discovery](05-webhooks-and-pr-discovery.md) | Signed ingestion, deduplication, event ordering, discovery, mergeability. |
| 06 | [Source and configuration](06-source-and-configuration.md) | Fresh upstream verification, exact-commit archives, safe extraction, `.previewrc.yaml`. |
| 07 | [Repository secrets](07-repository-secrets.md) | Encrypted values, key rotation, fork withholding, collision rule, redaction. |
| 08 | [Build logs](08-build-logs.md) | Current-attempt logs, epochs, limits, redaction, live streaming and retention. |
| 09 | [Image builds](09-image-builds.md) | Isolated BuildKit builders, the egress proxy, caches, limits and what was verified. |
| 10 | [Preview runtime](10-preview-runtime.md) | Hardened, isolated preview containers: secrets, readiness, capacity, cleanup and what was verified. |
| 11 | [Backend previews](11-backend-previews.md) | Managed PostgreSQL and Redis per candidate, setup command, processes, docs path, and owner-approved outbound connections: contract, isolation, lifecycle and limits. |
| 12 | [Routing and HTTPS](12-routing-and-https.md) | Stable preview hostnames, the fenced route controller, Traefik, wildcard certificates, origins and convergence limits. |
| 13 | [Teardown and cleanup](13-teardown-and-cleanup.md) | Close, revocation and local teardown; route removal; ownership-verified cleanup that survives crashes and revoked access. |
| 14 | [PR feedback](14-pr-feedback.md) | The living PR comment and exact-commit check runs: states, identity, ordering limits, failures, permissions and the live proof. |
| 15 | [Housekeeping, expiry and capacity](15-housekeeping-and-capacity.md) | Recovery jobs, expiry, orphan cleanup with grace periods, PR reconciliation, pruning, build and preview capacity, dry runs. |
| 16 | [Build policy and build intent](16-build-policy-and-intents.md) | Manual vs Automatic, observed state vs authorized intent, idempotency. |
| 17 | [Build scheduling](17-build-scheduling.md) | Deadlines, wake-ups, atomic admission, handoff, recovery. |
| 19 | [Generation ordering](19-generation-ordering.md) | The durable publication decision, and the SQL fences and races that make newer work always win. |
| 20 | [Realtime updates](20-realtime-updates.md) | Live dashboard updates with triggers, LISTEN/NOTIFY and SSE. |
| 21 | [UI architecture](21-ui-architecture.md) | Templ/HTMX structure, theme, navigation, progressive enhancement. |
| 22 | [Operating the MVP](22-mvp-operations.md) | What server and worker do, the capability and configuration matrix, queues and draining, running the full pipeline, what is proven and the live proofs still required. |
| 23 | [Observability and hardening](23-observability-and-hardening.md) | Logs, metrics, readiness, dashboards and alerts; ingress controls, production mode, the kill switch and dependency checks. |
| 24 | [Incident runbook](24-incident-runbook.md) | What to do for each alert: outages, backlogs, failures, disk, backups, certificates, key rotation. |
| 25 | [Production deployment](25-production-deployment.md) | Release images, host setup, GitHub and DNS, staged certificates, backups and restore, upgrades and rollback, CI and releases, the smoke test. |
| 26 | [Release report](26-release-report.md) | Evidence for each release acceptance item: what passed, how, and what is still pending. |
| 27 | [Roadmap](27-roadmap.md) | Prompts 1–13 (13: backend previews), the planned Prompt 14 (missed uninstall detection), and the release checklist. |

Related files outside this folder:

- [README.md](../README.md): product requirements, user flow and technology choices.
- [technical-docs/](../technical-docs/README.md): go-to references: the tech stack with every URL and dashboard, the end-user guide with a manual test checklist, and Mermaid class and database diagrams.
- [DESIGN.md](../DESIGN.md): per-feature design and the package-by-feature layout (Section 20).
- [AGENTS.md](../AGENTS.md) and [HANDOFF.md](../HANDOFF.md): working rules and current state for contributors and coding agents.
- [test-docs/](../test-docs/README.md): the regression test specification, 242 cases by module with the evidence for each and the latest cycle's results.
- [web/tests/browser/README.md](../web/tests/browser/README.md) and [web/components/README.md](../web/components/README.md): browser tests and component provenance.

When behaviour, configuration or decisions change, update the matching document in
the same change, and keep every relative link resolving. A new document takes the
next unused number and its row in this table; the table order is the reading order.
Numbers are permanent: migrations, code comments, scripts, workflows and other
documents link to these files by name, so a number is never changed or reused.
Numbers 03 and 18 are retired: the user guide moved to
[technical-docs/user-guide.md](../technical-docs/user-guide.md) and the publication
decision into [19](19-generation-ordering.md).
