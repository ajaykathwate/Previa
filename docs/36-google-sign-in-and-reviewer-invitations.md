# 36 · Google sign-in and reviewer invitations

F7 is the final owner-requested MVP addition. Authentication identifies a Previa
account; workspace memberships and explicit repository grants determine its role.
Google login never assigns a global Reviewer role. GitHub login never assigns
Developer/Admin access to an existing workspace.

## User flow

1. The public page offers GitHub and, when configured, Google. With Google
   configured, the navbar's **Get started** opens the sign-in choices
   (`/auth/login`); without it, it goes straight to GitHub. Protected-page
   redirects use `/auth/login` to offer both and preserve the original review or
   preview destination. Existing GitHub-only configuration retains its old flow.
   After a logout, GitHub shows its account picker so another account can be used
   ([sign-in](04-github-sign-in-and-app.md)); Google always shows its chooser.
2. A Google-only signup creates an account without GitHub or an administrative
   workspace. `/workspaces` explains how to join a team. Existing invited members
   can use their granted previews/reviews, findings and comments without GitHub.
3. A manager enters a GitHub username for any existing role, or a verified email
   address for **Reviewer**. Select repositories and create an invitation. Copy
   its link to the person: **Previa sends no invitation email**. The recipient
   also sees pending invitations inside Previa when their verified email matches.
4. The recipient signs in, reviews scope, then accepts or declines. Forwarding
   the link to another mailbox does not grant access. Expiry, revocation,
   creator authority, workspace state and repository grants still apply.
5. Your profile has explicit **Connect Google** / **Connect GitHub** actions.
   They preserve the account, memberships and authorship. A connected identity
   cannot be reassigned from a different account. Matching emails never merge
   accounts. Existing users should sign in with their original method and connect
   the other one before using it as a separate signup. There is no account-merge
   workflow in this MVP; identity collisions are refused with an explanation.
6. Developer/Admin/Owner capabilities require a connected GitHub account plus
   the appropriate workspace assignment. Linking GitHub does not promote a
   Reviewer. Creating a new workspace after connecting GitHub remains subject
   to F6 operator approval. Source-sensitive actions still need current GitHub
   repository permissions and the App installation.

## Authentication and identity

`internal/auth` remains the owner of identity, sessions, profile and linking.
GitHub's immutable numeric ID becomes nullable; Google's immutable `sub` is
unique. Either provider can authenticate the same linked local user ID. Email
and display name are not identity keys. Deleted provider IDs remain tombstones.

Google uses the existing confidential authorization-code flow with state bound
both to the session and signed expiring cookie, plus PKCE S256 and a nonce. The
maintained `coreos/go-oidc/v3` verifier validates signed RS256 ID tokens, issuer,
audience and expiry; the callback checks nonce, subject and verified email.
It does not use tokeninfo for authentication or persist access/refresh/ID tokens.
Only `openid email profile` scopes are requested. Google pictures are not fetched;
users can upload a profile picture through the existing feature.

The OAuth provider and optional linking target are bound to the one-use session
state. Linking begins only through an authenticated CSRF-protected POST; its
callback must still belong to that same active account. Ordinary login never
silently becomes a linking action. Success rotates the session and CSRF token;
provider mismatch, replay, consent refusal and failed proofs do not authenticate.

A separate verified Google email is kept for matching invitations, so refreshing
GitHub with another primary email cannot silently change Google invitation
identity. Email normalization trims/lowercases, without Gmail dot/plus rewriting.
A GitHub verified primary email may accept a matching email invitation too.
No provider role is trusted or mapped into Previa permissions.

## Authorization, erasure and restore

Email invitations are Reviewer-only and retain token digests, seven-day expiry,
manual link sharing, revision checks and atomic membership/grant creation.
Database triggers require GitHub for workspace ownership and active technical
roles, supplementing service checks. Reviewers cannot fetch source, inspect logs,
change secrets, build, sync or connect repositories. Re-invitation of a removed
Google-only member uses their verified email, not their display name.

Private previews offer the login chooser when Google is configured. Active local
membership/grants authorize a Google reviewer without contacting GitHub. An
uninvited Google-only visitor cannot use the GitHub-reader outsider fallback,
even if their display name matches a GitHub login. Existing session-bound preview
cookies and membership-revocation checks remain in force.

F4 account deletion clears Google email/profile while retaining immutable subject
as a resurrection fence. Journal **v2** includes the Google subject for account
revocations; membership replay uses the stable public user reference. Readers
accept prior v1 journals. A v2-aware application must be used for restore; older
readers refuse v2 rather than silently losing Google revocations. No email,
token or review content is added to the independent encrypted journal.

## Configuration and migration

Goose **0046**, application schema **46**. Existing users, sessions, IDs, roles,
GitHub callbacks and F6 approvals remain valid. Rollback is tested on a scratch
schema and deliberately refuses once Google identities or email invitations
exist; forward-fix then rather than discard account/access data.

Optional server env, configured together:

```dotenv
GOOGLE_OAUTH_CLIENT_ID=
GOOGLE_OAUTH_CLIENT_SECRET=
```

Register a **Web application** OAuth client in Google Cloud, configure its consent
screen/audience and register the exact redirect URI:
`APP_BASE_URL/auth/google/callback`. Use separate clients/URIs for development and
production as appropriate. No secret belongs in Git or this documentation.
Both blank disables Google routes/buttons while GitHub keeps working. A partial
pair is a configuration error. Restart the server after setting them. Worker
credentials do not change. See [development setup](02-development-setup.md#google-sign-in-and-email-reviewer-invitations-f7).

## Verification and references

Automated evidence is recorded in the dated `test-docs` feature report, including
signed TLS ID-token failures, provider-bound state and linking, PostgreSQL identity/
role/invitation/restore checks, real browser stakeholder review with scripts on/off,
existing regressions and migration up/down/up. Run `make test-google-browser` with
`TEST_DATABASE_URL` pointing to a disposable PostgreSQL database. The fake provider
is test-only; live consent with your registered Google client is a separate setup
acceptance check. User-guide manual rows M1–M9 cover it.

Primary references: [Google OpenID Connect](https://developers.google.com/identity/openid-connect/openid-connect),
[go-oidc](https://github.com/coreos/go-oidc).
