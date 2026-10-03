# Development setup

Follow these steps to configure, build, and run Previa locally.
Run commands from the repository root
in Bash or Zsh (on Windows, use WSL2 with Docker integration enabled).

## 1. Prerequisites

- Go **1.26.5 or newer**, as required by `go.mod` and `tools/go.mod`.
- Git, GNU Make, curl, a SHA-256 tool (`sha256sum` or `shasum`), and OpenSSL (to generate the local OAuth-state secret).
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
or globally installed Go development tools are required. Official Tailwind CSS
**v4.1.18** is downloaded once into ignored `bin/` by asset/build/server targets
and verified against pinned SHA-256 digests. Linux/WSL2 and macOS x64/arm64 are
supported; the first asset build downloads a roughly 115 MB binary on Linux x64.

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
- Replace the OAuth and session-secret placeholders before testing sign-in.
  Keep secrets out of Git; do not print or share the contents of `.env`.
- Tool versions (Goose, templ, sqlc, golangci-lint, and Air) are pinned in
  `tools/go.mod`; invoke them with `go tool -modfile=tools/go.mod <tool>`.

The current configuration is:

| Variable | How to obtain or choose it | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | Keep the example for the included Compose database. Change its credentials/host port together with Compose if needed. | Application data, sessions, and River jobs; shared by server, worker, and migrations. |
| `HTTP_ADDR` | Keep `127.0.0.1:8080` locally. | Server listen address. |
| `APP_BASE_URL` | Keep `http://localhost:8080` locally. | Browser origin for OAuth redirects, CSRF origin checks, and cookie security. |
| `GITHUB_OAUTH_CLIENT_ID` | Copy from your GitHub OAuth App settings (steps below). | Identifies dashboard sign-in. |
| `GITHUB_OAUTH_CLIENT_SECRET` | Generate in the same OAuth App's settings. | Exchanges the OAuth authorization code for a token. |
| `SESSION_SECRET` | Run `openssl rand -hex 32` and copy the output into `.env`. | Signs OAuth state; must contain at least 32 characters. |
| `MAX_CONCURRENT_BUILDS` | Keep `2` initially; must be a positive integer. | Per-worker queue concurrency. |
| `BUILD_TIMEOUT` | Keep `12m` initially; a positive Go duration, at most `24h`. | Build execution budget, with a separate completion grace. |

Register dashboard sign-in once:

1. Open GitHub **Settings → Developer settings → OAuth Apps → New OAuth App**.
2. Choose an application name and set **Homepage URL** to `http://localhost:8080`.
3. Set **Authorization callback URL** to exactly
   `http://localhost:8080/auth/github/callback`.
4. Register the application, copy its Client ID, and generate a client secret.
5. Fill `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET` in `.env`, then
   generate and fill `SESSION_SECRET` as described above.

**Expected:** the three placeholder values have been replaced. OAuth placeholders
can pass startup validation but cannot complete GitHub sign-in. These credentials
come from an **OAuth App**, separate from a repository-integration GitHub App.
See [GitHub's registration instructions](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app).

### Configure GitHub App repository linking (optional)

Keep the dashboard OAuth App above; repository linking uses a separate **GitHub App**.
See [App setup and installation testing](GITHUB_INSTALLATIONS.md) for registration,
permissions, private-key handling, and the complete verification flow.

| Server-only variable | Source | Purpose |
| --- | --- | --- |
| `GITHUB_APP_ID` | Numeric App ID in GitHub App settings. | JWT issuer for App authentication. |
| `GITHUB_APP_SLUG` | Name in `https://github.com/apps/<slug>`. | Installation page to open. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | Absolute path to the downloaded RSA PEM; store outside Git, `chmod 600`. | Signs short-lived App JWTs. |
| `GITHUB_APP_CLIENT_ID` | GitHub App Client ID, separate from OAuth App. | User authorization for verified linking. |
| `GITHUB_APP_CLIENT_SECRET` | Generate in GitHub App settings. | Exchanges the App user authorization code. |

Configure all five together and restart the server; omit all five to keep linking
visibly disabled while dashboard sign-in works. The worker needs none of these.
Register **Setup URL** `http://localhost:8080/installations/callback` and **Callback
URL** `http://localhost:8080/installations/authorize/callback`. Leave automatic OAuth
on installation unchecked; Previa explicitly starts it after the setup redirect.
Leave webhooks inactive until their HTTP receiver is implemented.

After migrations and server startup, visit `/dashboard` and click **Install on a repo**.
Expected: select repositories on GitHub, authorize the same account, then return to
linked installation/repository records. To independently prove token minting/API access:

```sh
TEST_GITHUB_INSTALLATION_ID=<your-installation-id> TEST_GITHUB_REPO_ID=<your-repo-id> make test-github-app
```

Replace both placeholders with numeric IDs. The target loads `.env`; no manual export
is needed. Expected: a passing real repository-metadata API check, with no token printed.
See the App guide for an ID lookup command and pending-approval/reconnect behavior.

### Loading configuration

Run `make run-server` or `make run-worker` directly: each target loads `.env` and
exports its values for that process. Already exported variables take precedence;
if you previously loaded an older `.env` into your shell, open a fresh terminal
or reload it before launching. Use `make run-server ENV_FILE=.env.local` (or the
worker equivalent) to select another file. A missing default `.env` uses only the
inherited environment; a missing explicitly selected file stops with an error.
Run targets do not fall back to placeholder credentials in `.env.example`.

The Go binaries still read environment variables directly. For **direct `go run`**
or manual migration commands, load your trusted local file into the terminal:

```sh
set -a
. ./.env
set +a
```

- `set -a`: automatically export variables assigned afterward to child processes.
- `. ./.env`: execute the local file's assignments in the current shell.
- `set +a`: turn automatic exporting off; the loaded variables remain exported.
- Repeat only in a new terminal or after editing `.env`; stopping/restarting a
  command in the same terminal does not clear the variables. Restart the server
  or worker after reloading changed values, since running processes retain their
  existing environment.
- Keep `.env` in shell assignment syntax and quote values containing spaces or
  shell special characters. Source only your trusted local configuration file.

| Command | Loads `.env` automatically? |
| --- | --- |
| `make run-server`, `make run-worker` | Yes; exported variables take precedence. |
| Direct `go run` | No; use the export block once per terminal. |
| `make dev` | Yes; Air loads `.env` on startup. Restart Air after configuration changes. |
| `make migrate-up`, `make migrate-down`, `make river-migrate-up`, `make river-status` | Yes; an exported `DATABASE_URL` takes precedence. |
| `make build`, `make lint`, `make generate` | No application configuration required. |
| `make test` | Database integration tests need an explicitly set `TEST_DATABASE_URL`. |

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
The table list shows application tables (including `environment_operations` after
Goose migration 4), `installations`/`repos` extended by migration 5, `goose_db_version`, and River's `river_*` tables.
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
`GOOSE_DBSTRING`. Neither command receives the connection string as an argument.
If you already loaded `.env` in this terminal, skip the export block.

## 5. Build and check the project

```sh
make generate
make build
make lint
make test
```

- `generate`: runs templ and, when query entries are configured in `sqlc.yaml`,
  sqlc. Expect generation to complete without errors.
- `assets`: `make assets` compiles official Tailwind from `web/styles/app.css`
  into `web/static/css/app.css`, installing the pinned CLI on first use. Expect
  Tailwind to report completion; no Node.js or CSS CDN is used.
- `build`: prepares CSS, generates Go code, and compiles all packages.
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

**Expected:** `TestPoolIntegration`, `TestMigrationConstraintsIntegration`, and
environment ordering/outbox integration tests pass rather than skip. The schema
tests create isolated schemas and remove them afterward. These tests use fake
external systems; they do not build Docker images or create routes. `-count=1`
forces a fresh run instead of using cached results.

Manual equivalents of the main Make targets:

| Make command | Underlying command |
| --- | --- |
| `make assets` | Install pinned standalone Tailwind if needed, then compile/minify CSS |
| `make build` | Compile CSS and generate Go code, then `go build ./...` |
| `make test` | Compile CSS and generate Go code, then `go test ./...` |
| `make lint` | Compile CSS and generate Go code, then `go vet ./...` and `go tool -modfile=tools/go.mod golangci-lint run` |
| `make generate` | `go tool -modfile=tools/go.mod templ generate`; sqlc generation is conditional as described above |
| `make run-server` | Compile CSS, generate templates, load local env with exported overrides, then `go run ./cmd/server` |
| `make run-worker` | Generate templates, load local env with exported overrides, then `go run ./cmd/worker` |
| `make dev` | `go tool -modfile=tools/go.mod air -c .air.toml` |

## 6. Run the application

In one terminal, start the server (configuration loads automatically):

```sh
make run-server
```

Open <http://localhost:8080/>. Successful startup logs `database health check passed`
followed by `HTTP server listening`. The server checks PostgreSQL before accepting
requests; connection failures stop startup. Set `HTTP_ADDR` to change the listen
address (default `127.0.0.1:8080`).

The root URL serves the public home page. Click **Get started with GitHub** to sign
in; successful authentication opens `/dashboard`, with the account navbar and
workspace sidebar. Upcoming navigation links lead to labeled placeholder sections.
Open **Developer tools** to use the queue ping diagnostic; run the worker to process
queued pings. The navbar remains visible while scrolling. The account menu leads
to session details and a logout confirmation modal. Cancel or Escape keeps the
current page and session; Confirm logout returns to the public home page.
Unauthenticated requests to `/dashboard` redirect to GitHub sign-in.

For GitHub OAuth App registration, callback configuration, and the full sign-in
test flow, follow [Test GitHub sign-in locally](AUTH_TESTING.md).

Start the worker in a second terminal:

```sh
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

Air loads `.env`, watches Go, templ, CSS, JavaScript, SQL, Makefile, and shell
sources, compiles CSS, regenerates templates, rebuilds, and restarts the server.
Edit pages under `web/pages`, the shells under `web/layouts`, shared components
under `web/components`, and CSS/theme source under `web/styles`. Polling supports WSL-mounted directories.
Generated template files, compiled CSS, and `bin/` are excluded from watching
to prevent rebuild loops. Worker startup does not need the Tailwind CLI.
Refresh the browser after a rebuild to see the change. Check the terminal for startup
or compilation errors. Use `Ctrl+C` to stop the process.
Air reloads the server; restart the worker separately after changes to worker code.

## 7. Daily workflow and shutdown

- Start or resume PostgreSQL with the Compose `up -d --wait postgres` command above.
- After pulling changes, run `make migrate-up`; run `make generate` when needed.
- Start the server with `make dev` for live reload or `make run-server` without
  reload. Start `make run-worker` in a second terminal; no manual env export needed.
- After editing `.env`, restart affected processes; each run command reloads the
  file. Previously exported shell values still take precedence: use a fresh
  terminal or reload manual exports if necessary. Restart Air after config edits.
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
- **Missing configuration or stale values:** check `.env` (or `ENV_FILE`) and
  restart the process after changes. Exported variables override file values;
  use a fresh terminal if old exports remain. Direct `go run` needs manual exports.
- **GitHub sign-in fails:** replace OAuth placeholders and check the registered
  callback matches `APP_BASE_URL` plus `/auth/github/callback`. Use `localhost`
  consistently in the browser for the documented local setup. Follow
  [the sign-in checks](AUTH_TESTING.md).

- **Tailwind download/build failure:** check curl and SHA-256 tool availability,
  GitHub release access, and `make assets`. A checksum mismatch stops installation;
  do not bypass it. Edit CSS sources rather than generated `web/static/css/app.css`.
- **Stylesheet missing after a manual build:** run `make assets` before compiling
  the server. The binary embeds assets at build time; restart it after rebuilding.

### Editor reports undefined Templ components

From the repository root, run:

```sh
make generate
go build ./...
```

**Expected:** generated component functions match the source templates, and the
Go build succeeds. Generated files are ignored and must exist locally for editor
type checking. If diagnostics persist while the build passes, reload the editor
workspace/restart its Go and Templ language servers; confirm the correct workspace
and Go toolchain. For automatic template generation without running the app:

```sh
make watch-templates
```

**Expected:** Air reports watching/building and regenerates Go when `.templ` files
change. It uses polling for WSL-mounted folders, needs no `.env`/database, and does
not start an HTTP server. Stop with Ctrl+C. Use `make dev` instead when you also
want server reload; one watcher is sufficient. `make templates` regenerates only
the presentation code once. `make generate` also generates sqlc database code.

## Optional browser regression checks

These checks require Node.js 20+ only for development; running the application,
Tailwind, and ordinary Go checks still requires no Node.js.

```sh
make browser-install
make test-browser
```

- Run the first command once to install locked Playwright v1.58.2 and Chromium.
- The second prepares assets/templates and starts a temporary test-only server.
  **Expected:** all nine browser scenarios pass and the server stops automatically.
- No Postgres, `.env`, or GitHub credentials are needed. GitHub and queue providers
  are fakes; session rotation, CSRF and logout use the actual middleware.
- On Linux, missing browser system libraries may require Playwright's OS dependency
  installation. See [browser checks](../web/tests/browser/README.md) for commands,
  screenshots and troubleshooting. These checks do not validate real GitHub consent
  or PostgreSQL integrations.
