# Test GitHub sign-in locally

## 1. Register the OAuth App

In GitHub, open **Settings → Developer settings → OAuth Apps → New OAuth App**.

- Application name: a name such as `Previa local`.
- Homepage URL: `http://localhost:8080`.
- Authorization callback URL: **`http://localhost:8080/auth/github/callback`**.
- Generate a client secret and put it, with the client ID, in your ignored `.env`.

```sh
APP_BASE_URL=http://localhost:8080
GITHUB_OAUTH_CLIENT_ID=your-client-id
GITHUB_OAUTH_CLIENT_SECRET=your-client-secret
```

Generate a random session secret with `openssl rand -hex 32` and set
`SESSION_SECRET` to the result in `.env`. Keep the remaining settings from
`.env.example`. Use `localhost` consistently in the browser; `127.0.0.1` is a
different cookie host. If changing the port, update `HTTP_ADDR`, `APP_BASE_URL`,
and the registered callback URL together. HTTPS is required outside localhost;
HTTPS origins enable the cookies' Secure flag.

## 2. Start the database and server

From the repository root:

```sh
docker compose -f deploy/docker-compose.yml up -d --wait postgres
make migrate-up
make run-server
```

The run target loads `.env`; exported variables take precedence.
Expect the database health-check and HTTP listening messages. Use `make dev` for
reloads; Air loads `.env` itself. Restart it after changing credentials in `.env`.

Migration `0002_auth.sql` adds the avatar field and SCS's required `sessions`
table/index. [SCS pgxstore requires this table](https://github.com/alexedwards/scs/tree/master/pgxstore)
and does not run migrations; it owns session CRUD and removes
expired rows every five minutes. User upsert/lookup and session inspection are
sqlc-generated queries in `internal/auth/dbgen`.

## 3. Exercise the complete flow

1. Open `http://localhost:8080/` in a private browser window. Expect
   **Get started with GitHub**.
2. Open `/protected` while logged out. Expect a redirect into GitHub sign-in.
   Return home to start the flow manually if desired.
3. Click **Get started with GitHub**. The GitHub authorization request includes the client
   ID, exact callback URI, and random state. No additional OAuth scopes are requested:
   the public profile provides the ID, username, and avatar.
4. Approve the GitHub consent screen. GitHub sends a temporary code and the matching
   state to `/auth/github/callback`. The server verifies state, exchanges the code,
   fetches `/user`, upserts the local user, rotates the session, and redirects to `/dashboard`.
5. Expect the dashboard navbar, workspace sidebar, and your GitHub username.
   The account menu opens session details and the logout confirmation. `/protected`
   also displays your username; `/dashboard` is protected by the same middleware.
6. In browser developer tools, inspect cookies. `previa_session` is opaque,
   HttpOnly, SameSite=Lax, and has a different value after login. The short-lived
   `previa_oauth_state` cookie is cleared by the callback. Secure is enabled for
   HTTPS; local HTTP development uses non-Secure cookies.
7. Restart the server and refresh `/protected`. The DB-backed session remains valid.
8. Open the account menu and click **Log out**. Expect a centered modal over the
   same page. Cancel or Escape closes it and preserves the session; focus returns
   to the account menu. Reopen it and click **Confirm logout**. Confirmation submits a CSRF-protected
   POST. Expect the home page's Get started button to return. The authenticated session row
   is deleted and its cookie cleared; `/protected` redirects to login again.

Sessions have a 30-minute idle timeout and a 24-hour absolute lifetime. Activity
renews the idle expiry without extending the absolute deadline. OAuth tokens are
discarded after the identity fetch and are not stored in the session or database.

## 4. Check failure paths

- **State mismatch:** open
  `http://localhost:8080/auth/github/callback?code=unused&state=wrong`.
  Expect HTTP 400 and a retry page. No token exchange or authenticated session is
  created. Start a new login afterward.
- **Denied consent:** start a fresh authorization and cancel it on GitHub. Expect
  a clear decline/interruption page. Previously authorized apps may bypass consent;
  revoke the local app in GitHub's application settings to show it again.
- **CSRF:** while logged in, run this in the browser console:

  ```js
  fetch('/auth/logout', {method: 'POST'}).then(r => console.log(r.status))
  ```

  Expect `403`; `/protected` should still work. Logout succeeds through the
  confirmation form containing `csrf_token`. HTMX requests can send the same
  session-bound value in `X-CSRF-Token`.
- **Database/GitHub outage:** failure pages offer a retry and do not issue a new
  authenticated session. Automated tests cover unavailable exchanges, unavailable
  identity fetches, failed user persistence, and failed session rotation/commit.

## 5. Run automated checks

```sh
make build lint test
TEST_DATABASE_URL="$DATABASE_URL" go test -count=1 -v ./...
```

The auth tests simulate GitHub exchange/profile responses and verify cookie/state
checks, failed/denied callbacks, replay rejection, session rotation/deletion, CSRF,
idle/absolute expiry, and HTTPS cookie flags. The PostgreSQL test verifies persisted
sessions, login after session-manager restart, and username/avatar updates without
changing the local ID for the same numeric GitHub ID. Integration tests skip when
`TEST_DATABASE_URL` is absent. They clean up their test sessions and roll back users.
The real GitHub consent screen is checked manually using steps 1–3.

## DESIGN.md Section 2 checklist

| Step | Implementation |
| --- | --- |
| 1 | Login creates random state, signs a 10-minute HttpOnly cookie, and redirects to GitHub with the configured callback URI. State is also tied to the browser's server-side session. |
| 2 | Callback accepts GitHub's temporary code and returned state. |
| 3 | Callback validates signature, expiry, cookie state, and pending session state; it consumes the pending state and clears the cookie. |
| 4 | `golang.org/x/oauth2` exchanges the code with GitHub server to server. |
| 5 | `go-github` fetches the authenticated `/user` identity: numeric ID, username, avatar. |
| 6 | sqlc upserts `users` on the unique numeric `github_id`, updating mutable profile fields. |
| 7 | SCS rotates the session token before storing the user ID and redirects to `/dashboard`. |
| 8 | The temporary OAuth token/client are discarded after fetching identity. |
