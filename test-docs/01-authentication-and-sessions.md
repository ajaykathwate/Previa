# 01 · Authentication and sessions

**Module:** AUTH · **Code:** `internal/auth`, `internal/platform/httpx` · **Feature
docs:** [04 · GitHub sign-in and App](../docs/04-github-sign-in-and-app.md)

Signing in with GitHub, the server-side session, signing out, and the access and
CSRF rules every page and action depends on.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Sign-in | TC-AUTH-SIGN | 6 |
| Sessions | TC-AUTH-SES | 4 |
| Sign-out | TC-AUTH-OUT | 2 |
| Access control and CSRF | TC-AUTH-ACC | 4 |

## 1. Sign-in (TC-AUTH-SIGN)

- [x] **TC-AUTH-SIGN-001 · Signing in creates a fresh, rotated session**  
  Type: Positive · Priority: P1 · Level: Integration  
  Steps: Open the dashboard signed out; start sign-in; return from GitHub with a valid code and state.  
  Expected: Signed out, the dashboard redirects to sign-in. After the callback the session ID is rotated, the old one no longer exists, cookies are HttpOnly, and the dashboard shows the username.  
  Evidence: `internal/auth` › `TestLoginRotationProtectedRouteAndLogout`

- [x] **TC-AUTH-SIGN-002 · A failed or tampered callback never authenticates**  
  Type: Negative · Priority: P1 · Level: Integration  
  Steps: Call the callback with a mismatched state, a declined authorization, no code, and with GitHub failing.  
  Expected: Each shows a clear error page; no state cookie is left; no authenticated session exists afterwards.  
  Evidence: `internal/auth` › `TestCallbackFailuresDoNotAuthenticate`

- [x] **TC-AUTH-SIGN-003 · An OAuth state cannot be replayed**  
  Type: Security · Priority: P1 · Level: Integration  
  Steps: Complete a sign-in, then send the same callback (code and state) again over HTTPS.  
  Expected: The replay is refused and not exchanged with GitHub again; HTTPS cookies carry Secure and the `__Host-` prefix rules.  
  Evidence: `internal/auth` › `TestHTTPSCookieFlagsAndCallbackReplay`

- [x] **TC-AUTH-SIGN-004 · Sign-in asks GitHub for the minimum and trusts no partial identity**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Run the code exchange and identity lookup against a fake GitHub that succeeds, fails the exchange, fails the identity read, or returns an invalid identity.  
  Expected: No extra OAuth scopes are requested; the exchanged token is used for the identity read; every failure returns no identity at all.  
  Evidence: `internal/auth` › `TestGitHubExchangeAndIdentity`

- [x] **TC-AUTH-SIGN-005 · A session store outage does not sign anyone in**  
  Type: Resilience · Priority: P1 · Level: Integration  
  Steps: Make the session store fail during the callback.  
  Expected: The response is an error, not a success page, and the client is not authenticated.  
  Evidence: `internal/auth` › `TestSessionStorageFailureDoesNotAuthenticate`

- [x] **TC-AUTH-SIGN-006 · Sign-in is rate limited per client**  
  Type: Security · Priority: P2 · Level: Integration (assembled server)  
  Steps: Start sign-in 25 times in a burst from one client address, then once from another, then load the home page from the first.  
  Expected: The first 20 starts are allowed; later ones get 429 with `Retry-After`. The other client and ordinary pages are not limited.  
  Evidence: `internal/app` › `TestServerRoutesRegression/sign-in_is_rate_limited_per_client`; `internal/platform/httpx` › `TestRateLimitPerClient`

## 2. Sessions (TC-AUTH-SES)

- [x] **TC-AUTH-SES-001 · Idle and absolute session timeouts are enforced**  
  Type: Edge · Priority: P1 · Level: Integration  
  Steps: Use a session past 30 minutes idle; renew one repeatedly towards its 24-hour limit.  
  Expected: An idle-expired or absolutely-expired session is refused; renewing never extends past the absolute deadline.  
  Evidence: `internal/auth` › `TestSessionTimeouts`

- [x] **TC-AUTH-SES-002 · Sessions live in PostgreSQL and hold only identity and CSRF token**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL)  
  Steps: Sign in; inspect the stored session; rename the GitHub user; restart the session manager; sign out.  
  Expected: The session survives a restart; it holds only the user ID and CSRF token; a rename keeps the same user; sign-out deletes the row.  
  Evidence: `internal/auth` › `TestPostgresLoginPersistenceAndIdentityRename`

- [x] **TC-AUTH-SES-003 · The production session cookie is host-only and hardened**  
  Type: Security · Priority: P1 · Level: Integration (assembled server)  
  Steps: Load the home page from the production configuration (HTTPS base URL).  
  Expected: The cookie is `__Host-previa_session`, Secure, HttpOnly, SameSite=Lax, with no Domain and Path `/`, so a preview subdomain cannot plant or read it.  
  Evidence: `internal/app` › `TestServerRoutesRegression/the_session_cookie_is_host-only,_secure,_HttpOnly_and_SameSite=Lax`

- [x] **TC-AUTH-SES-004 · A session that expires during in-page work sends the whole page to sign-in**  
  Type: Usability · Priority: P2 · Level: Integration + Browser  
  Steps: Let the session expire, then select a repository panel (an in-page request).  
  Expected: The server answers 401 with `HX-Redirect`; the browser navigates the whole document to sign-in instead of showing a broken panel.  
  Evidence: `internal/auth` › `TestExpiredSessionHTMXRedirectsWholeDocument`; browser › *expired sessions during panel selection navigate the whole document to sign-in*

## 3. Sign-out (TC-AUTH-OUT)

- [x] **TC-AUTH-OUT-001 · Sign-out is confirmed, CSRF-protected and final**  
  Type: Security · Priority: P1 · Level: Integration  
  Steps: Open the sign-out confirmation; post with an invalid token, from another origin, then correctly; replay the old cookie.  
  Expected: Invalid and cross-origin posts are refused; a valid sign-out clears the cookie and deletes the server-side session; the old cookie no longer works.  
  Evidence: `internal/auth` › `TestLoginRotationProtectedRouteAndLogout`

- [x] **TC-AUTH-OUT-002 · Sign-out opens as an accessible modal and invalidates the session**  
  Type: Usability · Priority: P2 · Level: Browser  
  Steps: Open the account menu, choose Log out, use the keyboard, confirm.  
  Expected: A modal over the current page traps focus, keeps the scroll position and returns focus on cancel; confirming signs out.  
  Evidence: browser › *logout modal preserves page scroll and traps and restores focus*; browser › *modal works on session page and confirmed logout invalidates session*

## 4. Access control and CSRF (TC-AUTH-ACC)

- [x] **TC-AUTH-ACC-001 · Every protected page sends anonymous visitors to sign in**  
  Type: Negative · Priority: P1 · Level: Integration (assembled server) + Browser  
  Steps: Request the dashboard, repositories, a workspace, a PR, its logs, Secrets, Outbound, Builds, Previews (and its former `/environments` address) and sign-out signed out; repeat as in-page (htmx) requests.  
  Expected: Page requests get 303 to `/auth/github/login`; in-page requests get 401 with `HX-Redirect`.  
  Evidence: `internal/app` › `TestServerRoutesRegression/protected_pages_send_anonymous_visitors_to_sign_in`; browser › *public home and protected dashboard access*

- [x] **TC-AUTH-ACC-002 · Mutations without a CSRF token, or from another site, are refused**  
  Type: Security · Priority: P1 · Level: Integration (assembled server)  
  Steps: Post to sign-out, policy, refresh, build, teardown, secrets and outbound without a token, and with a guessed token plus a foreign `Origin`.  
  Expected: Every one is 403; nothing changes.  
  Evidence: `internal/app` › `TestServerRoutesRegression/mutations_without_a_CSRF_token_or_from_another_site_are_refused`

- [x] **TC-AUTH-ACC-003 · A forged CSRF token on an in-page action explains itself and changes nothing**  
  Type: Security / Usability · Priority: P2 · Level: Browser + Unit  
  Steps: Change the Build form's hidden token, then press Build.  
  Expected: 403; the panel says "This action could not be verified. Refresh the page and try again."; after reload the PR still has no build request. Page (non-htmx) requests still get the HTML error page. *Defect D-01, fixed in this cycle.*  
  Evidence: browser › *a forged CSRF token is refused and changes nothing*; `internal/auth` › `TestPingRequiresLoginAndCSRF`

- [x] **TC-AUTH-ACC-004 · Developer tools exist only outside production**  
  Type: Security · Priority: P3 · Level: Integration  
  Steps: Request the developer tools with development and production settings.  
  Expected: Available in development; absent in production.  
  Evidence: `internal/dashboard` › `TestDeveloperToolsOnlyOutsideProduction`
