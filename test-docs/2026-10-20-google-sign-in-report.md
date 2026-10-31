# F7 · Google sign-in and email Reviewer invitations

Verification date: 20 October 2026 (Asia/Calcutta). Branch `mvp-complete`,
uncommitted F7 changes over `bb4f970`. Feature behavior and decisions:
[docs/36](../docs/36-google-sign-in-and-reviewer-invitations.md).

## Scope and decisions

Authentication methods identify one local account; memberships still assign roles.
Google-only signup creates no Owner workspace. A manager can invite a Reviewer by
verified email and share the copied link. Acceptance requires explicit consent and
matching verified identity. Reviewers can submit findings/comments and use granted
private previews without GitHub. Technical roles require a connected GitHub identity
and an explicit role assignment; linking never promotes a member.

Account linking starts from a signed-in profile through CSRF-protected POST and
proves control through the provider callback. Email does not auto-merge accounts;
an identity already belonging to another account is refused. Separately created
accounts cannot be merged in this MVP. No invitation emails are sent. Google
credentials are optional env configuration, not embedded in application code.

Migration 46 preserves existing users/sessions, makes GitHub ID optional, adds
Google subject and verified email, email invitations and technical-role constraints.
Deletion and independent journal v2 cover Google identities; v1 remains readable.
Rollback refuses when Google identities or email invitations would be lost.

## Automated evidence

| Check | Result |
| --- | --- |
| Focused auth, config, accounts, workspaces and preview access tests | Pass against disposable PostgreSQL, including identity linking, role restrictions, invitations, erasure/restore and migration roundtrip/refusal |
| Signed Google ID tokens over fake TLS | Pass: signature, issuer, audience, expiry, nonce, verified email, missing subject/token, exchange failure and S256 PKCE |
| Provider-bound callback and explicit linking | Pass: provider confusion, state/cookie validation, replay, CSRF, same-user linking, ordinary login does not link |
| Initial Google stakeholder browser journey | Pass, 3 scenarios: JavaScript on/off review and existing GitHub account linking/return |
| Final build, vet and lint | Pass, 0 lint issues |
| Full existing Go suite | Pass across all 52 packages with tests; packages without tests compile. Initial termination resumed only unfinished packages; corrected config-example case rerun passed |
| Existing browser suites | Pass, 94/94: general UI 55, workspaces 14, setup 5, review 6, test data 3, usage 4, accounts 7 |
| Final Google browser and captures | Pass, 3/3 scenarios (27.36 s); seven views at both widths/themes reviewed, plus mobile connection captures |
| Four real worker/Docker journeys | Pass (708.25 s total): backend, reference, standard and persistent-data journeys; includes F3 usage accounting, F4 disconnect erasure and F5 missed-uninstall recovery |
| Affected real private routing | Pass: real Traefik configuration recovery, authentication and revocation (11.83 s test, 35.62 s package) |
| Dependency vulnerability scan | Pass: 0 affecting the application; one module advisory has no imported/called vulnerable package |

Browser assertions include forged-CSRF refusal, Google-only workspace creation
refusal, attempted takeover of an existing GitHub identity, a real saved finding,
no horizontal overflow and no JavaScript/CSP errors. Screenshots cover login,
welcome, Google-only profile, email invitation modal, consent, review and linked
profile at 1440/390 px in light/dark. Screenshot review caught and corrected raw
login-link styling, unpadded connection cards and a stale GitHub-only consent label.
Email invite activity also renders the address without a spurious username prefix.

The first verification runs found test-fixture assumptions (build status, radio
selector and a fake GitHub user's missing numeric ID) and a provider-neutral photo
button expectation. These were corrected; they did not require weakening access
controls. Lint findings were resolved through a small invitation helper and
standard error capitalization. The environment-example contract caught missing optional-setting
labels; both examples were corrected and the config suite passed. One browser
rerun used the snap binary directly and failed to load its libraries; the documented
`/snap/bin/chromium` launcher passed. The full Go process was terminated with
exit 143 before finishing; only its unfinished packages were resumed. Logs remain in `/tmp/previa-google-*.log` locally.

## Reproduction and limits

Use a disposable PostgreSQL database migrated to Goose 46 and River 007. Set
`TEST_DATABASE_URL` for direct commands; see
[development setup](../docs/02-development-setup.md) for env loading and browser
prerequisites. Run `make build lint`, `go test -p=2 -timeout=30m ./... -count=1`,
`make test-google-browser`, and the existing browser targets. `make test-browser test-workspace-browser test-setup-browser test-review-browser
test-test-data-browser test-usage-browser test-accounts-browser` covers existing UI.
`TEST_ROUTING=1 go test ./internal/routing -run '^TestRealPrivateRouting' -count=1`
checks the affected private-preview route. `make test-e2e`
uses real Docker/BuildKit/Traefik with a test GitHub provider. Google browser
fixtures use a test-only provider; separate signed-token tests exercise the real
OIDC verifier. No production data or GitHub writes are involved.

Live Google consent is not claimed: the owner must register a Google Web OAuth
client, set `GOOGLE_OAUTH_CLIENT_ID`/`GOOGLE_OAUTH_CLIENT_SECRET`, register the exact
`APP_BASE_URL/auth/google/callback`, migrate the intended database and restart the
server. The development database and user's running processes were left alone.
Manual acceptance rows M1–M9, policy review, release rehearsal and deployment/live
checks remain owner gates; feature implementation does not mark them complete.
