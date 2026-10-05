# 25 · Release report: v0.1.0-rc.1

The release acceptance checklist from [26](26-roadmap.md#release-acceptance-checklist),
answered item by item with the evidence gathered on 2026-10-05 for the release
candidate `v0.1.0-rc.1`, built from this repository. Every check below ran on a
developer workstation with disposable resources; nothing was deployed, published or
written to GitHub. **Verdict: ready for a staging deployment; not yet
production-ready.** Production readiness is conditional on the pending live checks
at the end, which need a host, a domain, a DNS token and an authorized GitHub
repository that were not available.

## Environment

| Component | Version |
| --- | --- |
| Host | WSL 2 (Linux 6.18) on Windows, Docker Desktop: Engine 29.8.1, Compose 5.5.1 |
| Go | 1.26.8 (`go.mod` minimum; release images build with `golang:1.26.8-bookworm`, pinned by digest) |
| PostgreSQL | 17.11 (`postgres:17.11-alpine`, pinned) |
| Router | Traefik v3.7.13 (pinned); Pebble for certificate issuance tests |
| Builds | rootless BuildKit v0.33.1 (pinned) |
| Observability | Prometheus v3.5.0, Grafana 12.1.1 (pinned) |
| Browser tests | Playwright 1.58.2 with Chromium |
| Images | `previa-server` 123 MB, `previa-worker` 130 MB, `previa-egress` 10 MB (distroless, UID 65532) |

## Commands and results

| Command | Result |
| --- | --- |
| `make build`, `make lint` | Pass; golangci-lint 0 issues. |
| `make vuln` | 0 reachable vulnerabilities (after Go 1.26.8 and containerd 2.3.6); one unreachable advisory (`x/crypto/openpgp`, no fix, not imported). |
| `go test ./...` on a throwaway database migrated to Goose 18 | 31 packages pass, 0 fail. |
| `go test -race` on every package, throwaway database at Goose 18 | Pass, no races. |
| `make test-builder`, `test-runtime`, `test-routing` (7/7), `test-housekeeping` | Pass. |
| `make test-e2e` | Pass: publish, failed rebuild keeps the old preview, a GitHub reporting outage (6 calls rejected; the preview published and the report caught up), owner teardown, an Automatic burst, close; nothing left behind. |
| `make test-browser` | 30/30 under the production Content-Security-Policy. |
| `make observability-check` | 24 alert rules valid, their unit tests pass, Prometheus configuration valid, 28 dashboard queries parse. |
| `make release-images`; a clean `--no-cache` rebuild | Built; the rebuilt binaries are bit-for-bit identical. |
| `make prod-smoke` with an older-schema dump | 43/43 checks ([24](24-production-deployment.md#local-staging-smoke-test)). |
| shellcheck on `deploy/`; actionlint on the workflows; `goreleaser check` and a snapshot build | Pass. |

## Checklist

| Item | Evidence | Status |
| --- | --- | --- |
| Fresh setup, clean migration and startup from documented commands; real readiness and capability; fail closed on incomplete config | `make prod-smoke`: the production Compose stack becomes healthy in about 20 s, migrations apply once, `/readyz` reports schema, version and capabilities; `check-config` exits 1 on a missing setting; processes refuse an older schema; `setup.sh` passes shellcheck. | Passed locally; **`setup.sh` on a real host pending.** |
| Real hosted OAuth/App installation and signed GitHub delivery; open-only discovery; close/merge keeps the row; conflicts cannot execute | Local signed fixtures, a real local Git conflict and the discovery, mergeability and lifecycle tests ([05](05-webhooks-and-pr-discovery.md)). | **Pending: hosted GitHub.** |
| Manual builds the freshly verified SHA; pushes alone do not; Automatic bursts deploy the latest after five quiet minutes; Retry/Rebuild fence old work; duplicates coalesce | `make test-e2e` and the intent, scheduling and action tests ([21](21-mvp-operations.md#what-is-proven-and-by-which-test)). | Passed with a fake GitHub; **real repository pending.** |
| A real Dockerfile app builds, becomes healthy, serves stable valid HTTPS, streams redacted logs, one living comment and a valid check; secrets for trusted sources, none for forks | `make test-e2e` (real Docker, BuildKit, Traefik, fake GitHub over TLS, Traefik's default certificate); `make test-routing` (Pebble-issued certificates); `make test-runtime` (fork withholding). | Passed locally; **Let's Encrypt and a real PR pending.** |
| Successful rebuild swaps without an avoidable gap; failed rebuild keeps the old deployment; stale operations and crashes cannot leave an obsolete final state | `make test-routing`, `make test-e2e`, the ordering, publication and recovery tests. | Passed. |
| Close, owner teardown, expiry and revocation remove routes and resources durably; reopen follows policy; no recreation loop or orphan; unrelated resources survive | `make test-routing`, `make test-housekeeping`, `make test-e2e` (close and teardown leave no container, image or network). | Passed. |
| Build and runtime probes cannot reach host, control plane, PostgreSQL, metadata or other previews; limits and capacity races cannot exhaust the host; previews cannot receive dashboard cookies or credentials | `make test-builder`, `make test-runtime` (network probes, limits, atomic admission race); host-only `__Host-` cookies; separate preview domain. | Passed locally; **capacity on the target host's resources pending.** |
| Browser checks: owner isolation, CSRF, modals, keyboard and mobile navigation, logs reconnect/reset, empty and failure states, no-JavaScript flows | `make test-browser` 30/30 under the CSP; `TestActionsRefuseAnotherOwnersPullRequest`. | Passed. |
| GitHub, proxy and database outages, disk pressure and abrupt worker restarts recover without losing intent or cleanup; operators see and act on failures | e2e GitHub outage; router restarts (`make test-routing`); disk-pressure and recovery tests; graceful stop and restart in the smoke test; 24 alerts with a runbook ([23](23-incident-runbook.md)). | Passed locally. |
| Production images, Compose and CI validate; wildcard TLS verified; backup/restore and upgrade/rollback rehearsed on fresh disposable infrastructure | Images built and reproducible; Compose validated and run by the smoke test; actionlint and `goreleaser check`; the smoke test's backup (encrypted, about 1 s), restore (21 s, with the data-loss window shown), upgrade and rollback, and a migration-16 database restored and upgraded to 18 (21 s). | Passed locally; **wildcard TLS from Let's Encrypt, the workflows on GitHub and a rehearsal on a real host pending.** |

## Outstanding inputs and pending gates

1. A host (Debian 12 / Ubuntu 24.04) to run `setup.sh`, the production stack, the
   backup timer with a real object-storage remote, and a restore and upgrade there.
2. A dashboard domain and a preview domain with wildcard DNS, and a DNS provider
   token, for Let's Encrypt staging and then production certificates.
3. Hosted GitHub: the OAuth App and GitHub App registered for the production host,
   webhook delivery, discovery, a real repository built end to end, and
   `make test-source` / `make test-reporting` on an authorized disposable PR.
4. The repository on GitHub: the CI workflows running, a protected `release`
   environment with reviewers, and a tag for the first published release.
5. An Alertmanager and a receiver for alert delivery.
6. Optional: gVisor (`PREVIEW_OCI_RUNTIME=runsc`) if the threat model requires it.

None of these was simulated or claimed. Repeat this report's commands on the
target host and record the results before calling a release production-ready.
