# 04 · GitHub sign-in and App installation

Previa uses **two separate GitHub registrations**, on purpose:

| Registration | Used for | Credentials | Process |
| --- | --- | --- | --- |
| **OAuth App** | Dashboard sign-in (who you are) | `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET` | server |
| **GitHub App** | Repository access, webhooks, PR APIs (what Previa may touch) | App ID, private key, slug, App client ID/secret, webhook secret | server; worker needs only ID + key |

Signing in grants no repository access; installing the App does. Keeping them
apart means a login token is never used for repository work, and repository
permissions are chosen per installation by the account owner.

## Part A · Dashboard sign-in (OAuth App)

### Register

1. GitHub **Settings → Developer settings → OAuth Apps → New OAuth App**.
2. Homepage URL `http://localhost:8080`; **Authorization callback URL**
   `http://localhost:8080/auth/github/callback` (exactly).
3. Copy the Client ID, generate a client secret, and set in `.env`:

```dotenv
APP_BASE_URL=http://localhost:8080
GITHUB_OAUTH_CLIENT_ID=your-client-id
GITHUB_OAUTH_CLIENT_SECRET=your-client-secret
SESSION_SECRET=<output of: openssl rand -hex 32>
```

Use `localhost` consistently in the browser; `127.0.0.1` is a different cookie
host. If you change the port, update `HTTP_ADDR`, `APP_BASE_URL` and the callback
together. Outside localhost HTTPS is required; HTTPS origins turn on the cookies'
Secure flag.

### How the flow works

1. **Get started with GitHub** creates random state, stores it in a signed
   10-minute HttpOnly cookie and in the server-side session, and redirects to
   GitHub with the exact callback URL. No extra OAuth scopes are requested.
2. The callback verifies the signature, expiry, cookie state and pending session
   state, consumes the state and clears the cookie.
3. `golang.org/x/oauth2` exchanges the code server to server; `go-github` fetches
   `/user` (numeric ID, login, avatar).
4. sqlc upserts `users` on the immutable numeric GitHub ID, updating mutable
   profile fields.
5. SCS rotates the session token, stores the user ID and redirects to `/dashboard`.
6. The OAuth token is discarded; it is never stored in the session or database.

Sessions live in PostgreSQL (`sessions` table, SCS `pgxstore`, which prunes
expired rows every five minutes). They have a **30-minute idle timeout** and a
**24-hour absolute lifetime**. The `previa_session` cookie is opaque, HttpOnly
and SameSite=Lax. Logout is a CSRF-protected POST behind a confirmation modal; it
deletes the session row and leaves your GitHub session untouched. A legacy GET
`/auth/logout` only opens that modal.

### Check it

1. Private window → `http://localhost:8080/` → **Get started with GitHub**.
2. Approve on GitHub; expect `/dashboard` with your username.
3. Restart the server and refresh: the database-backed session survives.
4. **Account menu → Log out → Confirm logout**; `/protected` now redirects to sign-in.

Failure paths:

- **State mismatch:** `http://localhost:8080/auth/github/callback?code=unused&state=wrong`
  returns 400 and a retry page; no session is created.
- **Denied consent:** cancel on GitHub; expect a clear interruption page.
- **CSRF:** while signed in, `fetch('/auth/logout', {method: 'POST'})` in the
  console returns 403 and the session survives. HTMX requests send the token as
  `X-CSRF-Token`.
- **Outages:** database/GitHub failures show a retry page and never issue a session.

Automated tests simulate GitHub and cover state/cookie checks, replay rejection,
session rotation and deletion, CSRF, idle/absolute expiry and HTTPS cookie flags;
the PostgreSQL test covers persisted sessions across restarts.

## Part B · Repository access (GitHub App)

### Register

1. GitHub **Settings → Developer settings → GitHub Apps → New GitHub App**.
2. Homepage `http://localhost:8080`.
3. **Setup URL** `http://localhost:8080/installations/callback`; enable
   **Redirect on update**.
4. **Callback URL** `http://localhost:8080/installations/authorize/callback` (App
   user authorization, distinct from the OAuth App callback).
5. Leave **Request user authorization (OAuth) during installation** unchecked;
   Previa starts it explicitly after verifying the setup redirect.
6. Repository permissions: **Contents: read** (source archives), **Pull requests:
   write** (the living PR comment), **Checks: write** (exact-commit check runs);
   GitHub adds **Metadata: read**. See [14](14-pr-feedback.md#required-github-app-permissions)
   for what each is used for and what happens if one is withdrawn.
7. Webhooks: enable, set a secret and a public HTTPS URL ending in
   `/webhooks/github`, subscribe to **Pull request** ([05](05-webhooks-and-pr-discovery.md#enable-github-deliveries-locally)).
8. Register, then copy the **App ID**, **Client ID** and slug (`github.com/apps/<slug>`),
   generate a **client secret** and a **private key**.
9. Store the key outside the repository (on WSL, in your Linux home rather than a
   Windows mount) and `chmod 600` it. Add to `.env`:

```dotenv
GITHUB_APP_ID=123456
GITHUB_APP_SLUG=your-app-slug
GITHUB_APP_PRIVATE_KEY_PATH=/absolute/path/to/github-app.private-key.pem
GITHUB_APP_CLIENT_ID=your-github-app-client-id
GITHUB_APP_CLIENT_SECRET=your-github-app-client-secret
```

The server requires all five or none (none = linking visibly disabled). It rejects
unreadable keys and key files readable by group/other. Workers need only App ID
and key path. Keys, secrets and tokens never reach templates, records or logs.

### Linking flow

1. **Repositories → Install on GitHub** submits a CSRF-protected
   `POST /installations/start`, which opens
   `github.com/apps/<slug>/installations/new?state=<random>`.
2. Choose repositories. If an organization owner must approve, Previa reports a
   pending request and links nothing.
3. GitHub returns to the setup URL. Previa validates the session-bound state, then
   redirects to App user authorization. Authorize with the **same account** you
   signed in with.
4. Previa exchanges the code, verifies the numeric user ID and that this user can
   access the installation, and paginates the user-accessible repositories. App
   authentication confirms the installation belongs to this App and is not
   suspended; an installation token lists repositories.
5. You return to **Repositories** with a confirmation notice. No build starts;
   newly accessible repositories queue a first open-only PR discovery.

The setup redirect's `installation_id` is never trusted on its own
([GitHub guidance](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url)).

### Ownership and lifecycle rules

- One verified local user owns an installation. Reconnecting refreshes its
  repository list; another user cannot take it over. History moves from an inactive
  installation to a new one only for the same owner.
- Linking reads the installation **revision before external calls**, then locks the
  row (`SELECT … FOR UPDATE`) in a short final transaction that checks revision,
  status and ownership before syncing repositories and bumping the revision. A
  concurrent lifecycle event therefore causes a conflict, never restored stale
  access. Linking is bounded to 25 seconds, each GitHub request to 10.
- Signed lifecycle events update access: repository removal clears access (rows and
  history kept), suspend hides access, unsuspend restores it, deletion is terminal
  for that installation ID. Permission acceptance and lifecycle changes evict the
  cached token.
- Suspension, deletion and repository removal record durable `cleanup_pending` and,
  in the same transaction, a teardown request for each affected PR with a live
  preview or unfinished build. Teardown and cleanup never need the App's token
  ([13](13-teardown-and-cleanup.md)).
- Installation tokens are cached per installation (via `ghinstallation` v2.18.0,
  refreshed a minute before expiry) behind a mutex-protected transport.

### Prove real API access (opt-in)

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa -c \
 "SELECT i.github_installation_id, r.github_repo_id, r.full_name FROM installations i JOIN repos r ON r.installation_id = i.id WHERE i.status = 'active' AND r.accessible;"
TEST_GITHUB_INSTALLATION_ID=12345678 TEST_GITHUB_REPO_ID=987654321 make test-github-app
```

**Expected:** `TestRealInstallationRepositoryAccess` passes using a real
installation token, which is never printed. Ordinary tests skip it and use fake
GitHub APIs.

### Where the code lives

`internal/installations` (handlers, service, authorization, repository, SQL,
lifecycle), `internal/platform/githubapp` (App JWT and token transport),
migration `0005_installation_linking.sql`. Its rollback deliberately fails rather
than delete history if unlinked rows prevent restoring the old NOT NULL owner.
