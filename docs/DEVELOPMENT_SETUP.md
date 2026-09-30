# Development setup

Follow these steps to configure, build, and run Previa locally.
Run commands from the repository root
in Bash or Zsh (on Windows, use WSL2 with Docker integration enabled).

## 1. Prerequisites

- Go **1.26.5 or newer**, as required by `go.mod` and `tools/go.mod`.
- Git and GNU Make.
- Docker Engine with the Compose plugin, or Docker Desktop running Linux containers.
- Internet access for the first dependency and container image downloads.

Confirm the tools are available:

```sh
git --version
go version
make --version
docker compose version
```

Each command should print its version. No separate PostgreSQL installation, Node.js,
or globally installed Go development tools are required.

## 2. Get the code and configuration

If you have not already cloned the repository, replace the placeholder with its
clone URL:

```sh
git clone <repository-clone-url> Previa
cd Previa
```

Create your local configuration once (skip the copy if `.env` already exists):

```sh
cp .env.example .env
go mod download
go mod download -modfile=tools/go.mod
```

- Successful downloads normally produce no output; the tools module may take longer.
- `.env` is Git-ignored. Its default `DATABASE_URL` matches the local Compose database:
  `postgres://previa:change-me@localhost:5432/previa?sslmode=disable`.
- Review `.env` and replace placeholder credentials for the integrations you use.
  `.env.example` groups shared, server-only, and worker-only settings. The server
  needs `DATABASE_URL`, OAuth client ID/secret, and `SESSION_SECRET` (at least 32
  characters); HTTP address/origin have local defaults. The worker needs only
  `DATABASE_URL` and positive `MAX_CONCURRENT_BUILDS`; optional `BUILD_TIMEOUT`
  defaults to `12m`. Keep secrets out of Git.
- Tool versions (Goose, templ, sqlc, golangci-lint, and Air) are pinned in
  `tools/go.mod`; invoke them with `go tool -modfile=tools/go.mod <tool>`.

## 3. Start Docker and PostgreSQL

Start Docker Desktop, or on a Linux host using systemd:

```sh
sudo systemctl start docker
```

Verify access before starting the database:

```sh
docker version
docker compose -f deploy/docker-compose.yml up -d --wait postgres
docker compose -f deploy/docker-compose.yml ps
```

**Expected:** `docker version` shows both Client and Server information. Compose
starts PostgreSQL 17 and reports `postgres` as running and healthy, with port
`127.0.0.1:5432` published. The first run downloads `postgres:17-alpine`.

Confirm PostgreSQL accepts connections:

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U previa -d previa
```

**Expected:** `accepting connections`. Compose runs PostgreSQL;
Go commands run on your host. Database data persists in a Docker named volume.

## 4. Apply and verify the schema

```sh
make migrate-up
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa -c '\dt'
```

**Expected:** Goose and River apply pending migrations (or report none pending).
The table list shows application tables, `goose_db_version`, and River's `river_*` tables.
Re-running `make migrate-up` is safe; it applies only pending migrations.

The migration Make targets load `.env` automatically, falling back to `.env.example`
if it is absent. An already exported `DATABASE_URL` takes precedence over either file.

To run migrations manually, first export your local configuration:

```sh
set -a
. ./.env
set +a
GOOSE_DRIVER=postgres GOOSE_DBSTRING="$DATABASE_URL" go tool -modfile=tools/go.mod goose -env=none -dir db/migrations up
go run ./cmd/river-migrate up
```

Use that same Goose environment prefix with `status` in place of `up` to inspect
application migrations; use `go run ./cmd/river-migrate status` or `make river-status`
for River. The River adapter reads `DATABASE_URL` directly; Goose reads
`GOOSE_DBSTRING`. Neither command receives the connection string as an argument. Application configuration reads exported variables, not `.env` directly;
repeat the export block in each new terminal that needs them. Keep `.env` in shell
assignment syntax and quote values containing spaces or shell special characters.

## 5. Build and check the project

```sh
make generate
make build
make lint
make test
```

- `generate`: runs templ and, when query entries are configured in `sqlc.yaml`,
  sqlc. Expect generation to complete without errors.
- `build`: compiles all packages; success normally has no additional output.
- `lint`: runs `go vet` and the pinned golangci-lint; expect no reported issues.
- `test`: runs the existing suite; expect `ok` for tested packages and
  `[no test files]` for packages without tests. Database integration tests skip unless
  `TEST_DATABASE_URL` is set.

Run the database integration tests after migrations, using the local development DB:

```sh
set -a
. ./.env
set +a
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 -v ./...
```

**Expected:** `TestPoolIntegration` and `TestMigrationConstraintsIntegration` pass
rather than skip. The schema test rolls back its inserted data. `-count=1` forces
a fresh run instead of using cached results.

Manual equivalents of the main Make targets:

| Make command | Underlying command |
| --- | --- |
| `make build` | Generate templates, then `go build ./...` |
| `make test` | Generate templates, then `go test ./...` |
| `make lint` | Generate templates, then `go vet ./...` and `go tool -modfile=tools/go.mod golangci-lint run` |
| `make generate` | `go tool -modfile=tools/go.mod templ generate`; sqlc generation is conditional as described above |
| `make run-server` | Generate templates, then `go run ./cmd/server` |
| `make run-worker` | Generate templates, then `go run ./cmd/worker` |
| `make dev` | `go tool -modfile=tools/go.mod air -c .air.toml` |

## 6. Run the application

In one terminal, load your configuration and start the server:

```sh
set -a
. ./.env
set +a
make run-server
```

Open <http://localhost:8080/>. Successful startup logs `database health check passed`
followed by `HTTP server listening`. The server checks PostgreSQL before accepting
requests; connection failures stop startup. Set `HTTP_ADDR` to change the listen
address (default `127.0.0.1:8080`).

For GitHub OAuth App registration, callback configuration, and the full sign-in
test flow, follow [Test GitHub sign-in locally](AUTH_TESTING.md).

Start the worker in a second terminal:

```sh
set -a
. ./.env
set +a
make run-worker
```

**Expected:** `database health check passed`, then `River worker started`.
The server inserts jobs; the separate worker processes them. Keep both running
against the same database. `MAX_CONCURRENT_BUILDS` sets concurrency per worker
process. Stop the worker with `Ctrl+C`; it drains active work before exiting.

For live reload, stop the server and run:

```sh
make dev
```

Air loads `.env`, watches Go, templ, CSS, and JavaScript files, regenerates templates,
rebuilds, and restarts the server on changes. Polling supports WSL-mounted directories.
Generated template files are excluded from watching to prevent rebuild loops.
Refresh the browser after a rebuild to see the change. Check the terminal for startup
or compilation errors. Use `Ctrl+C` to stop the process.
Air reloads the server; restart the worker separately after changes to worker code.

## 7. Daily workflow and shutdown

- Start or resume PostgreSQL with the Compose `up -d --wait postgres` command above.
- After pulling changes, run `make migrate-up`; run `make generate` when needed.
- Make changes, then run `make build`, `make lint`, and `make test`. For database
  changes, also run the integration command from step 5.
- Stop the local database when finished:

  ```sh
  docker compose -f deploy/docker-compose.yml down
  ```

  This removes the container and retains its data volume. Avoid `down -v` unless
  you intend to delete the local database. `make migrate-down` rolls back the most
  recent application migration; the initial migration's rollback drops application
  tables. It does not roll back River's independently managed schema.

## Troubleshooting

- **Docker connection or permission error:** start the daemon/Desktop and confirm
  `docker version` shows its Server section. On Linux, ensure your account is allowed
  to use Docker; after a Docker group change, start a new login session. On WSL2,
  check Docker Desktop's integration for your distribution.
- **Port 5432 already in use:** stop the conflicting local database, or change the
  host port in `deploy/docker-compose.yml` and the port in `.env`'s `DATABASE_URL`
  together. PostgreSQL's internal container port remains 5432.
- **Database not healthy or migration connection failure:** check
  `docker compose -f deploy/docker-compose.yml logs postgres`, confirm step 3 passes,
  and check for an exported `DATABASE_URL` overriding `.env`.
- **Password authentication failed:** Compose credentials are defined in the YAML,
  not read from `.env`. Keep them consistent. Changing Compose credentials does not
  update an already initialized database volume's password.
- **Missing tables or skipped integration tests:** run `make migrate-up`, then use
  the explicit `TEST_DATABASE_URL` command from step 5.
- **Go version or tool errors:** confirm `go version` meets both modules' requirement
  and re-run their dependency downloads. The first tool invocation may take time to
  compile; a separate global tool installation is unnecessary.
