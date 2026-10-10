Here's how I'd run the whole app on your machine and test it end to end by hand. Every step is checked against the repo's docs, `Makefile` and code. On WSL, run everything from the Linux side, inside the repo.

## Docs to keep open

| Doc | Use it for |
| --- | --- |
| [docs/02-development-setup.md](docs/02-development-setup.md) | The main guide: prerequisites, config, database, migrations, running, inspecting |
| [technical-docs/config-setup.md](technical-docs/config-setup.md) | Every setting in `.env`: what it's for and exactly how to get its value |
| [docs/04-github-sign-in-and-app.md](docs/04-github-sign-in-and-app.md) | Registering the GitHub OAuth App (Part A) and the GitHub App (Part B) for `localhost` |
| [technical-docs/user-guide.md](technical-docs/user-guide.md) | §0 local previews without a domain, every screen and flow, and a **manual test checklist (A–K)** |
| [docs/05-webhooks-and-pr-discovery.md](docs/05-webhooks-and-pr-discovery.md) | Getting GitHub webhooks to reach your machine (optional) |
| [docs/11-backend-previews.md](docs/11-backend-previews.md) | Previews with PostgreSQL/Redis, setup commands and workers |
| [test-docs/](test-docs/README.md) | The full list of test cases, if you want to tick things off |

## Step 1: Register the two GitHub apps (one-time, about 15 minutes)
Follow `docs/04`:
- **OAuth App** (sign-in): callback `http://localhost:8080/auth/github/callback`.
- **GitHub App** (repository access):
  - setup URL `http://localhost:8080/installations/callback`;
  - callback `http://localhost:8080/installations/authorize/callback`;
  - permissions: Contents read, Pull requests write, Checks write.

Save the App's private key in your Linux home (not under `/mnt/c`) and `chmod 600` it. If you won't set up webhooks yet, untick **Active** under Webhooks.

## Step 2: Fill in `.env`
Your `.env` already exists. Every line has a `[Required]`, `[Required for …]` or `[Optional]` label, and `config-setup.md` explains each one.
- **Sign-in:** `APP_BASE_URL=http://localhost:8080`, the OAuth client ID/secret, and `SESSION_SECRET` (`openssl rand -hex 32`).
- **Repositories:** the five `GITHUB_APP_*` values.
- **Repository secrets:** `SECRETS_ENCRYPTION_KEYS=1:<openssl rand -base64 32>` and `SECRETS_ACTIVE_KEY_VERSION=1`.
- **Local previews, no real domain needed** (user guide §0):
  - `PREVIEW_BASE_DOMAIN=preview.localtest.me`;
  - `ROUTER_DYNAMIC_DIR=/home/<you>/.local/share/previa/router` (a full path, no `~`);
  - the `ACME_*` placeholders from §0, with `ACME_CA_SERVER=https://127.0.0.1:1/directory`;
  - `MAX_CONCURRENT_BUILDS=1` on a laptop.

Always open the dashboard as `localhost`, not `127.0.0.1`, because sign-in cookies are tied to the hostname.

## Step 3: Database and migrations
```sh
docker compose -f deploy/docker-compose.yml up -d --wait postgres
make migrate-up        # your dev DB was last at 19; this brings it to Goose 20
```

## Step 4: Build, then start the router
```sh
make build             # optionally: make test
make egress-image      # the outbound proxy every build and preview uses
make router-up         # local Traefik on ports 80/443
```
**Expected:** `Routes: … holds route table version …`. Traefik uses its built-in self-signed certificate, so the browser will warn.

## Step 5: Run the server and worker in two terminals
```sh
make dev               # terminal 1: server with live reload, http://localhost:8080
make run-worker        # terminal 2
```
**Expected:** the worker logs `River worker started execution=true …`. If it says `execution=false`, the warning just above names the missing setting.

To check both are healthy: `curl -s localhost:9090/readyz` and `curl -s localhost:9091/readyz`. In the dashboard, **Overview** should show every pipeline stage as **Connected**.

## Step 6 (optional): Webhooks
These let PRs appear by themselves and make **Automatic** builds work. Run a tunnel to port 8080 (for example `cloudflared tunnel --url http://localhost:8080`), put `<tunnel URL>/webhooks/github` and `GITHUB_WEBHOOK_SECRET` into the App, restart the server, then run `make test-webhook` (expect `HTTP 204`). Without webhooks, use **Sync Pull Requests** and **Build latest commit**.

## Step 7: A small test repository
Create a GitHub repository containing these three files:
```dockerfile
# Dockerfile
FROM python:3.13-alpine
WORKDIR /app
COPY index.html .
CMD ["python", "-m", "http.server", "3000", "--bind", "0.0.0.0"]
```
```yaml
# .previewrc.yaml
port: 3000
```
plus any `index.html`. Commit to its default branch, then open a PR that changes `index.html`.

## Step 8: Test the full journey
Follow the user guide's **manual test checklist (A–K)**:
1. Sign in → **Repositories → Install on a repo** → pick the test repo → **Sync Pull Requests**.
2. Open the PR → **Build latest commit** → watch **Build logs** stream.
3. **Preview is live** → **Open preview** at `https://pr-…preview.localtest.me` (accept the certificate warning) → the PR gets a comment and a check run.
4. Push a failing commit (`RUN exit 1`) → rebuild → the old preview keeps serving.
5. Add a secret on the **Secrets** tab and rebuild. For a backend preview, add `services: {postgres: 17}` to `.previewrc.yaml` (`docs/11`).
6. **Tear down preview**, then close the PR → the preview goes and the address shows "Preview not found or expired".

For what's happening behind the scenes, see `docs/02` §7 (jobs and operations) and `docker ps --filter label=io.previa.managed=true`.

## Step 9: Stop everything
Press `Ctrl+C` in both terminals, then run `make router-down` and `docker compose -f deploy/docker-compose.yml stop`. The database volume is kept.