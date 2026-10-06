# Technical docs

Go-to references for Previa, each complete on its own. The numbered documents in
[docs/](../docs/README.md) explain how and why every part works; these pages answer
"what is it, where is it, and how do I use it" in one place.

| Document | Read it to learn |
| --- | --- |
| [inside-previa.md](inside-previa.md) | *Inside Previa: From Pull Request to Live Preview*, the whole flow in one book: from a Build click or a push to a live HTTPS preview, step by step, with every network call, container, security control, library and timeout, 35 Mermaid diagrams and outside references. |
| [tech-stack.md](tech-stack.md) | Every technology and version, what it is used for, where it lives in the code, processes and containers, every URL and built-in dashboard, configuration and infrastructure files, deployment setups, tests and admin commands. |
| [config-setup.md](config-setup.md) | Every configuration setting: Required or Optional, what it is for, where the app uses it, and step by step how to get its value (accounts, pages, commands). |
| [ci-in-action.md](ci-in-action.md) | The CI workflow block by block: events, GitHub runners, permissions, temporary PostgreSQL, generation, Go/browser/Docker tests, image scans, logs and current validation limits. |
| [cd-in-action.md](cd-in-action.md) | The Release workflow, GoReleaser and Dockerfile block by block, followed by artifact publication, manual DigitalOcean deployment, secrets, backups, rollback and automation boundaries. |
| [user-guide.md](user-guide.md) | What Previa offers, every user flow step by step from sign-in to a live preview and its removal, what each message means, and a manual test checklist with edge cases. |
| [class-diagram.md](class-diagram.md) | Mermaid class diagrams: every package, the worker's build pipeline, and the server's request handling. |
| [database-schema-diagram.md](database-schema-diagram.md) | A Mermaid entity-relationship diagram of the whole database, with notes on key columns. |
| [database-schema.md](database-schema.md) | Every table, column, constraint, index and trigger, with its purpose. |

Every diagram is Mermaid code: copy a ```` ```mermaid ```` block (from the first
`---` to the last line) into the **Code** panel of [mermaid.live](https://mermaid.live)
to view, zoom and rearrange it. GitHub and VS Code with a Mermaid extension also
render the blocks in place.

Configuration lives in [.env.example](../.env.example) (local) and
[deploy/production/previa.env.example](../deploy/production/previa.env.example)
(production); [config-setup.md](config-setup.md) explains every setting in full.
