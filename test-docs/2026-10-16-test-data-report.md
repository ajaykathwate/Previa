# Ready-to-test sample data (Feature 5) · 2026-10-16

## Delivered behavior

- **Seeding stays the repository's `setup`.** Starter kits offer a migrate-and-seed
  preset per stack and say seeds must be repeatable and synthetic. Every deployment
  starts from an empty database and is seeded again.
- **Setup on the review page.** The Preview card reports the serving deployment's
  *Setup: Completed in 12.4 s* or *No setup declared*. It adds a *Test data* note when
  the deployment runs managed databases. When a newer commit failed in its setup
  command, it says so while the earlier deployment keeps serving. The facts come from
  the newest publication, never from the failed attempt.
- **Reset test data.** A confirmation modal over the page posts the pull request's
  Build action as a Rebuild with `reset=true` and `return=review`. It is offered only
  when a Rebuild applies now and the viewer can build. The request is recorded as
  `preview.reset_requested` in the same transaction. The page returns with *Reset
  requested…* or a notice saying why nothing was reset.
- **Test access.** It is repository-level and has two parts:
  - an account shown to everyone with preview access;
  - an optional password, sealed with the secrets keyring and bound to the
    repository and the purpose `previa:test-access`.

  Owners and Admins edit it in place. Members with preview access reveal the
  password with a `no-store` POST: JSON for the page script, or the page itself
  without JavaScript. Each reveal is recorded and spends review allowance. Saves wake
  live pages; reveals and key rotations do not. A revealed value survives other live
  refreshes for two minutes. Without encryption keys, only the account can be stored.
- **Key rotation.** `make secrets-reencrypt` now also re-encrypts test access and
  prints separate `Test access:` lines.
- **Reference journeys.** An HTTP API with SQL data (`shop`) and a web app with a test
  login (`notes`) are built from their own Dockerfile and run through the worker's
  pipeline on real Docker. They are documented as customer copies in
  [31](../docs/31-test-data.md).

## Implementation and setup

Migration 0040 adds `preview_components.setup_ms` and `review_test_access`, with a
realtime trigger that fires on saves only. New binaries require Goose 40 (River
stays at 007). Run `make migrate-up`, then restart the server and the worker. There
is no new configuration: test access passwords use the existing
`SECRETS_ENCRYPTION_KEYS`. The migration was also applied, rolled back and
re-applied in a scratch schema of the test database. See
[31](../docs/31-test-data.md) and [setup](../docs/02-development-setup.md).

## Verification

| Check | Result |
| --- | --- |
| `make build` (Tailwind, templ, sqlc, Go build) | Passed |
| `make lint` (`go vet`, pinned golangci-lint) | Passed, 0 issues |
| `go test ./...` against PostgreSQL (isolated schemas) | 47 of 47 packages passed |
| `make test-e2e` on Docker Engine 29.8.1 | `TestRealEndToEnd`, `TestRealBackendEndToEnd` and the new `TestRealReferenceJourneys` passed (407 s in total; the reference journeys took 99 s) |
| `make test-review-browser` | 6 of 6 scenarios passed (one new), with no JavaScript or CSP errors |
| `make test-workspace-browser`, `make test-setup-browser` | 14 of 14 and 5 of 5 passed |
| `make test-browser` (in-memory UI suite) | 55 of 55 passed |
| Screenshots | The review page with setup facts and the test access card, the Reset confirmation, test access being edited, a revealed password after a live refresh: light and dark at 1440 px, phone at 390 px. Fixed: the account's Copy button wrapping onto its own line (now icon-only, as in field rows) |

Found and fixed during verification:

- **Setup facts disappeared after a failed update.** The first real run showed it:
  the review joined the environment's applied revision, which a failed update moves
  to the failed attempt. That also hid Feature 3's *Open docs*. The page now reads
  the newest publication's components.
- **A reveal hid the password it had just revealed.** The reveal's bookkeeping woke
  the page's own live refresh, which re-rendered the card masked. Reveals no longer
  notify, and the page keeps a revealed value across other refreshes unless test
  access changed.
- **A longer Starter kits note made a guidance card taller than the viewport.**
  The note was shortened; the full guidance is in *Seed data for reviewers*.

## Test cases

| ID | What is checked | Evidence |
| --- | --- | --- |
| TD-01 | The API journey: two seeded orders; a tester's order removed by Reset (deployment 2 of the same commit, one timeline event by the requester); the setup time recorded and shown; a failing seed fails with `setup_failed`, names the seed in the log, keeps deployment 2 serving its data, and leaves no container of the failed candidate | `TestRealReferenceJourneys` |
| TD-02 | The web app journey: a repository secret gives the seed a preview-only account; the build log never contains the password; test access saved through the review service is stored sealed and revealed equal to the secret; it signs in (303, a session, three notes); a wrong password gets 401; a fork's preview has no test login and says so; teardown leaves no container, image, network or volume | `TestRealReferenceJourneys` |
| TD-03 | Test access: nothing to reveal at first; Developers and Reviewers cannot set it; Admins and Owners can; stored sealed with key version 1; a Reviewer's view; a stale form refused; a repeated save is no change; reveal by a Reviewer recorded and shown; outsiders and ungranted members refused; a value moved to another repository, or a repository secret placed there, does not open; removing the password keeps the account; clearing both removes it; the timeline has kinds only, never the password; input limits | `TestTestAccessIsSealedAndRevealedOnRequest` |
| TD-04 | Without encryption keys a password is refused with a reason, the account is saved, nothing is revealable | `TestTestAccessWithoutEncryptionKeepsTheAccountOnly` |
| TD-05 | Rotation lists values on older keys, re-seals them, skips a value changed since it was read, counts by version; reveal works with the new keyring and not with one lacking the active key; `secrets-reencrypt`'s test-access pass reports counts only | `TestTestAccessFollowsKeyRotation`, `TestReencryptionCoversTestAccess` |
| TD-06 | A save signals the repository topic without data; a reveal does not (checked against a later signal, in commit order) | `TestTestAccessSavesWakeLivePagesButRevealsDoNot` |
| TD-07 | Forms: an Owner saves (redirect, toast, account shown, password never in the page); a refused password re-renders the account but not the password; a Reviewer's save is 404; a reveal without CSRF is 403; no-JS reveal returns the page with the value and `no-store`, `no-referrer`; JSON reveal returns the value with `no-store`; the last reveal is shown; nothing to reveal gives a JSON 404 or a notice | `TestTestAccessSavesAndRevealsWithAndWithoutJavaScript` |
| TD-08 | Setup facts (*12.4 s*, Test data, docs); Reset offered to Owners and Developers, not Reviewers; not while building, without repository access, with conflicts, a source to refresh, a teardown requested or a closed pull request; after a failed setup the earlier deployment's facts and docs stay, the note names the setup command, and Reset is withdrawn; *No setup declared* | `TestTheReviewShowsSetupAndOffersReset` |
| TD-09 | Reset only with Rebuild; a plain Rebuild records nothing; a reset is recorded once with its revision, commit and requester; a repeated submission is one reset; if it cannot be recorded, no build is requested | `TestResetTestDataIsARebuildRecordedInItsTransaction` |
| TD-10 | The Build action returns to the review page with *saved=reset*, or with *build-changed* and *build-closed* notices | `TestResetFromTheReviewPageReturnsThere` |
| TD-11 | Setup duration recorded with each candidate, none without setup, clamped when absurd | `TestRecordComponentsKeepsOnlyServedRows` |
| TD-12 | Every Starter kit setup preset, including the new migrate-and-seed ones, passes Previa's configuration rules | `TestEveryKitPassesPreviasConfigRules` |
| TD-13 | Browser: setup facts; the Reset modal opens in place with the right form, Escape closes it; test access added in place with a toast, the password absent from the page; a Reviewer reveals it, a live refresh keeps it, a reload drops it and shows *Last revealed by*; the reset toast; no-JS reveal | `make test-review-browser` |

## Remaining limits

- Test access is one value per repository, carried across rebuilds. Binding it to
  one deployment is deferred. The third reference journey (a background process) is
  deferred.
- While a build runs, the page says the commit is being built instead of showing a
  separate *Setup declared* state.
- The journeys ran on a local Docker Engine with a fake GitHub. No hosted or live
  GitHub acceptance was run.
- The pull request page still reads its *Runs with* and docs details from the
  environment's applied revision. After a failed update it stops showing the serving
  deployment's services and docs until the next successful one. This behaviour is
  unchanged and outside this feature; the review page has the fix.
