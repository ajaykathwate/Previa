# GitHub App installation linking

This implements DESIGN Section 4's installation redirect, verified user linking,
installation authentication, dashboard records, and directly callable lifecycle
handling. Dashboard login still uses the separate OAuth App. No webhook HTTP
receiver, preview configuration parser, or job enqueueing is added.

## Register and configure the App

1. In GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App**.
2. Set the homepage to `http://localhost:8080` for local development.
3. Set **Setup URL** to `http://localhost:8080/installations/callback`.
4. Set **Callback URL** to `http://localhost:8080/installations/authorize/callback`.
   This is App user authorization, separate from the OAuth App's
   `http://localhost:8080/auth/github/callback` dashboard-login callback.
5. Leave **Request user authorization (OAuth) during installation** unchecked.
   Previa explicitly starts authorization after verifying the setup redirect.
   Enable **Redirect on update** so selection updates can return to the setup URL.
6. Configure repository permissions: **Contents: read**, **Pull requests: write**
   (covers PR comments), **Checks: write**; GitHub supplies **Metadata: read**.
   The App does not request personal access tokens or broad OAuth scopes.
7. Leave **Webhook → Active** disabled for this local phase: no receiver exists.
   When the receiver is built, enable it and configure `pull_request`, installation
   and repository-selection lifecycle delivery according to DESIGN Sections 4/6.
8. Choose whether the App can be installed only on your account or others, according
   to where you intend to test. Register the App.
9. Copy **App ID**, **Client ID**, and the URL slug from
   `https://github.com/apps/<slug>`. Generate a **client secret**.
10. Generate/download a **private key**. Move it outside the repository, preferably
    into your WSL/Linux home directory rather than a Windows-mounted directory.
    Restrict permissions:

    ```sh
    chmod 600 /absolute/path/to/github-app.private-key.pem
    ```

11. Add all five settings to `.env` (the example documents them):

    ```dotenv
    GITHUB_APP_ID=123456
    GITHUB_APP_SLUG=your-app-slug
    GITHUB_APP_PRIVATE_KEY_PATH=/absolute/path/to/github-app.private-key.pem
    GITHUB_APP_CLIENT_ID=your-github-app-client-id
    GITHUB_APP_CLIENT_SECRET=your-github-app-client-secret
    ```

The numeric App ID identifies the JWT issuer; the RSA PEM signs the JWT. The slug
selects the installation page. The App client ID/secret exchange a user authorization
code to verify the installation link. These are not the dashboard OAuth App credentials.

All App settings may be omitted: dashboard sign-in remains available and connection
is visibly disabled. A partial App configuration fails server validation. A configured
server rejects unreadable/invalid keys and files accessible to group/other users.
The worker does not read any App settings. Keys, client secrets, and tokens are never
sent to templates, persisted as installation records, or logged.

## Test the browser flow against real GitHub

```sh
make migrate-up
make run-server
```

1. Visit **http://localhost:8080**, sign in, and arrive at **/dashboard**.
2. Open **Repositories** (`/repositories`), then click **Install on a repo**
   in the page header (or **Manage connections** after linking). This submits the CSRF-protected
   `POST /installations/start`; GitHub opens
   `https://github.com/apps/<slug>/installations/new?state=<random-state>`.
3. Select a test repository and approve installation. If organization-owner approval
   is required, Previa reports a pending request; it does not link an unapproved ID.
4. GitHub returns to the setup URL. Previa validates the session-bound state, then
   redirects to GitHub App user authorization. Authorize using the **same GitHub
   account** as dashboard sign-in. If consent already exists, GitHub may return
   immediately without another consent screen.
5. Previa exchanges the code, verifies the numeric user ID, checks that the user can
   access this installation, and paginates its user-accessible repository list.
   App-authenticated installation metadata confirms this is an installation of our
   App and is not suspended. An installation token then authenticates a repository
   listing and, when a repository exists, its metadata lookup.
6. Expected: return to `/repositories` with a connection confirmation and the linked
   repository names, account, installation ID and access state. Lists use 25 rows
   per page; search and access filters run against the database. The server logs a successful link without
   exposing credentials. No build/job is started. Refreshing preserves DB records.
7. For selection updates, start again from **Manage connections** and manage the
   existing installation. A callback opened manually without the session-bound state
   is rejected; use the dashboard flow. External GitHub changes do not automatically
   synchronize this local database until lifecycle delivery is wired in a later phase.

Installation ownership in Previa is a single verified local user. Reconnecting the
same installation refreshes its list. Another local user cannot overwrite that owner.
An inactive old installation's repository history can move to a new installation
only when both installations belong to the same local user. Unlinked events cannot
transfer repository history; the verified redirect retrieves its list later.

## Independent real token/API proof

After linking a real repository, find its numeric IDs in the local DB (no secrets):

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U previa -d previa -c \
 'SELECT i.github_installation_id, r.github_repo_id, r.full_name FROM installations i JOIN repos r ON r.installation_id = i.id WHERE i.status = '\''active'\'' AND r.accessible;'
```

Use the returned IDs, replacing the example numbers:

```sh
TEST_GITHUB_INSTALLATION_ID=12345678 TEST_GITHUB_REPO_ID=987654321 make test-github-app
```

The Make target loads `.env` automatically. Expected: `TestRealInstallationRepositoryAccess`
passes and reports that an installation token authenticated a real repository metadata
request. It never prints the token. This test is opt-in; ordinary tests skip it and
cannot be cited as real GitHub verification. Real verification requires working App
credentials, installation approval, and GitHub network access.

## Implementation boundaries

- Feature: `internal/installations` contains handler, service, authorization,
  repository, SQL and tests. GitHub JWT/token transport is in
  `internal/platform/githubapp`; `ghinstallation/v2` is pinned to **v2.18.0**.
- `internal/app` constructs dependencies and adapts installation counts to simple
  overview display models. Repository handlers pass view data to `web/pages/repositories` and
  presentation imports no backend packages. A consumer-owned summary reader keeps dashboard independent
  of the installations implementation. Approved styles/modal behavior stay intact.
- `0005_installation_linking.sql` extends the existing tables: nullable owner,
  `active`/`suspended`/`inactive` status, account login, installation revision,
  stable GitHub repository IDs, and access flags. Existing rows/history remain.
  Rollback deliberately fails if unlinked rows prevent restoring the old NOT NULL
  owner constraint; it never deletes history to make rollback succeed.
- Linking reads the installation revision **before external calls**, then obtains
  `SELECT ... FOR UPDATE` inside the final transaction. Revision equality, current
  active status, existing ownership, repository synchronization and revision bump
  execute under that lock. Every committed lifecycle call bumps revision. A newer
  event makes the callback return a conflict instead of restoring stale access.
  External GitHub calls never hold DB locks; overall linking is bounded to 25s,
  and individual HTTP/token requests to 10s.
- One mutex-protected token transport is cached per installation; ghinstallation
  refreshes one minute before expiry. Its mutable App transport is copied per
  installation to avoid cross-installation refresh races. Lifecycle calls evict the
  cache; in-flight GitHub requests remain subject to GitHub's own permission checks.
- `Service.HandleEvent(ctx, parsedPayload)` accepts `*github.InstallationEvent` and
  `*github.InstallationRepositoriesEvent`. Fixtures in `testdata/` use
  `github.ParseWebHook` and call it directly. It has no HTTP route or signature
  verification. A future receiver must verify the signature/provenance before calling.
- Creation stores an unlinked installation when needed; callbacks assign ownership.
  Repository additions/removals update access without deleting PR/environment history.
  Suspend hides access; unsuspend restores retained selection. Delete is terminal for
  an installation ID; a delayed created event cannot revive it. Permission acceptance
  invalidates tokens. Unknown actions/invalid repository IDs are rejected.
- Suspension/deletion commits revoked access, evicts credentials, and calls a small
  idempotent `CleanupRequester` boundary. No cleanup adapter is wired yet: the handler
  returns `ErrCleanupUnavailable` after recording access revocation. It does not claim
  that real environments were removed. Before live lifecycle delivery, wire durable
  teardown scheduling/recovery; do not acknowledge cleanup as complete based only on
  these status updates. Repository removal cleanup is also future runtime integration.
- Fixtures verify state/CSRF/replay/session identity, user installation access,
  pagination, signed JWT/token caching/refresh/concurrency, lifecycle and cleanup
  boundaries, DB isolation/revision fences, rename/reinstall history, and rendered data.
  Installation/repository event ordering beyond terminal deletion requires the later
  receiver/reconciliation strategy; arrival order is not a provider event version.

GitHub documents why a setup redirect's `installation_id` alone must not be trusted:
[setup URL verification](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url).
See also [App user authorization](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
and [installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation).
