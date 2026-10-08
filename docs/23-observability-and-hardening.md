# 23 · Observability and production hardening

How an operator sees what Previa is doing and how it fails, and what protects the
dashboard and its processes in production: structured, correlated, redacted logs;
bounded Prometheus metrics; liveness and readiness on an internal listener; a
Grafana dashboard and alert rules; ingress controls, security headers and
production mode; the execution kill switch; and dependency checks. What to do when
an alert fires is in [24](24-incident-runbook.md); how it all runs on a host is in
[25](25-production-deployment.md).

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Where metrics and health are served | A separate **internal listener** per process (`INTERNAL_ADDR`, `WORKER_INTERNAL_ADDR`), never the public router | Metrics and readiness reveal internals; on a host they stay on the Compose control network and are never published. |
| Who exports durable state (queue depth, environments, operations, workers) | The **server**, read from PostgreSQL at scrape time (at most every 10 s, 3 s timeout) | The server keeps running when no worker does, which is exactly when queue depth matters; one exporter means no double counting. |
| Metric labels | Only fixed sets or code-defined names; anything else becomes `other` | PR IDs, URLs, hostnames and secrets never become label values, so cardinality stays bounded whatever users do. |
| Headline number | `previa_request_to_preview_seconds`: from a request becoming eligible (after Automatic's quiet period) to its preview serving | DESIGN §18 names webhook-to-running-preview latency the headline; the deliberate quiet period is not latency. |
| Redaction | At the logging layer, by key (`secret`, `token`, `password`, `authorization`, `cookie`, `private_key`, `credential`, `dsn`, …) and by shape (GitHub tokens, bearer/basic credentials, PEM keys, URL passwords) in messages, strings and errors | A last line of defence; code still never logs secrets. |
| Content-Security-Policy | A per-request nonce; no `unsafe-inline`, no `unsafe-eval`; HTMX configured not to inject styles or evaluate code | The one inline script (theme boot) carries the nonce; injected speculation rules reuse it. |
| Production mode | `APP_ENV=production` turns on checks a developer machine cannot always meet | WSL's `/mnt/c` reports every file as world-readable; production must not accept that. |
| Kill switch | `BUILDS_PAUSED=true` on the worker: no build starts, everything else keeps running | An incident must be able to stop untrusted code without stranding previews or cleanup. |

## Logs

JSON lines on standard output at `LOG_LEVEL` (`debug`, `info` default, `warn`,
`error`). Every record made with a request's or job's context carries its
correlation fields:

| Where | Fields |
| --- | --- |
| HTTP requests | `request_id` (also the `X-Request-ID` response header), `client_ip` (from trusted proxies only), `method`, `path`, `status`, `duration` |
| Webhook deliveries | `delivery_id`, `event`, then the repository, PR number and installation |
| River jobs | `job_id`, `job_kind`, `job_attempt`, and when the job's arguments name them `pr_id`, `generation`, `revision`, `sha`, `operation_id`, `repo_id` |
| Processes | the release `version`, schema version and `environment` at start; the worker's capabilities and every unavailable reason |

`docker compose logs worker | grep '"pr_id":42'` follows one pull request through
discovery, admission, build, publication and reports.

## Metrics

All on `GET /metrics` of the internal listeners. The Go runtime and process
collectors are included.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `previa_build_info` | gauge | `version`, `go_version` | Always 1: which release runs. |
| `previa_request_to_preview_seconds` | histogram | | The headline latency (see Decisions). |
| `previa_builds_total` | counter | `outcome` (succeeded, failed, superseded, interrupted), `code` (failure category) | Finished build attempts. |
| `previa_build_duration_seconds` | histogram | `outcome` | Source to healthy candidate. |
| `previa_build_waits_total` | counter | `reason` (capacity, disk, paused) | Admissions deferred. |
| `previa_build_disk_free_bytes` | gauge | | Free Docker host storage at the last build's disk check (the builder measures its own state directory, the path that actually fills). |
| `previa_jobs_total` | counter | `kind`, `outcome` (succeeded, failed, snoozed, cancelled) | Every River job worked. |
| `previa_job_duration_seconds` | histogram | `kind` | Job run time. |
| `previa_job_queue_wait_seconds` | histogram | `queue` | From a job becoming due to a worker starting it. |
| `previa_egress_connections_total` | counter | `outcome` (`forwarded`, `unapproved`, `refused_address`, `unresolvable`, `dial_failed`, `limit`, `revoked`, `byte_limit`) | Owner-approved outbound connections the egress proxy forwarded or refused, since it started (worker; read from the proxy every 15 s). |
| `previa_egress_bytes_total` | counter | `direction` (`up`, `down`) | Bytes forwarded to approved destinations and back. |
| `previa_housekeeping_actions_total` | counter | `job`, `action` | Recovery wake-ups, orphans scheduled for cleanup, unknown PRs' objects removed, missing previews, confirmed PRs, expired previews, pruned images/workspaces/caches. Dry runs count no deletions. |
| `previa_github_requests_total` | counter | `status` (2xx…5xx, error) | GitHub API calls through the App. |
| `previa_github_rate_limit_remaining` | gauge | `resource` | GitHub's rate-limit headroom. |
| `previa_github_rate_limited_total` | counter | | Responses that reported a rate limit. |
| `previa_webhook_deliveries_total` | counter | `event`, `outcome` (accepted, ignored, invalid_signature, invalid, conflict, failed, too_large, overloaded, disabled) | Webhook requests. |
| `previa_http_requests_total`, `previa_http_request_duration_seconds` | counter, histogram | `route` (the router pattern), `method`, `status` | Dashboard traffic; event streams are excluded from the duration. |
| `previa_ingress_rejections_total` | counter | `reason` (rate_limited, wrong_host, forwarded_host, body_too_large, overloaded) | Requests refused before a handler. |
| `previa_sse_streams` | gauge | | Open live-update and log streams. |
| `previa_queue_jobs`, `previa_queue_oldest_available_seconds` | gauge | `queue`, `state` | Queue depth and the age of the oldest due job (server). |
| `previa_environments`, `previa_operations`, `previa_build_requests` | gauge | status; kind and state; state and waiting reason | Durable state (server). Failed operations count for a day. |
| `previa_workers_live`, `previa_worker_capability` | gauge | `capability` | Workers with a fresh heartbeat and what they provide, including `paused` (server). |
| `previa_backup_last_success_timestamp_seconds` | gauge | | Modification time of `BACKUP_STATUS_FILE` (server). |
| `previa_state_refresh_failures_total` | counter | | Scrape-time reads that failed (the last good snapshot is kept). |

## Health, readiness and capabilities

| Endpoint | Server (`INTERNAL_ADDR`, default `127.0.0.1:9090`) | Worker (`WORKER_INTERNAL_ADDR`, default `127.0.0.1:9091`) |
| --- | --- | --- |
| `GET /livez` | 200 while the process runs | Same |
| `GET /readyz` | database ping and schema at least the build's migration (19 now); adds `version` | database, schema, River running, heartbeat recorded within 90 s, Docker reachable (when it builds); adds `capabilities` and `version` |
| `GET /metrics` | Prometheus | Prometheus |

`/readyz` answers 503 with each failing check's (redacted) reason. Both binaries
have two subcommands for images without a shell: `previa-server healthcheck` /
`previa-worker healthcheck` (the container health check: exit 0 when `/readyz`
is ready) and `check-config` (validate the environment and exit; a deploy gate).
Both processes refuse to start against a schema older than they were built for.

## Dashboards and alerts

`deploy/observability/` holds `prometheus.yml` (scrapes the server, the worker and
the router's metrics entry point on the control network), `alerts.yml` (24 rules,
each naming its runbook section), `alerts_test.yml` (promtool unit tests) and a
provisioned Grafana dashboard (30 panels: headline latency, builds, queues,
requests, operations, housekeeping, disk, GitHub, webhooks, HTTP, streams, backup
age and certificate expiry). `make observability-check` validates the rules,
runs their tests, checks the Prometheus configuration and parses every dashboard
query with the pinned promtool. The production Compose file runs both under the
`observability` profile; delivering alerts needs an Alertmanager the operator
configures ([25](25-production-deployment.md)).

| Alert | Fires when |
| --- | --- |
| PreviaServerDown / PreviaWorkerDown | The server's metrics are unreachable for 2 min; no worker for 5 min. |
| PreviaBuildsUnavailable / PreviaBuildsPaused | Workers run but none can build for 10 min / builds paused for over an hour. |
| PreviaQueueWaitHigh / PreviaBuildQueueWaitHigh | A due job waited over 10 min (30 min for builds). |
| PreviaBuildFailureSpike / PreviaPreviewsNotBecomingReady | Over half of the last hour's builds failed (at least five) / five previews never became ready in an hour. |
| PreviaJobsFailing / PreviaOperationsFailed / PreviaCleanupBacklog | Jobs keep failing; an operation failed within a day; over 25 cleanups pending for 30 min. |
| PreviaReconcilerMassDeletion | Housekeeping removed over 20 resources in 30 min. |
| PreviaWebhookFailures / PreviaHTTPErrors / PreviaStateUnreadable | Failing or badly signed deliveries; over 5% dashboard errors; state unreadable. |
| PreviaDiskLow / PreviaBuildsWaitingForDisk / PreviaBuildsWaitingForCapacity | Under 20 GiB free; builds waiting 15 min for disk / 30 min for capacity. |
| PreviaGitHubRateLimitLow / PreviaGitHubRateLimited | Under 500 requests left; GitHub refused requests. |
| PreviaBackupStale / PreviaBackupNeverRecorded | No backup for 26 h / none ever recorded. |
| PreviaCertificateExpiringSoon / PreviaCertificateExpiring | A router certificate expires within 14 / 3 days (renewal is failing). |

## Ingress and process hardening

| Control | Behaviour |
| --- | --- |
| Security headers | Every response: a nonce-based Content-Security-Policy (`default-src 'self'`, no inline script without the nonce, no `eval`, `object-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'`, forms only to the dashboard and `https://github.com`), `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin`, `Cross-Origin-Opener-Policy: same-origin`, a restrictive `Permissions-Policy`, and HSTS when `APP_BASE_URL` is HTTPS. The browser suite runs under this policy and fails on any violation. |
| Host validation | Only `APP_BASE_URL`'s host is served (421 otherwise), and through a trusted proxy the forwarded host must match too. Signed webhooks are exempt so they work through a tunnel. The server never builds URLs from request headers. |
| Trusted proxies | `TRUSTED_PROXIES` (addresses or CIDR ranges): `X-Forwarded-For` is read right to left only while hops are trusted; from anyone else forwarded headers are ignored. In production the router's control-network address is the only trusted proxy. |
| Request limits | 64 KiB of headers; 256 KiB of form body (the largest form is a 32 KiB secret); webhooks keep their own 2 MiB cap. Timeouts: 5 s headers, 30 s read and write, 60 s idle; event streams have their own caps and per-frame deadlines ([20](20-realtime-updates.md)). |
| Abuse controls | Sign-in and installation callbacks: 20 requests per client at once, then one every 2 s (429 with `Retry-After`; at most 10,000 clients tracked). Webhooks: at most 32 at once; the rest get 503 before anything is recorded, so redelivery and deduplication work as usual. |
| Outbound timeouts | OAuth and GitHub API clients 10 s; database connects 10 s; webhook processing 8 s; internal listener 10–15 s. |
| Sessions and cookies | `__Host-` session cookie, `Secure`, `HttpOnly`, `SameSite=Lax`, host-only, so a preview on another subdomain or domain never receives it; CSRF tokens plus an `Origin` check on every mutation (tested in `internal/auth`). The server warns when previews share the dashboard's registrable domain. |
| Production mode | `APP_ENV=production`: `APP_BASE_URL` must be HTTPS, `GITHUB_WEBHOOK_SECRET` is required, the GitHub App key file must have no group or other permissions, and the queue-ping developer tools are not served (the page says so). Release images default to it. |
| Processes and containers | Release images are distroless, run as UID 65532 on a read-only root with every capability dropped and `no-new-privileges`. Only the worker has the Docker socket (root-equivalent on the host by design: it builds and runs previews); the server and previews never do. |
| Credentials | Key material, the DNS token and the App key are files with mode 600, never environment values or arguments; repository secrets are AES-256-GCM at rest ([07](07-repository-secrets.md)). |

The isolation of builds and previews (no route to the host, PostgreSQL, the
control plane, metadata or other previews) is unchanged and proven by
`make test-builder`, `make test-runtime` and `make test-routing`.

## Execution kill switch

`BUILDS_PAUSED=true` on the worker (restart it): admission makes no GitHub or Docker
call for a build; each request waits with reason `paused` (dashboard, PR comment
and check run say "Builds paused") and runs when a worker starts without the
switch. Teardown, cleanup, routing, reports and housekeeping keep running. The
worker reports `paused` in its capabilities, the dashboard refuses new build
requests with "Builds are paused by the operator", and `PreviaBuildsPaused` reminds
the operator after an hour. Migration 0018 adds the reason and the capability.

## Dependencies

`make vuln` runs govulncheck. It found nine reachable vulnerabilities in Go 1.26.5's
standard library and `containerd/v2` 2.3.4; the minimum Go version is now 1.26.8 and
containerd 2.3.6, and the scan reports none reachable. One advisory remains in a
required module but is not reachable: `golang.org/x/crypto/openpgp` (deprecated,
no fix; Previa never imports it). CI runs the scan, and Trivy scans the release
images for fixable HIGH and CRITICAL vulnerabilities ([25](25-production-deployment.md#ci-and-releases)).

## What was verified

- Unit tests: redaction by key and shape, correlation fields, levels
  (`internal/platform/logging`); bounded labels and the state collector keeping its
  last snapshot (`metrics`); client IP from trusted proxies only, host and
  forwarded-host validation, the nonce policy and headers, body limits, per-client
  rate limits and their memory bound, the webhook concurrency cap, `/livez`,
  `/readyz` (including a failing dependency, with redacted reasons) and the probe
  (`httpx`); GitHub rate-limit headroom (`githubapp`); production-mode and setting
  validation (`config`); the schema version against the newest migration
  (`database`); queue depth and job metrics on River's real table (`queue`); the
  paused worker's capabilities (`workers`); a paused build that makes no external
  call and resumes (`environments`); the paused message and 503 (`pullrequests`);
  developer tools off in production (`dashboard`).
- The browser suite (30 scenarios) under the real Content-Security-Policy.
- `make observability-check`: 24 rules, their unit tests, the configuration and
  28 dashboard queries.
- The production smoke test ([25](25-production-deployment.md#local-staging-smoke-test)):
  headers, host validation, rate limits, unpublished internal listeners, readiness,
  metrics, Prometheus scraping all three targets, the alert rules loaded and the
  Grafana dashboard provisioned.

## Not done here

- Alert delivery (an Alertmanager and a receiver are the operator's choice).
- Optional OpenTelemetry tracing (DESIGN §18 lists it as optional).
- Every live proof in [22](22-mvp-operations.md#live-proofs-still-required-before-a-public-rollout)
  and [26](26-release-report.md).
