# Technical docs

Go-to references for Previa, each complete on its own. The numbered documents in
[docs/](../docs/README.md) explain how and why every part works; these pages answer
"what is it, where is it, and how do I use it" in one place.

| Document | Read it to learn |
| --- | --- |
| [tech-stack.md](tech-stack.md) | Every technology and version, what it is used for, where it lives in the code, processes and containers, every URL and built-in dashboard, configuration and infrastructure files, deployment setups, tests and admin commands. |
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
(production): every setting with a placeholder, its purpose and how to get it.
