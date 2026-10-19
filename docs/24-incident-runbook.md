# 24 · Incident runbook

What to do when something goes wrong on a production host. In production, Sentry
tells you ([25](25-production-deployment.md#monitoring-with-sentry)): an email about
a new issue, a failing uptime check or a backup that failed or never ran. The table
says where each signal leads. The **Alerts** named in each section are the rules in
`deploy/observability/alerts.yml`, which fire only with the optional observability
profile; they name the same situations. The metrics and endpoints are described in
[23](23-observability-and-hardening.md); the host layout in
[25](25-production-deployment.md).

| What Sentry shows | Start at |
| --- | --- |
| The uptime monitor for `https://previa.live` fails | [Server or worker down](#server-or-worker-down); if the VPS does not answer at all, the OVHcloud Control Panel (KVM console, the VPS's state) |
| The cron monitor `previa-backup` reports an error or a missed check-in | [Backup failed](#backup-failed) |
| An issue from `process=worker` about a job, a build or Docker | [Queue backlog](#queue-backlog), [Build failures](#build-failures) or [Pending operations](#pending-operations), by its message and `job_kind` |
| An issue about GitHub (rate limit, authentication, an API error) | [GitHub outage or rate limits](#github-outage-or-rate-limits) |
| An issue `HTTP handler panic` from `process=server` | Its stack field names the handler; `dc logs server` around that time |
| An issue about certificates or ACME is **not** sent: Traefik's own logs do not reach Sentry | [Certificate renewal](#certificate-renewal); the uptime monitor catches an expired dashboard certificate, and the monthly check in [25](25-production-deployment.md#monthly-checks) the preview wildcard |

## First steps for any incident

```sh
ssh ops@previa-prod-01              # over Tailscale; then sudo -i
cd /opt/previa                      # the release checkout used to deploy
dc() { docker compose -f deploy/production/compose.yml --env-file /etc/previa/previa.env "$@"; }
probe() { docker run --rm --network previa_control curlimages/curl:8.15.0 -fsS "$@"; }

dc ps                                              # state and health of every service
probe http://server:9090/readyz; echo              # database and schema
probe http://worker:9091/readyz; echo              # database, schema, River, heartbeat, Docker, capabilities
dc logs --since 30m worker | grep '"level":"ERROR"'
dc logs --since 30m worker | grep '"pr_id":42'     # one pull request's story
```

`lazydocker` shows the same containers and their logs on one screen. Sentry's
issue shows the message, `process` and correlation fields (`job_kind`, `pr_id`,
`operation_id`) to search the logs for.

Logs never contain secrets or tokens (they are redacted), so they can be shared
with whoever helps. Do not paste `/etc/previa/previa.env`.

## Server or worker down

**Alerts:** PreviaServerDown, PreviaWorkerDown.

1. `dc ps`: is the container restarting? `dc logs --tail 100 server` (or `worker`).
2. Common causes and fixes:
   - `the database schema is at migration N but this build needs M`: the migration
     step failed or was skipped. `dc logs migrate`, fix the cause, `dc up -d`.
   - `configure server: ...` / `configure worker: ...`: a setting is invalid. Run
     `dc run --rm --no-deps server check-config` (or `worker check-config`), fix
     `/etc/previa/previa.env`, `dc up -d`.
   - `builds need Docker`: the worker cannot reach `/var/run/docker.sock`; check
     `DOCKER_GID` matches `stat -c %g /var/run/docker.sock`.
   - The worker's readiness reports `heartbeat`: it cannot write to PostgreSQL;
     see [Database unavailable](#database-unavailable).
3. While the worker is down nothing is lost: requests, wake-ups, teardowns and
   reports are durable and run when it starts. The dashboard refuses new build
   requests with "No preview worker is running".

## Builds unavailable

**Alert:** PreviaBuildsUnavailable. Workers run, but none can build.

The worker logs why at start: `dc logs worker | grep 'capability unavailable'`.
Typical reasons: the GitHub App settings, `PREVIEW_BASE_DOMAIN`, the routing
settings, or `BUILDS_PAUSED` (see below). Fix the setting and `dc up -d worker`.

## Execution kill switch

**Alert:** PreviaBuildsPaused (a reminder after an hour).

Use it to stop running untrusted code quickly: a suspected malicious preview, a
runaway build, a host under pressure. Teardown, cleanup, routing, reports and
housekeeping keep working.

```sh
echo 'BUILDS_PAUSED=true' >> /etc/previa/previa.env   # or edit the existing line
dc up -d worker      # the worker drains for 30 s, then cancels running builds
```

Cancelled builds are not failures; they wait (reason `paused`) with every new
request. To stop a specific preview as well, tear it down from its pull request
page or the Previews page. To resume, remove the line (or set it to `false`)
and `dc up -d worker`.

## Database unavailable

**Alerts:** PreviaStateUnreadable, PreviaHTTPErrors; readiness reports `database`.

1. `dc ps postgres`, `dc logs --tail 100 postgres`. Disk full is the usual cause:
   see [Disk exhaustion](#disk-exhaustion).
2. When PostgreSQL is back, the server and worker reconnect by themselves; River
   resumes due jobs and housekeeping's recovery (every minute) re-creates lost
   wake-ups and expired leases. Nothing needs replaying by hand.
3. If the data is damaged, restore the last backup
   ([25](25-production-deployment.md#backups-and-restore)).

## Queue backlog

**Alerts:** PreviaQueueWaitHigh, PreviaBuildQueueWaitHigh, PreviaJobsFailing,
PreviaBuildsWaitingForCapacity.

1. Which queue, and are jobs failing or just waiting?
   `dc exec -T postgres psql -U previa -d previa -c "SELECT queue, state, count(*) FROM river_job GROUP BY 1, 2 ORDER BY 1, 2;"`
   (or Grafana's "Queues and requests" with the optional profile).
2. Builds waiting for capacity are normal under load: at most
   `MAX_CONCURRENT_BUILDS` run at once and the preview budget is bounded. Raise
   `MAX_CONCURRENT_BUILDS` only if the host has room
   ([15](15-housekeeping-and-capacity.md)).
3. Jobs failing repeatedly: `dc logs worker | grep '"job_kind":"<kind>"'` shows the
   error. Fix the cause; retries and recovery continue on their own.
4. A queue with work but no worker: see [Server or worker down](#server-or-worker-down).

## Build failures

**Alerts:** PreviaBuildFailureSpike, PreviaPreviewsNotBecomingReady.

1. The **Builds** page and Sentry's issues show which failure category dominates
   (or Grafana's "Build failures by code" with the optional profile).
2. `build_failed`, `unhealthy`, `crashed` across many repositories at once point
   to the platform: base image registry outage (the egress proxy must reach it),
   BuildKit image pull problems, or a host problem. One repository failing is
   usually its own Dockerfile or health path; its build log explains it.
3. `secrets_unavailable`: the worker has no keyring; set `SECRETS_*` like the server.
4. `capacity`/`disk_low` are waits, not failures; see the next sections.

## Pending operations

**Alerts:** PreviaOperationsFailed, PreviaCleanupBacklog.

Operations are routes, reports and cleanups ([19](19-generation-ordering.md#the-decision)).

1. `dc exec -T postgres psql -U previa -d previa -c "SELECT kind, state, count(*) FROM environment_operations GROUP BY 1, 2 ORDER BY 1, 2;"`
   (or Grafana's "Operations by kind and state" with the optional profile).
2. `activate_route`/`remove_route` failing: is the router healthy (`dc ps router`)?
   Is `/var/lib/previa/router` writable by UID 65532?
3. `cleanup_artifact` backlog: Docker slow or refusing removal. `docker ps -a
   --filter label=io.previa.managed=true` shows what is left; cleanup retries until
   it succeeds and never removes another database's objects.
4. `report_status` failing: GitHub; see the next section but one.

## Unexpected deletions

**Alert:** PreviaReconcilerMassDeletion (critical).

Housekeeping removed far more orphans than usual. Stop it from continuing while
you look:

```sh
echo 'HOUSEKEEPING_DRY_RUN=true' >> /etc/previa/previa.env
dc up -d worker
dc logs --since 1h worker | grep -E 'orphan|unknown PR|housekeeping'
dc run --rm --no-deps --entrypoint /usr/local/bin/previa-housekeeping worker   # what it would do now (a dry run)
```

Likely causes: a database restored from an old backup (its rows no longer match
the containers), or a second Previa database pointed at the same Docker host with
the same instance (it must not share the database). Only objects labelled with
this database's instance are ever touched, and only after the grace period.
Remove the dry-run line once explained.

## GitHub outage or rate limits

**Alerts:** PreviaWebhookFailures, PreviaGitHubRateLimitLow, PreviaGitHubRateLimited.

- **Outage:** builds and teardown continue; build requests are refused (fresh
  reads fail closed) and reports wait. Reports catch up on their own when GitHub
  answers (proven by `make test-e2e`). Nothing to do but wait; check
  https://www.githubstatus.com.
- **Rate limits:** work waits for GitHub's own delay without spending attempts.
  Persistent pressure means too many PRs reconciled or synced; check webhook
  delivery (missed webhooks force reconciliation) and avoid repeated manual syncs.
- **Invalid signatures:** `GITHUB_WEBHOOK_SECRET` differs from the App's webhook
  secret, or someone else is sending requests. Deliveries can be redelivered from
  the App's settings once fixed; deduplication makes that safe.

## Disk exhaustion

**Alerts:** PreviaDiskLow, PreviaBuildsWaitingForDisk.

1. `df -h /var/lib/docker`, `docker system df`.
2. Builds already wait instead of failing and housekeeping prunes at once when they
   do (untagged images, old workspaces, caches over `BUILD_CACHE_BUDGET`). Lower
   `BUILD_CACHE_BUDGET` or `ENV_TTL_HOURS` for more room.
3. Tear down previews nobody uses (Previews page), or pause builds.
4. Never `docker system prune -a` on a Previa host: it removes the egress image and
   serving previews. If space must be freed by hand, remove only images not used by
   a running container and labelled `io.previa.managed=true`.
5. If PostgreSQL's volume filled, free space first; PostgreSQL recovers by itself.

## Capacity outgrown

**Alert:** PreviaCapacityOutgrown.

Builds waited for a build slot, preview memory or disk over a quarter of the last
day. It is a planning signal, not an outage.

1. Is it every day, or one busy day? Watch the Builds page's waiting reasons over
   a few days (or Grafana's "Queues and requests" with the optional profile).
2. Cheaper first: `PREVIEW_IDLE_SLEEP=3h` frees idle previews' memory; a lower
   `ENV_TTL_HOURS` removes old ones sooner; `BUILDS_PER_ACCOUNT` and
   `PREVIEW_MAX_PER_ACCOUNT` stop one account taking everything.
3. If it persists, move to a larger server and raise the budgets together
   ([25](25-production-deployment.md#when-to-upgrade-the-server)). The worker warns
   at start when budgets exceed the host.

## Unusual build traffic

**Alerts:** PreviaBuildHeavyUpload, PreviaBuildHeavyDownload.

Package installs download a lot but upload almost nothing; a build that uploads
hundreds of megabytes is moving data out (exfiltration, abuse of the server's
bandwidth).

1. Find it: `dc logs worker | grep '"msg":"build traffic"'`. Each entry carries
   `pr_id`, `bytes_up`, `bytes_down` and `refused`; the build log ends with the same
   totals.
2. Look at the pull request and its Dockerfile. If it is not legitimate, tear the
   preview down, switch the repository to Manual builds, and remove the App from
   that account if needed.
3. To stop builds reaching anything but package registries, set
   `BUILD_EGRESS=registries` (add needed hosts to `BUILD_EGRESS_HOSTS`) and restart
   the worker. Forks never build without the owner's click
   ([16](16-build-policy-and-intents.md#behaviour-rules)).

## Backup failed

For F4 deletion progress stuck at `journal_pending`, inspect
`systemctl status previa-deletion-journal.service` and
`journalctl -u previa-deletion-journal -n 50`. Check the age recipient, off-host
remote permissions, retention ordering and plaintext size bound, then run the
service again. Do not mark an operation acknowledged by hand or prune unfinished
events to make the size check pass. `cleanup_pending`/`cleanup_retry` needs the
execution worker, route operations, runtime inventory, baseline/source storage
and build-cache removal to succeed. GitHub comment attempts stop after three
tries/ten minutes; local teardown does not require GitHub access.

During any restore, stop server/worker/router and both timers, retain the
independent latest journal, and use production B5/B6. Missing/invalid/foreign or
older-than-snapshot journal data deliberately prevents serving. Never start an
old binary around this check. [Account exits and recovery](34-accounts-support-and-deletion.md)
explains replay and minimal revocation identifiers.

**Sentry:** the cron monitor `previa-backup` reports an error, or no check-in
arrived within a day. **Alerts:** PreviaBackupStale, PreviaBackupNeverRecorded.

1. `systemctl status previa-backup.service`, `journalctl -u previa-backup -n 50`.
   A missed check-in with no failed run means the timer did not run
   (`systemctl list-timers previa-backup.timer`), the host was down, or it could
   not reach Sentry (`Sentry check-in … not sent` in the journal).
2. Common causes: `BACKUP_AGE_RECIPIENT` or `BACKUP_REMOTE` unset, rclone
   credentials expired, the remote full, PostgreSQL down.
3. Run it by hand to confirm: `systemctl start previa-backup.service`.
4. The status file only changes after every file was uploaded, and only then does
   the run check in *ok*, so the monitor and the alert clear only on a complete
   backup.

## Certificate renewal

**Sentry:** the uptime monitor fails when the dashboard's certificate is invalid.
A failing wildcard renewal reaches no one until the
[monthly check](25-production-deployment.md#monthly-checks) or a visitor sees it.
**Alerts:** PreviaCertificateExpiringSoon, PreviaCertificateExpiring.

Traefik renews 30 days before expiry. If it has not:

1. `dc logs router | grep -i acme`.
2. Wildcard (previews): the DNS token expired or lost permission, or
   `ACME_DNS_PROVIDER`/`ACME_DNS_CREDENTIAL_VARIABLE` changed. Replace
   `/etc/previa/dns-credential` (mode 600) and `dc up -d router`.
3. Dashboard: TLS-ALPN-01 needs port 443 reachable from the internet.
4. Let's Encrypt rate limits: switch `ACME_CA_SERVER` to staging while testing.

## Key rotation

- **Repository secrets keyring:** add a new version to `SECRETS_ENCRYPTION_KEYS`,
  set `SECRETS_ACTIVE_KEY_VERSION` to it, `dc up -d server worker`, then
  `dc run --rm --no-deps --entrypoint /usr/local/bin/previa-secrets-reencrypt server`
  until it reports zero values on old versions; only then remove the old version
  ([07](07-repository-secrets.md)). Back up the new keyring offline.
- **GitHub App private key:** generate a new key in the App's settings, replace
  `/etc/previa/github-app.pem` (owner 65532, mode 600), `dc up -d server worker`,
  then delete the old key on GitHub.
- **Webhook secret:** change it in the App's settings and `GITHUB_WEBHOOK_SECRET`
  together, then `dc up -d server`; redeliver anything that failed in between.
- **Session secret / OAuth secret:** replace and `dc up -d server`; everyone signs
  in again.
- **DNS token:** see [Certificate renewal](#certificate-renewal).

## Suspected compromise

A preview behaving maliciously, or a leaked credential:

1. [Pause builds](#execution-kill-switch) and tear down the affected previews.
2. Rotate whatever may have leaked ([Key rotation](#key-rotation)). Repository
   secrets given to previews are the owner's to rotate at their provider.
3. Keep the logs (`dc logs > incident.log`) before restarting anything.
4. Previews never had the Docker socket, host networking, PostgreSQL or the
   control plane ([10](10-preview-runtime.md)); the worker did, and the server had
   the database. Investigate those first if host compromise is suspected.


## Missed GitHub lifecycle deliveries

A reconciliation repair means a signed webhook was missed. Review warning/error
logs (Sentry in production), the bounded installation reconciliation metric and
`PreviaMissedGitHubLifecycle` / `PreviaInstallationReconciliationBlocked` alerts.
Check GitHub App delivery history, webhook reachability/signatures, the configured
numeric App ID/private key and GitHub availability/rate limits. Reconciliation
never restores access; do not activate an old deleted installation manually.

If the all-known or change-budget circuit opens, inspect the affected identities
and redeliver the real signed lifecycle events from GitHub after fixing delivery.
Increasing `INSTALLATION_RECONCILE_MAX_CHANGES` does not disable the all-known
check. A verified explicit same-workspace reconnect may confirm an old deletion
and recover it; an outage or another workspace's history cannot be adopted.
Local teardown needs no GitHub access and continues while builds are paused.

For early-access approval, set `SUPPORT_OPERATOR_GITHUB_IDS` to numeric user IDs,
restart the server, then use `/operator/early-access`. Never grant a customer
Owner/Admin operator authority implicitly. Approval is audited and starts no build.
