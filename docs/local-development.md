# Local database development

The application and pinned tools use Go 1.26.5. PostgreSQL runs through
`deploy/docker-compose.yml`. It listens only on `127.0.0.1:5432` and uses
the local username, password, and database in `.env.example`.

## Docker on Ubuntu 24.04 in WSL 2

Choose either Docker Desktop integration or a native Docker Engine installation.
Homebrew is not needed for either setup.

### Docker Desktop integration

If Docker Desktop is already installed on Windows, start it. Otherwise install
[Docker Desktop for Windows](https://docs.docker.com/desktop/setup/install/windows-install/).
In Settings, enable the WSL 2 engine under General, then enable this Ubuntu
distribution under Resources > WSL Integration and apply the changes.

Reopen the WSL terminal and check:

```sh
docker version
docker compose version
```

If Docker reports socket permission denied, compare `id -nG` with
`getent group docker`. If the group lists your username but `id -nG` does not
include `docker`, run `newgrp docker` in that terminal and retry `docker version`.
The existing session has not picked up the group membership yet.

See [Docker's WSL integration instructions](https://docs.docker.com/desktop/features/wsl/).

### Native Docker Engine in WSL

Use this option if you want Docker to run directly inside Ubuntu. Disable Docker
Desktop integration for this distro first if it was enabled.

Check `ps -p 1 -o comm=` in your WSL terminal. If it does not show `systemd`, edit
`/etc/wsl.conf` using `sudoedit`, preserving any existing settings, and enable:

```ini
[boot]
systemd=true
```

Run `wsl --shutdown` in Windows PowerShell, then reopen Ubuntu. This stops all
running WSL distributions. See [Microsoft's systemd setup guide](https://learn.microsoft.com/windows/wsl/systemd).

Install from Docker's official Ubuntu repository:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl --fail --silent --show-error --location \
  https://download.docker.com/linux/ubuntu/gpg \
  --output /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

# This repository entry is for this workspace's Ubuntu 24.04 (Noble).
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n' \
  "$(dpkg --print-architecture)" | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
newgrp docker
docker version
docker compose version
```

The Docker group allows control of the host's Docker daemon. The package setup
follows [Docker's Ubuntu installation guide](https://docs.docker.com/engine/install/ubuntu/).

## Start PostgreSQL and migrate

Run from the repository root:

```sh
# Create local settings if you have not already done so.
test -f .env || cp .env.example .env
docker compose -f deploy/docker-compose.yml up -d --wait
make migrate-up
```

The named volume retains data across container restarts. A fresh volume starts
with an empty application database. `make migrate-up` uses exported
`DATABASE_URL` first, then `.env`, then `.env.example` if `.env` is absent.
Use `ENV_FILE=.env.local` to select a different local file. Env files are sourced
as shell files: quote values containing shell metacharacters or whitespace.

The Go config loader only reads process environment variables. To load local
values into your shell, use `set -a; . ./.env; set +a`. All 12 application variables
are required. The private-key path is loaded but not opened in this phase.
`SESSION_SECRET` must have at least 32 characters; `SECRETS_ENCRYPTION_KEY` must
decode from base64 to exactly 32 bytes. The example values are placeholders.

## Verify the schema and pool

After migration:

```sh
make build test lint
TEST_DATABASE_URL='postgres://previa:change-me@localhost:5432/previa?sslmode=disable' \
  go test -count=1 -v ./internal/platform/database
```

Without `TEST_DATABASE_URL`, database integration tests explicitly skip. With it,
they check pool connectivity, context cancellation, foreign keys, valid statuses,
uniqueness, and active-environment constraints. Fixture rows are rolled back.

On a disposable development database, check rollback/reapply with:

```sh
make migrate-down
make migrate-up
```

The down migration drops all seven core tables and their data. Do not run it
against a database whose data you need to preserve.

## Initial schema

The complete SQL is in [0001_init.sql](../db/migrations/0001_init.sql).
It creates `users`, `installations`, `repos`, `pull_requests`, `environments`,
`build_logs`, and `env_secrets` with foreign keys and timestamps. Subdomains are
globally unique, and `(repo_id, number)` identifies a pull request uniquely.

Environment statuses are `queued`, `building`, `running`, `failed`,
`tearing_down`, and `torn_down`. All except `torn_down` retain the unique active
slot for a pull request, including failed environments that may own resources.
Allowed state transitions will be checked by future guarded update statements;
this phase only constrains status values. No queries or application table access
are implemented.

## Phase 2 verification

Verified on 2026-09-29 against Docker Desktop through WSL integration:

- Compose configuration validation and `up -d --wait` succeeded using a newly
  created `previa_postgres_data` volume. PostgreSQL was healthy, and the public
  schema contained zero tables before the first migration.
- `make migrate-up` applied `0001_init.sql` successfully at version 1.
- All tests passed with `TEST_DATABASE_URL` set, including the pool and schema
  integration tests. No tests were skipped; packages without tests reported that.
- All seven core tables contained zero rows after the test transaction rolled back.
- `make migrate-down` removed all seven tables, leaving only goose bookkeeping
  at version 0. Reapplying succeeded; a second `make migrate-up` was a no-op.
- `go build ./...`, `go vet ./...`, and the pinned `golangci-lint run` passed.

PostgreSQL was left running with the initial schema applied at version 1.
