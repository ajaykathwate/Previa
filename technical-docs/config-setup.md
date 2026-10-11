# Configuration setup

Every Previa setting: what it is, where the app uses it, and how to get its value.
Local settings go in `.env` (copy `.env.example`); production settings go in
`/etc/previa/previa.env` (copy `deploy/production/previa.env.example`). Settings
marked *command line* are typed before a `make` command instead.

## Contents

| Setting | Type | Needed for | Set in |
| --- | --- | --- | --- |
| [DATABASE_URL](#database_url) | Required | Starting the server, worker and migrations | `.env`, `previa.env` |
| [APP_ENV](#app_env) | Optional | Production hardening | `.env`, `previa.env` |
| [LOG_LEVEL](#log_level) | Optional | Log detail | `.env`, `previa.env` |
| [HTTP_ADDR](#http_addr) | Optional | Dashboard address | `.env` |
| [APP_BASE_URL](#app_base_url) | Required | Sign-in and previews | `.env`, `previa.env` |
| [GITHUB_OAUTH_CLIENT_ID](#github_oauth_client_id) | Required | Starting the server; sign-in | `.env`, `previa.env` |
| [GITHUB_OAUTH_CLIENT_SECRET](#github_oauth_client_secret) | Required | Starting the server; sign-in | `.env`, `previa.env` |
| [SESSION_SECRET](#session_secret) | Required | Starting the server | `.env`, `previa.env` |
| [GITHUB_WEBHOOK_SECRET](#github_webhook_secret) | Required | Live updates, Automatic builds; production | `.env`, `previa.env` |
| [INTERNAL_ADDR](#internal_addr) | Optional | Health and metrics | `.env` |
| [TRUSTED_PROXIES](#trusted_proxies) | Optional | Running behind a reverse proxy | `.env` |
| [BACKUP_STATUS_FILE](#backup_status_file) | Optional | Backup alerting | `.env` |
| [GITHUB_APP_ID](#github_app_id) | Required | Repositories, sync, builds, PR feedback | `.env`, `previa.env` |
| [GITHUB_APP_PRIVATE_KEY_PATH](#github_app_private_key_path) | Required | Repositories, sync, builds, PR feedback | `.env` |
| [GITHUB_APP_SLUG](#github_app_slug) | Required | Connecting repositories | `.env`, `previa.env` |
| [GITHUB_APP_CLIENT_ID](#github_app_client_id) | Required | Connecting repositories | `.env`, `previa.env` |
| [GITHUB_APP_CLIENT_SECRET](#github_app_client_secret) | Required | Connecting repositories | `.env`, `previa.env` |
| [SECRETS_ENCRYPTION_KEYS](#secrets_encryption_keys) | Required | Repository secrets | `.env`, `previa.env` |
| [SECRETS_ACTIVE_KEY_VERSION](#secrets_active_key_version) | Required | Repository secrets | `.env`, `previa.env` |
| [MAX_CONCURRENT_BUILDS](#max_concurrent_builds) | Required | Starting the worker | `.env`, `previa.env` |
| [BUILD_TIMEOUT](#build_timeout) | Optional | Builds | `.env`, `previa.env` |
| [WORKER_INTERNAL_ADDR](#worker_internal_addr) | Optional | Health and metrics | `.env` |
| [BUILDS_PAUSED](#builds_paused) | Optional | Incidents (kill switch) | `.env`, `previa.env` |
| [BUILD_CPUS](#build_cpus) | Optional | Builds | `.env` |
| [BUILD_MEMORY](#build_memory) | Optional | Builds | `.env` |
| [BUILD_PIDS](#build_pids) | Optional | Builds | `.env` |
| [BUILD_DISK_BUDGET](#build_disk_budget) | Optional | Builds | `.env` |
| [BUILD_MAX_IMAGE_SIZE](#build_max_image_size) | Optional | Builds | `.env` |
| [BUILD_MIN_FREE_DISK](#build_min_free_disk) | Optional | Builds | `.env` |
| [EGRESS_IMAGE](#egress_image) | Optional | Builds and previews | `.env` |
| [BUILDKIT_IMAGE](#buildkit_image) | Optional | Builds | `.env` |
| [SOURCE_WORKSPACE_ROOT](#source_workspace_root) | Optional | Builds | `.env` |
| [PREVIEW_ALLOW_ROOT](#preview_allow_root) | Optional | Previews | `.env` |
| [PREVIEW_WRITABLE_ROOTFS](#preview_writable_rootfs) | Optional | Previews | `.env` |
| [PREVIEW_TMPFS_SIZE](#preview_tmpfs_size) | Optional | Previews | `.env` |
| [PREVIEW_PIDS](#preview_pids) | Optional | Previews | `.env` |
| [PREVIEW_STARTUP_TIMEOUT](#preview_startup_timeout) | Optional | Previews | `.env` |
| [PREVIEW_MEMORY_BUDGET](#preview_memory_budget) | Optional | Previews | `.env` |
| [PREVIEW_CPU_BUDGET](#preview_cpu_budget) | Optional | Previews | `.env` |
| [PREVIEW_MAX_PER_REPOSITORY](#preview_max_per_repository) | Optional | Previews | `.env` |
| [PREVIEW_OCI_RUNTIME](#preview_oci_runtime) | Optional | Previews | `.env` |
| [PREVIEW_SERVICE_CATALOG](#preview_service_catalog) | Optional | Backend previews | `.env` |
| [PREVIEW_SETUP_TIMEOUT](#preview_setup_timeout) | Optional | Backend previews | `.env` |
| [PREVIEW_OUTBOUND_PORTS](#preview_outbound_ports) | Optional | Backend previews | `.env` |
| [PREVIEW_BASE_DOMAIN](#preview_base_domain) | Required | Builds and previews | `.env`, `previa.env` |
| [ROUTER_DYNAMIC_DIR](#router_dynamic_dir) | Required | Builds and previews | `.env` |
| [ROUTER_CONTAINER](#router_container) | Optional | Previews | `.env` |
| [ROUTER_ENTRYPOINT](#router_entrypoint) | Optional | Previews | `.env`, `previa.env` |
| [ROUTER_API_ADDRESS](#router_api_address) | Optional | Previews | `.env`, `previa.env` |
| [ACME_EMAIL](#acme_email) | Required | Previews (router) | `.env`, `previa.env` |
| [ACME_DNS_PROVIDER](#acme_dns_provider) | Required | Previews (router) | `.env`, `previa.env` |
| [ACME_DNS_CREDENTIAL_VARIABLE](#acme_dns_credential_variable) | Required | Previews (router) | `.env`, `previa.env` |
| [ACME_DNS_CREDENTIAL_FILE](#acme_dns_credential_file) | Required | Previews (router) | `.env`, `previa.env` |
| [ACME_CA_SERVER](#acme_ca_server) | Required | Previews (router) | `.env`, `previa.env` |
| [ACME_DNS_RESOLVERS](#acme_dns_resolvers) | Optional | Previews (router) | `.env` |
| [ENV_TTL_HOURS](#env_ttl_hours) | Optional | Preview expiry | `.env`, `previa.env` |
| [HOUSEKEEPING_DRY_RUN](#housekeeping_dry_run) | Optional | Housekeeping | `.env` |
| [HOUSEKEEPING_GRACE](#housekeeping_grace) | Optional | Housekeeping | `.env` |
| [BUILD_CACHE_BUDGET](#build_cache_budget) | Optional | Housekeeping | `.env` |
| [TEST_DATABASE_URL](#test_database_url) | Optional | Integration tests | `.env` |
| [TEST_GITHUB_INSTALLATION_ID](#test_github_installation_id) | Optional | Live GitHub checks | `.env` |
| [TEST_GITHUB_REPO_ID](#test_github_repo_id) | Optional | Live GitHub checks | `.env` |
| [TEST_GITHUB_PR_NUMBER](#test_github_pr_number) | Optional | Live GitHub checks | `.env` |
| [TEST_E2E_LOG](#test_e2e_log) | Optional | End-to-end tests | `.env` |
| [TEST_ROUTING_LOG](#test_routing_log) | Optional | Routing tests | `.env` |
| [WORKSPACES](#workspaces) | Optional | Housekeeping dry run | `.env` |
| [ENV_FILE](#env_file) | Optional | Choosing another settings file | command line |
| [UI_BROWSER_EXECUTABLE](#ui_browser_executable) | Optional | Browser tests | command line |
| [UI_BROWSER_SCREENSHOTS](#ui_browser_screenshots) | Optional | Browser tests | command line |
| [PREVIA_VERSION](#previa_version) | Required | Production; release images | `previa.env`, command line |
| [SMOKE_OLD_DUMP](#smoke_old_dump) | Optional | Production smoke test | command line |
| [SMOKE_KEEP](#smoke_keep) | Optional | Production smoke test | command line |
| [PREVIA_SERVER_IMAGE](#previa_server_image) | Optional | Production | `previa.env` |
| [PREVIA_WORKER_IMAGE](#previa_worker_image) | Optional | Production | `previa.env` |
| [PREVIA_EGRESS_IMAGE](#previa_egress_image) | Optional | Production | `previa.env` |
| [DOCKER_GID](#docker_gid) | Required | Production | `previa.env` |
| [POSTGRES_PASSWORD](#postgres_password) | Required | Production | `previa.env` |
| [PREVIA_DASHBOARD_HOST](#previa_dashboard_host) | Required | Production | `previa.env` |
| [GITHUB_APP_PRIVATE_KEY_FILE](#github_app_private_key_file) | Optional | Production | `previa.env` |
| [BACKUP_AGE_RECIPIENT](#backup_age_recipient) | Required | Production backups | `previa.env` |
| [BACKUP_REMOTE](#backup_remote) | Required | Production backups | `previa.env` |
| [BACKUP_RETENTION_DAYS](#backup_retention_days) | Optional | Production backups | `previa.env` |
| [BACKUP_FAILURE_WEBHOOK](#backup_failure_webhook) | Optional | Production backups | `previa.env` |
| [GRAFANA_ADMIN_PASSWORD](#grafana_admin_password) | Optional | Production dashboards | `previa.env` |
| [PREVIA_ENV_FILE](#previa_env_file) | Optional | Production | command line |
| [PREVIA_ROUTER_DIR](#previa_router_dir) | Optional | Production | command line |
| [PREVIA_BACKUP_STATUS_DIR](#previa_backup_status_dir) | Optional | Production | command line |

---

## Shared

### `DATABASE_URL`

- **Type:** Required. No default.
- **What it is:** the PostgreSQL connection for everything Previa stores: users,
  sessions, repositories, pull requests, builds, previews, logs, secrets and the job
  queue.
- **Where it's used:** the dashboard (every page and sign-in session), the worker
  (every job), and `make migrate-up` / `previa-migrate`, which create the tables.
- **How to get it (local):**
  1. Install Docker Desktop (Windows/macOS) or Docker Engine (Linux), and start it.
  2. In the project folder run `docker compose -f deploy/docker-compose.yml up -d`.
     This starts PostgreSQL 17 with user `previa`, password `change-me` and
     database `previa` on `127.0.0.1:5432`.
  3. Keep the value from `.env.example`. If you change the user, password, port or
     database name in `deploy/docker-compose.yml`, change them here too.
  4. Run `make migrate-up` once, and again after every update.
- **How to get it (production):** `deploy/production/setup.sh` generates
  [POSTGRES_PASSWORD](#postgres_password) and writes it into this URL; the host is
  the `postgres` service on the Compose network.
- **Example:** `DATABASE_URL=postgres://previa:change-me@localhost:5432/previa?sslmode=disable`

### `APP_ENV`

- **Type:** Optional. Default `development`.
- **What it is:** `development` or `production`.
- **Where it's used:** with `production`, the server refuses to start without an
  HTTPS [APP_BASE_URL](#app_base_url) and a
  [GITHUB_WEBHOOK_SECRET](#github_webhook_secret), refuses a GitHub App key file
  others can read, and the dashboard hides the **Developer tools** queue ping.
- **How to get it:** keep `development` on your machine. Release images and the
  production Compose file set `production`.
- **Example:** `APP_ENV=development`

### `LOG_LEVEL`

- **Type:** Optional. Default `info`.
- **What it is:** how much the server, worker and egress proxy log: `debug`,
  `info`, `warn` or `error`.
- **Where it's used:** the JSON logs of every process (secrets are always redacted).
- **How to get it:** keep `info`; use `debug` while investigating a problem.
- **Example:** `LOG_LEVEL=info`

## Dashboard (server)

### `HTTP_ADDR`

- **Type:** Optional. Default `127.0.0.1:8080`.
- **What it is:** the address and port the dashboard listens on.
- **Where it's used:** opening the dashboard in a browser.
- **How to get it:** keep `127.0.0.1:8080`. To use another port, change this,
  [APP_BASE_URL](#app_base_url) and both GitHub callback URLs together.
- **Example:** `HTTP_ADDR=127.0.0.1:8080`

### `APP_BASE_URL`

- **Type:** Required. Default `http://localhost:8080` for the server; the worker
  needs it set when [PREVIEW_BASE_DOMAIN](#preview_base_domain) is set.
- **What it is:** the dashboard's public address, exactly as people open it.
- **Where it's used:** sign-in redirects, the GitHub App's callbacks, CSRF checks,
  secure cookies, links in pull request comments, and the page an unknown preview
  address is sent to.
- **How to get it:**
  1. On your machine use `http://localhost:8080`, and always open the dashboard as
     `localhost`, not `127.0.0.1` (they are different cookie hosts).
  2. In production use your dashboard's HTTPS address, for example
     `https://previa.example.com` (HTTPS is required outside localhost). Create a DNS
     `A` record for that name pointing at the server, as in
     [PREVIA_DASHBOARD_HOST](#previa_dashboard_host).
- **Example:** `APP_BASE_URL=http://localhost:8080`

### `GITHUB_OAUTH_CLIENT_ID`

- **Type:** Required (the server does not start without it). No default.
- **What it is:** the Client ID of a GitHub **OAuth App** (not the GitHub App).
- **Where it's used:** **Get started with GitHub** on the home page: signing in to
  the dashboard. It grants no repository access.
- **How to get it:**
  1. Sign in at [github.com](https://github.com).
  2. Click your profile picture (top right) → **Settings**.
  3. In the left sidebar, at the bottom, click **Developer settings**.
  4. Click **OAuth Apps** → **New OAuth App** (or **Register a new application**).
  5. **Application name:** anything, for example `Previa (local)`.
  6. **Homepage URL:** your [APP_BASE_URL](#app_base_url), for example
     `http://localhost:8080`.
  7. **Authorization callback URL:** your APP_BASE_URL followed by
     `/auth/github/callback`, exactly: `http://localhost:8080/auth/github/callback`.
  8. Leave **Enable Device Flow** unchecked and click **Register application**.
  9. Copy the **Client ID** shown on the next page (it starts with `Ov23li`) and
     paste it here.
- **Example:** `GITHUB_OAUTH_CLIENT_ID=Ov23liAbCdEfGh123456`

### `GITHUB_OAUTH_CLIENT_SECRET`

- **Type:** Required (the server does not start without it). No default.
- **What it is:** the client secret of the same GitHub OAuth App.
- **Where it's used:** sign-in, when the server exchanges GitHub's code for your
  identity.
- **How to get it:**
  1. Open the OAuth App from [GITHUB_OAUTH_CLIENT_ID](#github_oauth_client_id)
     (**Settings → Developer settings → OAuth Apps →** your app).
  2. Click **Generate a new client secret** (GitHub may ask for your password or
     two-factor code).
  3. Copy the secret immediately; GitHub shows it only once. Paste it here.
  4. If you lose it, generate a new one and replace this value.
- **Example:** `GITHUB_OAUTH_CLIENT_SECRET=0123456789abcdef0123456789abcdef01234567`

### `SESSION_SECRET`

- **Type:** Required (the server does not start without it). No default. At least
  32 characters.
- **What it is:** a random secret that signs the short-lived sign-in state cookie.
- **Where it's used:** every sign-in.
- **How to get it:**
  1. Run `openssl rand -hex 32` in a terminal (on Windows, in WSL or Git Bash).
  2. Paste the 64-character output here.
  3. Changing it later only interrupts sign-ins in progress.
- **Example:** `SESSION_SECRET=9f2c4e…` (64 hex characters)

### `GITHUB_WEBHOOK_SECRET`

- **Type:** Required for live updates and Automatic builds, and always in
  production. No default; without it the webhook endpoint accepts nothing.
- **What it is:** a shared secret that proves webhook deliveries really come from
  your GitHub App.
- **Where it's used:** new pull requests, pushes, closes and merges appearing on
  their own, pages updating live from GitHub, and Automatic builds. Without it,
  pull requests change only when you click **Sync Pull Requests**.
- **How to get it:**
  1. Run `openssl rand -hex 32` and paste the output here.
  2. Open your GitHub App (**Settings → Developer settings → GitHub Apps →** your
     App → **General**).
  3. Under **Webhook**, check **Active**, paste the same value into **Webhook
     secret**, and set **Webhook URL** to a public HTTPS address ending in
     `/webhooks/github`.
  4. On your own machine, get that public address from a tunnel to port 8080, for
     example `cloudflared tunnel --url http://localhost:8080` (prints
     `https://<name>.trycloudflare.com`) or `ngrok http 8080`. Use
     `https://<that host>/webhooks/github`. In production use
     `https://<your dashboard host>/webhooks/github`.
  5. Click **Save changes**, then restart the server.
  6. Check it: **Advanced → Recent Deliveries** in the App shows `204` responses.
- **Example:** `GITHUB_WEBHOOK_SECRET=4b1d…` (64 hex characters)

### `INTERNAL_ADDR`

- **Type:** Optional. Default `127.0.0.1:9090`.
- **What it is:** the server's internal address for `/livez`, `/readyz` and
  `/metrics`.
- **Where it's used:** health checks, Prometheus and the Grafana dashboard. Never
  expose it publicly.
- **How to get it:** keep the default; containers set `0.0.0.0:9090` on their
  private network.
- **Example:** `INTERNAL_ADDR=127.0.0.1:9090`

### `TRUSTED_PROXIES`

- **Type:** Optional. Default empty (trust no proxy).
- **What it is:** comma-separated addresses or CIDR ranges of reverse proxies that
  may pass the visitor's real address and host.
- **Where it's used:** sign-in rate limits and request logs see the real client
  address when the dashboard sits behind a proxy.
- **How to get it:** leave it empty on your machine. Behind a reverse proxy, use
  the proxy's address as the server sees it; the production Compose file already
  sets `172.31.250.10` (its router).
- **Example:** `TRUSTED_PROXIES=172.31.250.10`

### `BACKUP_STATUS_FILE`

- **Type:** Optional. Default empty (off).
- **What it is:** a file the backup script touches after every successful backup.
- **Where it's used:** the "last successful backup" metric and its alert.
- **How to get it:** leave it empty on your machine. The production Compose file
  sets it to `/var/lib/previa/backup/last-success`.
- **Example:** `BACKUP_STATUS_FILE=/var/lib/previa/backup/last-success`

## GitHub App

### `GITHUB_APP_ID`

- **Type:** Required for repositories (connecting, syncing, building, PR comments
  and checks). No default.
- **What it is:** the numeric ID of your **GitHub App**, the registration that gives
  Previa access to the repositories you choose.
- **Where it's used:** **Repositories → Install on GitHub / Choose repositories on GitHub**,
  **Sync Pull Requests**, mergeability checks, every build (reading the commit),
  and the Previa comment and check run on each pull request. The worker uses it
  too.
- **How to get it (creates the App; the other App settings come from it):**
  1. Sign in at [github.com](https://github.com) → profile picture → **Settings** →
     **Developer settings** → **GitHub Apps** → **New GitHub App**.
  2. **GitHub App name:** a unique name, for example `previa-<your-login>-local`.
  3. **Homepage URL:** your [APP_BASE_URL](#app_base_url).
  4. **Callback URL:** APP_BASE_URL + `/installations/authorize/callback`, for
     example `http://localhost:8080/installations/authorize/callback`.
  5. Leave **Request user authorization (OAuth) during installation** unchecked.
  6. **Setup URL:** APP_BASE_URL + `/installations/callback`; check **Redirect on
     update**.
  7. **Webhook:** as in [GITHUB_WEBHOOK_SECRET](#github_webhook_secret). If you have
     no public address yet, uncheck **Active** and enable it later.
  8. **Repository permissions:** **Contents: Read-only**, **Pull requests: Read and
     write**, **Checks: Read and write** (GitHub adds **Metadata: Read-only**).
  9. **Subscribe to events:** **Pull request**.
  10. **Where can this GitHub App be installed?** **Only on this account** (or any
      account, if others will install it).
  11. Click **Create GitHub App**. On the App's **General** page, copy **App ID**
      (a number) and paste it here.
- **Example:** `GITHUB_APP_ID=1234567`

### `GITHUB_APP_PRIVATE_KEY_PATH`

- **Type:** Required for repositories. No default.
- **What it is:** the full path of the GitHub App's private key file (`.pem`).
- **Where it's used:** the server and worker sign their requests to GitHub with it:
  connecting repositories, syncing, building, PR comments and checks.
- **How to get it:**
  1. Open your GitHub App → **General** → **Private keys** → **Generate a private
     key**. A `.pem` file downloads.
  2. Move it outside the project folder. On WSL, keep it in your Linux home, not
     under `/mnt/c`:
     `mkdir -p ~/.config/previa && mv /mnt/c/Users/<you>/Downloads/<app>.*.private-key.pem ~/.config/previa/github-app.private-key.pem`
  3. Make it readable only by you: `chmod 600 ~/.config/previa/github-app.private-key.pem`.
  4. Paste the full path here, without `~` (run `echo $HOME` to see your home
     directory).
  5. In production the key is a file at
     [GITHUB_APP_PRIVATE_KEY_FILE](#github_app_private_key_file) instead.
- **Example:** `GITHUB_APP_PRIVATE_KEY_PATH=/home/ajay/.config/previa/github-app.private-key.pem`

### `GITHUB_APP_SLUG`

- **Type:** Required for connecting repositories. No default.
- **What it is:** the GitHub App's URL name.
- **Where it's used:** **Install on GitHub** and **Choose repositories on GitHub** send you to
  `https://github.com/apps/<slug>`.
- **How to get it:**
  1. Open your GitHub App → **General**.
  2. Click **Public page** (or look at the address of the App's public page):
     `https://github.com/apps/<slug>`.
  3. Paste the `<slug>` part (lowercase letters, digits and hyphens).
- **Example:** `GITHUB_APP_SLUG=previa-ajay-local`

### `GITHUB_APP_CLIENT_ID`

- **Type:** Required for connecting repositories. No default.
- **What it is:** the GitHub App's own Client ID (different from the OAuth App's).
- **Where it's used:** after you install the App, Previa asks GitHub to confirm that
  you, the signed-in user, really own that installation before linking it.
- **How to get it:** open your GitHub App → **General** → copy **Client ID** (it
  starts with `Iv`) and paste it here.
- **Example:** `GITHUB_APP_CLIENT_ID=Iv23liAbCdEfGh123456`

### `GITHUB_APP_CLIENT_SECRET`

- **Type:** Required for connecting repositories. No default.
- **What it is:** the GitHub App's own client secret.
- **Where it's used:** the same installation check as
  [GITHUB_APP_CLIENT_ID](#github_app_client_id).
- **How to get it:**
  1. Open your GitHub App → **General** → **Client secrets** → **Generate a new
     client secret**.
  2. Copy it immediately (shown once) and paste it here.
- **Example:** `GITHUB_APP_CLIENT_SECRET=0123456789abcdef0123456789abcdef01234567`

## Repository secrets

### `SECRETS_ENCRYPTION_KEYS`

- **Type:** Required for repository secrets. No default. Set it together with
  [SECRETS_ACTIVE_KEY_VERSION](#secrets_active_key_version), with the same values
  in the server and the worker.
- **What it is:** one or more versioned 256-bit keys, written `version:key`, that
  encrypt the values on a repository's **Secrets** tab.
- **Where it's used:** **Repositories → a repository → Secrets** (saving values),
  and every build of a repository that has secrets (the worker decrypts them for
  the preview). Without it the Secrets tab says secrets are not configured.
- **How to get it:**
  1. Run `printf '1:%s\n' "$(openssl rand -base64 32)"`.
  2. Paste the whole output here, for example `1:3q2+7w…=`.
  3. Keep a copy somewhere safe and offline: without it, stored secrets can never
     be decrypted again (including after restoring a backup).
  4. To rotate later, add a new version after a comma (`1:old…,2:new…`), set
     [SECRETS_ACTIVE_KEY_VERSION](#secrets_active_key_version) to `2`, restart, run
     `make secrets-reencrypt`, and only then remove version 1.
- **Example:** `SECRETS_ENCRYPTION_KEYS=1:3q2+7wJr2l4nXn0Yb7m1c9a0E5s3F8k6P2t4R1u0w8Y=`

### `SECRETS_ACTIVE_KEY_VERSION`

- **Type:** Required for repository secrets. No default.
- **What it is:** which version in [SECRETS_ENCRYPTION_KEYS](#secrets_encryption_keys)
  encrypts newly saved values.
- **Where it's used:** saving on the **Secrets** tab, and re-encryption after a key
  rotation.
- **How to get it:** use the number before the `:` of the key you want new values to
  use; `1` when you have one key.
- **Example:** `SECRETS_ACTIVE_KEY_VERSION=1`

## Builds (worker)

### `MAX_CONCURRENT_BUILDS`

- **Type:** Required (the worker does not start without it). No default.
- **What it is:** how many builds may run at the same time, across all workers.
- **Where it's used:** the **Builds** page: requests beyond this number wait as
  **Queued**. It is also each worker process's number of build workers and the
  concurrency of the developer-tools ping queue.
- **How to get it:** `1` or `2` on a laptop. On a server, roughly the free memory
  divided by [BUILD_MEMORY](#build_memory) (4 GiB by default).
- **Example:** `MAX_CONCURRENT_BUILDS=2`

### `BUILD_TIMEOUT`

- **Type:** Optional. Default `12m`. More than 0 and at most `24h`.
- **What it is:** the longest one build may take: download, image build and the
  preview becoming healthy.
- **Where it's used:** a build that runs longer fails with "The build took longer
  than allowed". The job gets one more minute after that to publish or clean up.
- **How to get it:** keep `12m`; raise it (for example `30m`) for very large images.
- **Example:** `BUILD_TIMEOUT=12m`

### `WORKER_INTERNAL_ADDR`

- **Type:** Optional. Default `127.0.0.1:9091`.
- **What it is:** the worker's internal address for `/livez`, `/readyz` (including
  what it can do) and `/metrics`.
- **Where it's used:** health checks, Prometheus and the Grafana dashboard. Never
  expose it publicly.
- **How to get it:** keep the default.
- **Example:** `WORKER_INTERNAL_ADDR=127.0.0.1:9091`

### `BUILDS_PAUSED`

- **Type:** Optional. Default `false`.
- **What it is:** the emergency switch: `true` starts no new build, while teardown,
  cleanup, routing, PR comments and housekeeping keep running.
- **Where it's used:** the Overview pipeline and every Build button then say
  **Builds are paused by the operator**; queued requests start once it is `false`
  again.
- **How to get it:** keep `false`. During an incident set `true` and restart the
  worker.
- **Example:** `BUILDS_PAUSED=false`

### `BUILD_CPUS`

- **Type:** Optional. Default `2`. Between `0.25` and `64`.
- **What it is:** CPUs one build may use.
- **Where it's used:** every image build.
- **How to get it:** keep `2`; lower it on a small machine.
- **Example:** `BUILD_CPUS=2`

### `BUILD_MEMORY`

- **Type:** Optional. Default `4GiB`. At least `256MiB`; sizes take `KiB`, `MiB`,
  `GiB` or `TiB`.
- **What it is:** memory one build may use.
- **Where it's used:** every image build; a build that needs more fails.
- **How to get it:** keep `4GiB`; raise it for heavy builds (for example large
  front-end bundles).
- **Example:** `BUILD_MEMORY=4GiB`

### `BUILD_PIDS`

- **Type:** Optional. Default `2048`. Between `128` and `65536`.
- **What it is:** processes one build may run at once.
- **Where it's used:** every image build (protects the host from runaway builds).
- **How to get it:** keep the default.
- **Example:** `BUILD_PIDS=2048`

### `BUILD_DISK_BUDGET`

- **Type:** Optional. Default `20GiB`. At least `1GiB`.
- **What it is:** disk space one build may use.
- **Where it's used:** every image build.
- **How to get it:** keep the default; raise it for very large images.
- **Example:** `BUILD_DISK_BUDGET=20GiB`

### `BUILD_MAX_IMAGE_SIZE`

- **Type:** Optional. Default `4GiB`. At least `16MiB` and at most
  [BUILD_DISK_BUDGET](#build_disk_budget).
- **What it is:** the largest image a build may produce.
- **Where it's used:** a larger image fails the build with "The image build
  failed".
- **How to get it:** keep the default.
- **Example:** `BUILD_MAX_IMAGE_SIZE=4GiB`

### `BUILD_MIN_FREE_DISK`

- **Type:** Optional. Default `15GiB`.
- **What it is:** free disk space the Docker host must keep.
- **Where it's used:** below it, builds wait as **Waiting for disk space** instead of
  filling the disk, and housekeeping prunes old images and caches.
- **How to get it:** keep the default; lower it (for example `5GiB`) on a small
  laptop disk.
- **Example:** `BUILD_MIN_FREE_DISK=15GiB`

### `EGRESS_IMAGE`

- **Type:** Optional. Default `previa-egress:dev`.
- **What it is:** the Docker image of Previa's egress proxy, the only way builds and
  previews reach the internet, and the health-check prober.
- **Where it's used:** every build and every preview. The image must exist before
  the worker can build.
- **How to get it:** run `make egress-image` once (and after updates); it builds
  `previa-egress:dev`. Release images use `previa-egress:<version>`.
- **Example:** `EGRESS_IMAGE=previa-egress:dev`

### `BUILDKIT_IMAGE`

- **Type:** Optional. Default empty, meaning the pinned rootless BuildKit v0.33.1
  image.
- **What it is:** the BuildKit image started for each build.
- **Where it's used:** every image build.
- **How to get it:** leave it empty. Change it only to another pinned rootless
  BuildKit image (`moby/buildkit:<version>-rootless@sha256:<digest>`).
- **Example:** `BUILDKIT_IMAGE=`

### `SOURCE_WORKSPACE_ROOT`

- **Type:** Optional. Default empty, meaning `previa-sources` in the system
  temporary directory.
- **What it is:** where each build's source code is unpacked while it builds.
- **Where it's used:** every build.
- **How to get it:** leave it empty, or use a full path on a Linux filesystem (not
  `/mnt/c`), for example `/home/<you>/.cache/previa/sources`.
- **Example:** `SOURCE_WORKSPACE_ROOT=`

## Previews (worker)

### `PREVIEW_ALLOW_ROOT`

- **Type:** Optional. Default `false`.
- **What it is:** whether previews may run as the root user. By default they run as
  an unprivileged user (UID 65532).
- **Where it's used:** every preview. `true` weakens isolation for every repository.
- **How to get it:** keep `false`; set `true` only if your apps cannot run otherwise.
- **Example:** `PREVIEW_ALLOW_ROOT=false`

### `PREVIEW_WRITABLE_ROOTFS`

- **Type:** Optional. Default `false`.
- **What it is:** whether previews may write anywhere in their file system. By
  default only `/tmp` is writable.
- **Where it's used:** every preview. `true` weakens isolation for every repository.
- **How to get it:** keep `false`; set `true` only if your apps cannot run otherwise.
- **Example:** `PREVIEW_WRITABLE_ROOTFS=false`

### `PREVIEW_TMPFS_SIZE`

- **Type:** Optional. Default `256MiB`. Between `16MiB` and `4GiB`.
- **What it is:** the size of each preview's writable `/tmp`.
- **Where it's used:** every preview.
- **How to get it:** keep the default; raise it for apps that write large temporary
  files.
- **Example:** `PREVIEW_TMPFS_SIZE=256MiB`

### `PREVIEW_PIDS`

- **Type:** Optional. Default `512`. Between `64` and `65536`.
- **What it is:** processes one preview may run at once.
- **Where it's used:** every preview.
- **How to get it:** keep the default.
- **Example:** `PREVIEW_PIDS=512`

### `PREVIEW_STARTUP_TIMEOUT`

- **Type:** Optional. Default `120s`. Between `5s` and `30m`.
- **What it is:** how long a new preview has to answer its health check.
- **Where it's used:** a slower app fails with "The app did not become ready".
- **How to get it:** keep `120s`; raise it for apps that start slowly.
- **Example:** `PREVIEW_STARTUP_TIMEOUT=120s`

### `PREVIEW_MEMORY_BUDGET`

- **Type:** Optional. Default `0`, meaning 60% of Docker's memory.
- **What it is:** memory all previews may use together.
- **Where it's used:** when full, a new preview waits as **Waiting for capacity**.
- **How to get it:** keep `0`, or set a size such as `8GiB`.
- **Example:** `PREVIEW_MEMORY_BUDGET=0`

### `PREVIEW_CPU_BUDGET`

- **Type:** Optional. Default `0`, meaning 75% of Docker's CPUs.
- **What it is:** CPUs all previews may use together.
- **Where it's used:** when full, a new preview waits as **Waiting for capacity**.
- **How to get it:** keep `0`, or set a number such as `4`.
- **Example:** `PREVIEW_CPU_BUDGET=0`

### `PREVIEW_MAX_PER_REPOSITORY`

- **Type:** Optional. Default `10`. Between `1` and `1000`.
- **What it is:** running previews allowed per repository.
- **Where it's used:** more waits as **Waiting for capacity**.
- **How to get it:** keep the default.
- **Example:** `PREVIEW_MAX_PER_REPOSITORY=10`

### `PREVIEW_OCI_RUNTIME`

- **Type:** Optional. Default empty, meaning Docker's default runtime.
- **What it is:** a different container runtime for previews, for stronger
  isolation (for example gVisor's `runsc`).
- **Where it's used:** every preview.
- **How to get it:** leave it empty. To use gVisor, install it on the Docker host
  ([gvisor.dev](https://gvisor.dev/docs/user_guide/install/)), register it with Docker
  as `runsc`, then set `runsc`.
- **Example:** `PREVIEW_OCI_RUNTIME=`

### `PREVIEW_SERVICE_CATALOG`

- **Type:** Optional. Default empty, meaning Previa's pinned catalog: the official
  PostgreSQL 17 and 16 and Redis 8 and 7 images, each pinned by digest.
- **What it is:** the managed services pull requests may request in
  `.previewrc.yaml` (`services: {postgres: 17}`), as `kind:version=image@sha256:digest`
  pairs separated by commas; `none` offers none.
- **Where it's used:** the worker, when it validates a commit's configuration and
  starts its services ([docs/11](../docs/11-backend-previews.md)).
- **How to get it:** leave it empty. To pin newer images, read each index digest
  with `docker buildx imagetools inspect postgres:17-alpine` (the `Digest:` line) and
  write, for example, `postgres:17=docker.io/library/postgres:17-alpine@sha256:<digest>`.
- **Example:** `PREVIEW_SERVICE_CATALOG=`

### `PREVIEW_SETUP_TIMEOUT`

- **Type:** Optional. Default `5m` (10s to 30m).
- **What it is:** how long a preview's setup command (migrations, seed data) may run.
- **Where it's used:** the worker, for every candidate that declares `setup`.
- **How to get it:** keep the default unless migrations are slow.
- **Example:** `PREVIEW_SETUP_TIMEOUT=5m`

### `PREVIEW_OUTBOUND_PORTS`

- **Type:** Optional. Default `5432,3306,6379,6380,27017,5671,5672,9092,1433`.
- **What it is:** the destination ports repository owners may approve on the
  Outbound tab for external databases and brokers; `none` turns the feature off.
  At most 16 ports; never 80, 443 (previews already reach HTTP and HTTPS through the
  proxy) or 3128 (the proxy's own port).
- **Where it's used:** the server (which approvals it accepts, and the Outbound tab)
  and the worker (which ports the egress proxy forwards). Set the same value for both.
- **How to get it:** list the ports of the external services your previews need.
- **Example:** `PREVIEW_OUTBOUND_PORTS=5432,3306,6379,6380,27017,5671,5672,9092,1433`

## Preview domain and router

### `PREVIEW_BASE_DOMAIN`

- **Type:** Required for builds and previews. No default; empty means the worker
  only tracks pull requests and the Build buttons explain that previews are not
  configured.
- **What it is:** the domain every preview address lives under:
  `https://pr-<number>-<repo>-<token>.<PREVIEW_BASE_DOMAIN>`.
- **Where it's used:** the **Build** buttons, preview addresses on the pull request
  page, **Environments**, Overview and the PR comment, and the router's
  certificate. It must not be the dashboard's host or a parent of it.
- **How to get it (one machine):** use `preview.localtest.me`. Every
  `*.localtest.me` name already points at your own computer, so no DNS is needed.
- **How to get it (real domain):**
  1. Pick a domain you control, ideally a different registered domain from the
     dashboard's (for example `preview.example.net`).
  2. At your DNS provider, add an `A` record named `*.preview` (for
     `*.preview.example.net`) pointing at the server's public IP address. On
     Cloudflare, set **Proxy status** to **DNS only**.
  3. Make sure ports 80 and 443 of that server are reachable.
  4. Set up the router settings from [ACME_EMAIL](#acme_email) onwards.
- **Example:** `PREVIEW_BASE_DOMAIN=preview.localtest.me`

### `ROUTER_DYNAMIC_DIR`

- **Type:** Required for builds and previews. No default.
- **What it is:** the folder the worker writes the routing table into and the
  router reads it from.
- **Where it's used:** publishing every preview at its address and removing it on
  teardown.
- **How to get it:**
  1. Choose a full path on the Linux filesystem, for example
     `/home/<you>/.local/share/previa/router` (run `echo $HOME` for your home). Do
     not use `~`, a path under `/mnt/c`, or a trailing slash.
  2. Paste it here. `make router-up` creates the folder and starts the router.
  3. In production the Compose file uses `/var/lib/previa/router`; see
     [PREVIA_ROUTER_DIR](#previa_router_dir).
- **Example:** `ROUTER_DYNAMIC_DIR=/home/ajay/.local/share/previa/router`

### `ROUTER_CONTAINER`

- **Type:** Optional. Default `previa-router`.
- **What it is:** the name of the router (Traefik) container.
- **Where it's used:** `make router-up` names the container; the worker checks it to
  confirm each preview is being served.
- **How to get it:** keep the default.
- **Example:** `ROUTER_CONTAINER=previa-router`

### `ROUTER_ENTRYPOINT`

- **Type:** Optional. Default `websecure`.
- **What it is:** the router's HTTPS entry point name.
- **Where it's used:** every preview route.
- **How to get it:** keep the default.
- **Example:** `ROUTER_ENTRYPOINT=websecure`

### `ROUTER_API_ADDRESS`

- **Type:** Optional. Default `127.0.0.1:8080`. Must be a loopback address.
- **What it is:** the router's API address inside its own container (never
  published).
- **Where it's used:** the worker confirms a preview is live before calling it
  **Ready**.
- **How to get it:** keep the default.
- **Example:** `ROUTER_API_ADDRESS=127.0.0.1:8080`

### `ACME_EMAIL`

- **Type:** Required for previews (`make router-up` refuses without it).
- **What it is:** the e-mail address of the Let's Encrypt account that issues the
  preview certificate.
- **Where it's used:** the router's HTTPS certificate for preview addresses (and, in
  production, the dashboard's).
- **How to get it:** your own e-mail address with a real domain. On one machine with
  `preview.localtest.me` no certificate is requested, so any address works.
- **Example:** `ACME_EMAIL=you@example.com`

### `ACME_DNS_PROVIDER`

- **Type:** Required for previews (`make router-up` refuses without it).
- **What it is:** the DNS provider that proves you own the preview domain, so a
  wildcard certificate can be issued.
- **Where it's used:** the router's certificate for preview addresses.
- **How to get it:**
  1. Use the company that hosts your domain's DNS, by its name in
     [lego's DNS provider list](https://go-acme.github.io/lego/dns/), for example
     `cloudflare`, which this guide uses.
  2. Choose a provider whose lego entry needs a single API token.
  3. On one machine with `preview.localtest.me`, keep `cloudflare` (nothing is
     contacted).
- **Example:** `ACME_DNS_PROVIDER=cloudflare`

### `ACME_DNS_CREDENTIAL_VARIABLE`

- **Type:** Required for previews (`make router-up` refuses without it).
- **What it is:** the name of the variable that provider reads its API token from.
- **Where it's used:** the router's certificate requests.
- **How to get it:** open your provider's page in
  [lego's list](https://go-acme.github.io/lego/dns/) and copy the token variable's
  name: `CF_DNS_API_TOKEN` for Cloudflare; other providers name theirs in the lego list.
- **Example:** `ACME_DNS_CREDENTIAL_VARIABLE=CF_DNS_API_TOKEN`

### `ACME_DNS_CREDENTIAL_FILE`

- **Type:** Required for previews (`make router-up` refuses without it, and the file
  must exist with mode 600).
- **What it is:** the full path of a file holding only the DNS provider's API token.
- **Where it's used:** the router reads the token from it to prove domain ownership.
- **How to get it (one machine):**
  1. Run `mkdir -p ~/.config/previa && printf x > ~/.config/previa/dns-credential && chmod 600 ~/.config/previa/dns-credential`.
  2. Paste the full path (no `~`), for example `/home/<you>/.config/previa/dns-credential`.
- **How to get it (real domain, Cloudflare):**
  1. Sign in at [dash.cloudflare.com](https://dash.cloudflare.com); your preview
     domain must be a zone there.
  2. Profile icon (top right) → **My Profile** → **API Tokens** → **Create Token**.
  3. Use the **Edit zone DNS** template.
  4. **Zone Resources:** **Include → Specific zone →** your domain.
  5. **Continue to summary → Create Token**, then copy the token (shown once).
  6. Save it without leaving it in your shell history:
     `read -rs TOKEN; printf '%s' "$TOKEN" > ~/.config/previa/dns-credential; chmod 600 ~/.config/previa/dns-credential; unset TOKEN`
     (paste the token after the first command and press Enter).
  7. In production use `/etc/previa/dns-credential` (owned by root, mode 600).
- **Example:** `ACME_DNS_CREDENTIAL_FILE=/home/ajay/.config/previa/dns-credential`

### `ACME_CA_SERVER`

- **Type:** Required for previews. Without it the router uses Let's Encrypt
  staging, which only works with a real domain.
- **What it is:** where the router requests certificates.
- **Where it's used:** the preview certificate (and, in production, the dashboard's).
- **How to get it:**
  1. One machine (`preview.localtest.me`): `https://127.0.0.1:1/directory`, which
     requests nothing. Browsers then warn about the router's own certificate;
     accept it for testing.
  2. Real domain, first: Let's Encrypt staging,
     `https://acme-staging-v02.api.letsencrypt.org/directory`, to test without rate
     limits.
  3. Real domain, once staging issued a certificate:
     `https://acme-v02.api.letsencrypt.org/directory`.
- **Example:** `ACME_CA_SERVER=https://127.0.0.1:1/directory`

### `ACME_DNS_RESOLVERS`

- **Type:** Optional. Default `1.1.1.1:53,8.8.8.8:53`.
- **What it is:** DNS servers the router uses to check its domain challenge.
- **Where it's used:** certificate issuance.
- **How to get it:** keep the default.
- **Example:** `ACME_DNS_RESOLVERS=1.1.1.1:53,8.8.8.8:53`

## Housekeeping (worker)

### `ENV_TTL_HOURS`

- **Type:** Optional. Default `48`. `0` means previews never expire; at most `8760`.
- **What it is:** how many hours a preview lives after its latest publication.
- **Where it's used:** **Expires** on the pull request page and **Environments**,
  and the "Preview expired" PR comment. The next build brings it back at the same
  address.
- **How to get it:** keep `48`, or choose another number of hours.
- **Example:** `ENV_TTL_HOURS=48`

### `HOUSEKEEPING_DRY_RUN`

- **Type:** Optional. Default `false`.
- **What it is:** `true` makes housekeeping log what it would delete (leftover
  containers, expired previews, old images and caches) without deleting it.
- **Where it's used:** the worker's periodic cleanup.
- **How to get it:** keep `false`; use `true` while checking what cleanup would do.
- **Example:** `HOUSEKEEPING_DRY_RUN=false`

### `HOUSEKEEPING_GRACE`

- **Type:** Optional. Default `15m`. Between `1m` and `24h`.
- **What it is:** how old an unused container, image or network must be before
  housekeeping removes it.
- **Where it's used:** the worker's periodic cleanup.
- **How to get it:** keep the default.
- **Example:** `HOUSEKEEPING_GRACE=15m`

### `BUILD_CACHE_BUDGET`

- **Type:** Optional. Default `40GiB`. At least `1GiB`.
- **What it is:** disk space all build caches may use together.
- **Where it's used:** the oldest unused caches are pruned first; caches make
  repeated builds faster.
- **How to get it:** keep the default; lower it on a small disk.
- **Example:** `BUILD_CACHE_BUDGET=40GiB`

## Tests and tooling

### `TEST_DATABASE_URL`

- **Type:** Optional. Default empty; the `make test-*` suites then use
  [DATABASE_URL](#database_url).
- **What it is:** the PostgreSQL the integration tests use. They create and drop
  their own schemas.
- **Where it's used:** `make test-*` suites, and `go test` (which skips database
  tests without it).
- **How to get it:** leave it empty, or point it at a separate database, for
  example `postgres://previa:change-me@localhost:5432/previa_test?sslmode=disable`
  after `createdb previa_test`.
- **Example:** `TEST_DATABASE_URL=`

### `TEST_GITHUB_INSTALLATION_ID`

- **Type:** Optional. Default empty (the live checks refuse to run).
- **What it is:** a real installation of your GitHub App.
- **Where it's used:** `make test-github-app`, `make test-source`,
  `make test-reporting` (live checks against GitHub).
- **How to get it:** install your App on a test repository, then open
  **github.com → Settings → Applications → Installed GitHub Apps →** your App →
  **Configure**. The number at the end of the address
  (`github.com/settings/installations/<id>`) is the ID.
- **Example:** `TEST_GITHUB_INSTALLATION_ID=12345678`

### `TEST_GITHUB_REPO_ID`

- **Type:** Optional. Default empty.
- **What it is:** the numeric ID of a repository that installation can access.
- **Where it's used:** the same live checks.
- **How to get it:** run `gh api repos/<owner>/<repo> --jq .id` (GitHub CLI), or
  open `https://api.github.com/repos/<owner>/<repo>` and copy `"id"`.
- **Example:** `TEST_GITHUB_REPO_ID=987654321`

### `TEST_GITHUB_PR_NUMBER`

- **Type:** Optional. Default empty.
- **What it is:** an open pull request in that repository.
- **Where it's used:** the same live checks; `make test-reporting` writes a comment
  and check runs to it, so use a disposable pull request.
- **How to get it:** open a throwaway pull request and copy its number from the
  address (`…/pull/<number>`).
- **Example:** `TEST_GITHUB_PR_NUMBER=1`

### `TEST_E2E_LOG`

- **Type:** Optional. Default `0`.
- **What it is:** `1` prints the worker's and router's logs during `make test-e2e`.
- **Where it's used:** investigating a failing end-to-end test.
- **How to get it:** set `1` when needed.
- **Example:** `TEST_E2E_LOG=0`

### `TEST_ROUTING_LOG`

- **Type:** Optional. Default `0`.
- **What it is:** `1` prints the router's logs during `make test-routing`.
- **Where it's used:** investigating a failing routing test.
- **How to get it:** set `1` when needed.
- **Example:** `TEST_ROUTING_LOG=0`

### `WORKSPACES`

- **Type:** Optional. Default empty.
- **What it is:** an extra source workspace folder for the housekeeping dry run to
  inspect.
- **Where it's used:** `make housekeeping-dry-run`.
- **How to get it:** leave empty, or use the folder from
  [SOURCE_WORKSPACE_ROOT](#source_workspace_root).
- **Example:** `WORKSPACES=`

### `ENV_FILE`

- **Type:** Optional, command line only. Default `.env`.
- **What it is:** another settings file for the `make` commands to load.
- **Where it's used:** running with a second configuration, for example
  `ENV_FILE=.env.local make run-server`. Setting it inside `.env` has no effect.
- **How to get it:** copy `.env` to the new name, edit it, and type
  `ENV_FILE=<file>` before the `make` command.
- **Example:** `ENV_FILE=.env.local make run-worker`

### `UI_BROWSER_EXECUTABLE`

- **Type:** Optional, command line only. Default: Playwright's own Chromium.
- **What it is:** a Chromium already installed on your machine for the browser
  tests.
- **Where it's used:** `make test-browser`.
- **How to get it:** run `which chromium` (on Ubuntu/WSL often `/snap/bin/chromium`),
  or run `make browser-install` to use Playwright's instead.
- **Example:** `UI_BROWSER_EXECUTABLE=/snap/bin/chromium make test-browser`

### `UI_BROWSER_SCREENSHOTS`

- **Type:** Optional, command line only. Default off.
- **What it is:** `1` saves a screenshot of every browser test to
  `web/tests/browser/artifacts/`.
- **Where it's used:** `make test-browser`.
- **How to get it:** type it before the command when you want screenshots.
- **Example:** `UI_BROWSER_SCREENSHOTS=1 make test-browser`

### `PREVIA_VERSION`

- **Type:** Required in production; optional on the command line (default `dev`).
- **What it is:** the release version, used as the tag of the server, worker and
  egress images.
- **Where it's used:** production Compose (which images run, and the version shown
  in `/readyz`), `make release-images` and `make prod-smoke`.
- **How to get it:** use the release tag you deploy (from the repository's
  **Releases** page, for example `v0.1.0`), or any tag for local image builds.
- **Example:** `PREVIA_VERSION=v0.1.0`

### `SMOKE_OLD_DUMP`

- **Type:** Optional, command line only. Default empty.
- **What it is:** a `pg_dump -Fc` file from an older release, to rehearse restoring
  and upgrading it.
- **Where it's used:** `make prod-smoke`.
- **How to get it:** `pg_dump -Fc "$DATABASE_URL" > /home/<you>/backups/previa-old.dump`
  on a database from the older release.
- **Example:** `SMOKE_OLD_DUMP=/home/ajay/backups/previa-old.dump make prod-smoke PREVIA_VERSION=v0.1.0`

### `SMOKE_KEEP`

- **Type:** Optional, command line only. Default off.
- **What it is:** `1` leaves the smoke-test stack running when it fails, to inspect
  it.
- **Where it's used:** `make prod-smoke`.
- **How to get it:** type it before the command when needed.
- **Example:** `SMOKE_KEEP=1 make prod-smoke PREVIA_VERSION=v0.1.0`

## Production only

### `PREVIA_SERVER_IMAGE`

- **Type:** Optional. Default `previa-server` (an image built on the host).
- **What it is:** the image repository of the dashboard server.
- **Where it's used:** production Compose runs `<this>:<PREVIA_VERSION>`.
- **How to get it:** leave the default when building on the host, or use your
  registry's path, for example `ghcr.io/<owner>/previa-server`.
- **Example:** `PREVIA_SERVER_IMAGE=ghcr.io/ajaykathwatee/previa-server`

### `PREVIA_WORKER_IMAGE`

- **Type:** Optional. Default `previa-worker`.
- **What it is:** the image repository of the worker.
- **Where it's used:** production Compose runs `<this>:<PREVIA_VERSION>`.
- **How to get it:** as for [PREVIA_SERVER_IMAGE](#previa_server_image).
- **Example:** `PREVIA_WORKER_IMAGE=ghcr.io/ajaykathwatee/previa-worker`

### `PREVIA_EGRESS_IMAGE`

- **Type:** Optional. Default `previa-egress`.
- **What it is:** the image repository of the egress proxy.
- **Where it's used:** production Compose runs `<this>:<PREVIA_VERSION>` and gives
  it to the worker as its egress image.
- **How to get it:** as for [PREVIA_SERVER_IMAGE](#previa_server_image).
- **Example:** `PREVIA_EGRESS_IMAGE=ghcr.io/ajaykathwatee/previa-egress`

### `DOCKER_GID`

- **Type:** Required in production. No default.
- **What it is:** the group ID that owns the Docker socket on the host.
- **Where it's used:** lets the worker container (and only it) control Docker to
  build and run previews.
- **How to get it:** `deploy/production/setup.sh` fills it in. By hand:
  `stat -c %g /var/run/docker.sock`, and paste the number.
- **Example:** `DOCKER_GID=988`

### `POSTGRES_PASSWORD`

- **Type:** Required in production. No default.
- **What it is:** the password of the production database.
- **Where it's used:** the `postgres` service, and inside
  [DATABASE_URL](#database_url).
- **How to get it:** `deploy/production/setup.sh` generates it. By hand: run
  `openssl rand -hex 24`, paste it here and in place of `REPLACE_WITH_POSTGRES_PASSWORD`
  in DATABASE_URL.
- **Example:** `POSTGRES_PASSWORD=5f1e…` (48 hex characters)

### `PREVIA_DASHBOARD_HOST`

- **Type:** Required in production. No default.
- **What it is:** the host name of the dashboard, the host part of
  [APP_BASE_URL](#app_base_url).
- **Where it's used:** the router serves the dashboard on this name and gets its
  HTTPS certificate.
- **How to get it:**
  1. Choose the name, for example `previa.example.com`.
  2. At your DNS provider, add an `A` record for it pointing at the server's public
     IP address (on Cloudflare, **DNS only**).
  3. Paste the name here, without `https://`.
- **Example:** `PREVIA_DASHBOARD_HOST=previa.example.com`

### `GITHUB_APP_PRIVATE_KEY_FILE`

- **Type:** Optional. Default `/etc/previa/github-app.pem`.
- **What it is:** where the GitHub App's private key file is on the production host.
- **Where it's used:** given to the server and worker containers as their App key.
- **How to get it:** download the key as in
  [GITHUB_APP_PRIVATE_KEY_PATH](#github_app_private_key_path), copy it to the host
  (for example with `scp`), then
  `sudo install -o 65532 -g 65532 -m 600 <key>.pem /etc/previa/github-app.pem`.
- **Example:** `GITHUB_APP_PRIVATE_KEY_FILE=/etc/previa/github-app.pem`

### `BACKUP_AGE_RECIPIENT`

- **Type:** Required for production backups. No default.
- **What it is:** an [age](https://github.com/FiloSottile/age) public key; every
  backup is encrypted to it before leaving the server.
- **Where it's used:** the nightly backup (`previa-backup.timer`).
- **How to get it:**
  1. On a trusted computer (not the server), install age (`sudo apt install age`
     or `brew install age`).
  2. Run `age-keygen -o previa-backup.key`. It prints `Public key: age1…`.
  3. Paste the `age1…` value here.
  4. Keep `previa-backup.key` offline and safe: it is the only way to read the
     backups.
- **Example:** `BACKUP_AGE_RECIPIENT=age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p`

### `BACKUP_REMOTE`

- **Type:** Required for production backups. No default.
- **What it is:** where backups are stored: an rclone remote (for example an S3
  bucket) or a full folder path.
- **Where it's used:** the nightly backup and `restore.sh`.
- **How to get it:**
  1. For cloud storage, run `sudo rclone config` on the server (the backup runs as
     root): **n** (new remote), give it a name such as `offsite`, choose the
     storage type (for example `s3`, `b2` or `drive`) and enter that service's
     access key and secret.
  2. Create a bucket or folder there, for example `previa-backups`.
  3. Paste `<remote name>:<bucket or path>`.
  4. Or use a full local folder path (ideally another disk), for example
     `/mnt/backup/previa`.
- **Example:** `BACKUP_REMOTE=offsite:previa-backups`

### `BACKUP_RETENTION_DAYS`

- **Type:** Optional. Default `30`.
- **What it is:** how many days backups are kept before they are deleted.
- **Where it's used:** the nightly backup.
- **How to get it:** keep `30`, or any positive number of days.
- **Example:** `BACKUP_RETENTION_DAYS=30`

### `BACKUP_FAILURE_WEBHOOK`

- **Type:** Optional. Default empty.
- **What it is:** a URL that receives a message when a backup fails.
- **Where it's used:** the nightly backup.
- **How to get it:** create an incoming webhook in your chat tool, for example in
  Slack: **Apps → Incoming Webhooks → Add to Slack →** choose a channel → copy the
  **Webhook URL**.
- **Example:** `BACKUP_FAILURE_WEBHOOK=https://hooks.slack.com/services/T000/B000/XXXX`

### `GRAFANA_ADMIN_PASSWORD`

- **Type:** Optional; needed when you run the `observability` profile.
- **What it is:** the password of Grafana's `admin` user.
- **Where it's used:** the Grafana dashboard at `127.0.0.1:3000` on the server (open
  it through `ssh -L 3000:127.0.0.1:3000 <server>`).
- **How to get it:** `deploy/production/setup.sh` generates it. By hand: run
  `openssl rand -hex 16` and paste the output.
- **Example:** `GRAFANA_ADMIN_PASSWORD=8c1d…` (32 hex characters)

### `PREVIA_ENV_FILE`

- **Type:** Optional, command line only. Default `/etc/previa/previa.env`.
- **What it is:** which production settings file Compose and the backup and restore
  scripts read.
- **Where it's used:** production Compose, `backup.sh`, `restore.sh`.
- **How to get it:** keep the default; type it before a command to use another file.
- **Example:** `PREVIA_ENV_FILE=/etc/previa/previa.env`

### `PREVIA_ROUTER_DIR`

- **Type:** Optional, command line only. Default `/var/lib/previa/router`.
- **What it is:** the host folder shared between the production worker and router
  for the routing table.
- **Where it's used:** production Compose.
- **How to get it:** keep the default.
- **Example:** `PREVIA_ROUTER_DIR=/var/lib/previa/router`

### `PREVIA_BACKUP_STATUS_DIR`

- **Type:** Optional, command line only. Default `/var/lib/previa/backup`.
- **What it is:** the host folder where the backup script records its last success.
- **Where it's used:** the backup metric and alert.
- **How to get it:** keep the default.
- **Example:** `PREVIA_BACKUP_STATUS_DIR=/var/lib/previa/backup`
