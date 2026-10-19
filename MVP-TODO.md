# Previa MVP: remaining work and deployment checklist

This file turns the release plan agreed on 16 October 2026 into checklists: what is
left to build, how each part is accepted, and how Previa gets deployed. It sets no
dates or deadlines.

The product's source of truth stays
[README-MVP-PRODUCT-EXPANSION.md](README-MVP-PRODUCT-EXPANSION.md) (cited below as
"spec §…"), [README.md](README.md) and [DESIGN.md](DESIGN.md); deployment follows
[docs/25](docs/25-production-deployment.md) and [docs/22](docs/22-mvp-operations.md).
If this file and those documents disagree, they win and this file is corrected.

## How to use this file

- Work from top to bottom: **Part 1 Features → Part 2 Release acceptance → Part 3
  Deployment.** Deployment starts only after every feature is built, tested by
  Claude and tested manually by the owner (the gate at the end of Part 2).
- Tick a box only when its evidence exists: a passing test or command, a reviewed
  screenshot, or the owner's manual test.
- **Owner** marks work only the owner can do. **Decision (owner)** marks a choice
  that blocks the item until it is answered. **Proposed** marks an idea that is not
  in the spec; it is built only after the owner approves it and it is specified.
- Working rules ([AGENTS.md](AGENTS.md) and standing instructions):
  - Claude commits only when the owner asks, on the checked-out branch. Work is
    pushed to `main`, never `master`; the owner merges `main` into `master`.
  - The UI stays compact: modals open in place, new screens are screenshotted at
    1440 and 390 px in light and dark.
  - docs/02 is updated whenever configuration, migrations or commands change, and
    HANDOFF.md stays under 100 lines.

## Where things stand

| Item | State | Evidence |
| --- | --- | --- |
| Feature 1: shared team workspaces | Done, committed (`adfef83` and follow-ups) | [docs/28](docs/28-shared-workspaces.md) |
| Feature 2: guided onboarding | Done, committed (`3562a78`) | [docs/29](docs/29-guided-onboarding.md) |
| Feature 3: native review and feedback | Done, committed (`2ddaaef`) | [docs/30](docs/30-reviews.md), [report](test-docs/2026-10-16-review-feedback-report.md) |
| Feature 5: ready-to-test sample data | Done, committed (`ea87e60`); walked through (F0); the owner's hands-on pass is in Part 2 | [docs/31](docs/31-test-data.md), [report](test-docs/2026-10-16-test-data-report.md) |
| F1: spec corrections and the PR page fix | Done by Claude, not committed | [report](test-docs/2026-10-17-persistent-test-data-report.md) |
| Feature 5B: persistent test data (F2) | Done, not committed; defaults decided; walked through; the owner's hands-on pass is in Part 2 | [docs/32](docs/32-persistent-test-data.md), [report](test-docs/2026-10-17-persistent-test-data-report.md) |
| Code on GitHub | Not pushed: all of the above is on the local `mvp-complete` branch; GitHub's `main` is at `6b07c5b` | `git log origin/main..mvp-complete` |
| Out of this release | Feature 4, GitHub review-status projection (step E), paid subscriptions (§9.4, step J), the third reference journey, notification integrations | spec §1.2 |

Order of the remaining work, as agreed: F0 → F1 → **F2 Feature 5B** → **F3 and F4
Feature 6 (free plan)** → F5 missed-uninstall detection → F6 the early-access
decision → Part 2 → Part 3.

---

## Part 1 · Features

### F0 · Close out Feature 5

**Outcome:** Feature 5 is confirmed by a walkthrough of its manual test rows, and
the owner's own hands-on pass is scheduled with the release acceptance.

- [x] **Owner:** apply the new migration to the local development database:
  `make migrate-up` (Goose 40), then restart the server and the worker. (Done; the
  development database is at Goose 41 now, checked read-only on 17 October.)
- [x] **Claude, 2026-10-17:** walked through
  [technical-docs/user-guide.md](technical-docs/user-guide.md) checklist section F2,
  rows R12–R18, through their automated equivalents on real Docker and in Chromium
  (row-by-row evidence in the
  [report](test-docs/2026-10-17-persistent-test-data-report.md#manual-checklist-walkthrough)).
  Nothing to fix.
- [x] **Owner (decided 17 October):** the owner's own hands-on pass of these rows
  moves to Part 2's full manual pass, with every later feature's rows. Storing a
  test-access password needs `SECRETS_ENCRYPTION_KEYS` in `.env`.

### F1 · Spec corrections and a known fix

**Outcome:** 5B no longer depends on the deferred Feature 4, and the pull request
page agrees with the review page after a failed update.

- [x] Remove Feature 4 dependencies from the 5B specification:
  - §11.2, the step G explanation ("needs Features 3/4 publications and review
    rounds");
  - §8B.9 ("Reviews and activity (Features 3/4)");
  - §8B.13 ("a new review round").

  Keep the publication and activity references, which exist.
- [x] Pull request page: after a failed update, *Runs with* and the docs link
  disappear, because they read the environment's applied revision (the failed
  attempt). Read the serving deployment (the newest publication), as the review
  page already does. **Evidence:** a test with a failed update; the PR page and the
  review page show the same setup, services and docs.
  (Done: the PR page, the GitHub comment's snapshot and component retention read
  the newest publication; three tests cover them.)

### F2 · Feature 5B: persistent test data (spec §8B, delivery step G)

**Outcome:**
- Previews open with useful data, and reviewers' records survive new commits and
  Rebuilds.
- Starting data is prepared once per repository instead of in every pull request,
  and pull request migrations run against existing rows.
- No database is ever shared between pull requests.

Scope:

- [x] **Test data tab** for Owners and Admins, CSRF-protected and closed to
  repository YAML. Its settings and defaults:
  - Test data: off;
  - Keep test data between builds: on;
  - Use repository baseline: on;
  - Baseline branch: the repository's GitHub default branch;
  - Refresh baseline automatically: on.

  (§8B.3 item 1) Developers with a grant see it read-only and may refresh.
- [x] **`seed` key** in `.previewrc.yaml`:
  - argv, with the same limits as `setup`;
  - needs a PostgreSQL or MySQL service;
  - runs only on an empty database and in baseline builds;
  - part of the configuration hash.

  (§8B.8)
- [x] **Repository baseline** (§8B.4):
  - a build that belongs to the repository, not a pull request, following build
    steps 1–8;
  - triggers: Test data turned on, a merge into the baseline branch (the existing
    `closed` webhook), the **Refresh baseline** button (confirmed in a modal), a
    daily freshness check, and an unusable baseline;
  - one refresh at a time, ordered by a refresh-sequence fence, with a minimum
    interval between automatic refreshes;
  - safe failure categories;
  - dumps encrypted with the platform keyring, size-limited and checksummed.

  A failed refresh keeps the previous baseline and affects no pull request.
- [x] **Kept PR data** (§8B.5):
  - copied from the pull request's serving candidate just before `setup`, streamed
    container to container;
  - per service kind, only into the same or a newer major version;
  - never stored by Previa.
- [x] **Starting-data selection with fallback** (§8B.6):
  - sources tried in order: kept data, then baseline, then empty;
  - `seed` runs only on an empty database;
  - at most two fallbacks, and copy failure categories are recorded;
  - the source used is stored on the candidate and referenced by its publication.
- [x] **Reset test data gains a source choice**: repository baseline, or empty
  database and seed. The choice is recorded on the activity event (§8B.7).
- [x] **Visible outcomes** (§8B.3 items 7–9):
  - the PR page shows the source, any fallback and its reason, and the Setup and
    Seed outcomes;
  - the review page shows a one-line reviewer version;
  - the Rebuild modal says data is kept and points to Reset (Rebuild has no modal:
    the note is beside *Runs with* on the PR page);
  - the GitHub comment gets one source line;
  - the Test data tab shows status, **Refresh baseline** and **Delete baseline**
    (confirmed in a modal).
- [x] **Data and access rules** (§8B.10):
  - Redis always starts fresh;
  - baseline builds get no secrets and no outbound approvals;
  - Owners and Admins change settings; Developers with a grant refresh and reset;
    Reviewers see the source line only;
  - turning Test data off deletes baselines after a confirmation.
- [x] **MySQL** as well as PostgreSQL (§8B.4 step 6, §8B.13). Proven on real Docker
  at the runtime level (copy, capture, restore); the web journey runs PostgreSQL.
- [x] **Housekeeping and crash cleanup** for baseline containers, networks and files
  (§8B.9, §8B.11).
- [x] **Starter kits and the reference journeys** show the setup/seed split for
  Node.js (Prisma), Rails, Django and Go (goose) (§8B.8).
- [x] **Operator settings**: `PREVIEW_BASELINE_DIR`, `PREVIEW_BASELINE_MAX_SIZE`,
  `PREVIEW_DATA_COPY_TIMEOUT` and `PREVIEW_BASELINE_MIN_INTERVAL` (§8B.8), with the
  decided defaults below (a temporary directory locally, the worker's `baselines`
  volume in production).
- [x] **Decision (owner, delegated to Claude on 17 October):** 64 MiB per dump (the
  spec proposed 128), 2-minute copies, automatic refreshes at most hourly per
  repository (the spec proposed 10 minutes), baselines on the worker's volume and
  not backed up. Reasons and the record: spec §8B.8 and §13.
- [x] Until F3 exists, baseline builds use today's admission (host capacity and the
  existing operator limits). F3 must count them (§8B.4 step 3, §9.3). They take
  build slots, count toward `BUILDS_PER_ACCOUNT` and add their time to
  `workspace_build_usage`.
- [x] **Development database at Goose 41** (checked read-only on 17 October). Restart
  the worker, which was started before F2 (`go run ./cmd/worker`); the server
  restarts by itself under Air.
- [x] **Claude, 2026-10-17:** walked through the user guide's checklist section F3,
  rows T1–T14, through their automated equivalents (row-by-row evidence in the
  [report](test-docs/2026-10-17-persistent-test-data-report.md#manual-checklist-walkthrough)).
  The owner's own hands-on pass is in Part 2.

**Done when:** every §8B.13 completion criterion holds:
- the baseline is built from the default branch and from a chosen branch;
- it refreshes on merge, the button and the daily check, coalesced and fenced;
- a pull request's first build restores it and runs only new migrations;
- reviewer records survive a new commit and a Rebuild, while a parallel PR and a
  fork never see them;
- both Reset choices work;
- incompatible histories fall back with their reason;
- crash injection leaves nothing behind, and disconnecting deletes baselines (after
  F4);
- no data, credentials or dump output reach logs, live streams, activity or GitHub;
- the reference journeys pass with Test data on (PostgreSQL and MySQL) and off.

The common gate below must also pass.

- [x] **Claude, 2026-10-17:** the common gate passed for F1 and F2: build, lint
  0 issues, `go test ./...` 48 of 48, migration 0041 up/down/up, browser suites
  (the new `make test-test-data-browser` 3 of 3, review, setup, workspace, UI),
  `make test-e2e` 4 of 4 (Test data off and on), `make test-runtime` 11 of 11,
  screenshots at 1440 and 390 px in both themes reviewed, docs updated
  ([report](test-docs/2026-10-17-persistent-test-data-report.md)). MySQL is proven
  at the runtime level, not in a web journey; crash recovery by unit tests.

### F3 · Feature 6, free plan, part H: usage and free limits (spec §9.3, §9.7, step H)

**Outcome:**
- Every workspace has finite free limits it can see.
- Real build execution is counted exactly once.
- Blocked work says why, until when, and what to do.
- Existing previews, reading and feedback are never cut off by a limit.

F3 makes finite allowances per-workspace and visible (§9.1). Existing operator
settings (`BUILDS_PER_ACCOUNT`, `BUILD_MINUTES_PER_ACCOUNT_DAY`) remain additional
workspace safety ceilings; zero disables only that extra ceiling.

- [x] **Free plan entitlements** per workspace (`workspace_entitlements`): finite
  limits, no purchase controls and no paid states (§9.1, §9.3).
- [x] **Execution accounting** (§9.7):
  - reservations at admission, at most 60 seconds per slice (spec §13);
  - execution segments with a key per attempt, so a replay never double-counts;
  - crash reconciliation, and splitting at UTC boundaries;
  - limits re-checked before execution and before publication.

  The existing `account_build_usage` is labelled legacy and never billed. A
  documented cutover starts the new accounting.
- [x] **Limits** (§9.3 table):
  - concurrent builds per workspace;
  - included build minutes, counting failed and superseded work and F2's baseline
    builds and fallback attempts;
  - the operator's daily ceiling;
  - active preview slots, where a same-PR replacement reuses its slot.

  Host capacity and preview lifetimes stay as they are.
- [x] **Usage page** (§9.3):
  - plan and period;
  - consumed and reserved minutes;
  - the running-build limit and preview slots;
  - the retention and expiry policy;
  - blocked work;
  - measured timestamps and pending reconciliation.

  Authorized developers also see a breakdown per repository.
- [x] **Behavior at a limit:** only compute is blocked. Reads, feedback, account
  access, cancellation and teardown keep working, and no data is deleted (§9.3).
- [x] **Decision (owner, delegated 18 October 2026):** 300 build minutes per UTC
  calendar month, one concurrent build and two active preview slots per workspace.
  One build gives teams a fair share of the host; two slots allow parallel PR
  reviews; baseline work shares the 300 minutes. Host budgets remain authoritative.
- [x] **Decision (owner, 18 October 2026):** no hosted users exist. Migration 0042
  grants existing active workspaces fresh free allowances and preserves legacy
  technical counters without importing or billing them.

**Done when:** spec §9.10's first three criteria hold:
- totals reconcile with attempt records, and replays never count twice;
- concurrent members cannot exceed reservations, slots or global ceilings;
- running out and the reset reach the specified state.

The common gate must also pass.

- [x] **Codex, 2026-10-18:** F3's implementation and automated common gate passed:
  build, vet/lint (0 issues), all 49 Go test packages against PostgreSQL, migration
  0042 up/down/up, usage race tests, UI 55/55, workspace 14/14, setup 5/5, review
  6/6, Test data 3/3, Usage 4/4, runtime 11/11, routing 8/8 and all four real-Docker
  end-to-end journeys. Reviewed Owner/Developer Usage at 1440/390 in both themes.
  Docs, user-guide rows U1–U10 and HANDOFF are updated. Evidence and recovery
  boundaries: [F3 report](test-docs/2026-10-18-usage-and-free-limits-report.md).
  The owner's hands-on acceptance and commit remain separate steps.

### F4 · Feature 6, free plan, part I: accounts, exits, support, status and policies (spec §9.5, §9.6, §9.8, step I)

**Outcome:** people can leave Previa and remove their data completely, reach
support, see whether the service works, and read in plain language what Previa
stores and why.

| Item | Today |
| --- | --- |
| End other sessions | Implemented; current browser retained, epochs/streams/private grants revoked |
| Leave workspace | Exists after handover ([docs/28](docs/28-shared-workspaces.md)) |
| Disconnect a repository or installation in Previa | Implemented with immediate fences and resumable physical cleanup |
| Delete workspace | Owner requests ordered deletion, including connected repositories and members |
| Delete personal account | Implemented after workspace handover/deletion; shared attribution pseudonymized |
| Help and support | Persisted requests, audited operator console and configurable contact |
| Status page, policy pages | Actual dependency checks/incidents; public Privacy, Terms and Acceptable use |

- [x] **End other sessions:** a user-to-session index or authorization epoch,
  rechecked on requests and live streams (§9.5, §9.7).
- [x] **Leave workspace:** confirm it revokes grants, streams and cookies as §9.5
  requires.
- [x] **Disconnect a repository** (§9.5):
  - it becomes unavailable at once, and cleanup progress is visible;
  - new work is fenced, and intents and baseline refreshes are cancelled;
  - routes, containers and services are removed;
  - secrets, test-data baselines and settings are deleted;
  - a later webhook cannot reconnect it.
- [x] **Disconnect an installation:** the same cleanup for each of its
  repositories. Explain that uninstalling the GitHub App is a separate step on
  GitHub.
- [x] **Delete workspace:** an ordered, resumable operation with visible
  progress. It records intent and denies new writes, fences work, and removes
  routes and previews. It revokes grants and sessions, and deletes secrets,
  configuration, baselines and review content. The free plan has no provider
  subscription step (§9.5).
- [x] **Delete personal account:**
  - allowed only after the user's owned workspaces are handed over or deleted;
  - erases personal fields and revokes sessions;
  - keeps shared attribution under a pseudonym;
  - never removes other people's team data (§9.5).
- [x] **Optional (§10.3):** a final "Disconnected" GitHub comment while App access
  still allows it, with finite retries.
- [x] **Help and support** (§9.6):
  - a support contact, FAQ links, service availability and a copyable diagnostic
    reference;
  - a persisted support request form;
  - an operator-only page to read and resolve requests;
  - the support operator role comes from configured identities, and operator
    actions are recorded.
- [x] **Status page** (§9.6): real build and routing availability with the time it
  was checked, an operator-only form for incidents, and unknown states shown as
  unknown.
- [x] **Policy pages** in plain language (§9.6): stored data, retention, GitHub
  permissions, what disconnecting and deleting do, and the usage limits. Paid
  cancellation terms are deferred with §9.4.
- [x] **Restore-safe deletion** (§9.8): an encrypted, bounded deletion journal sent
  through the backup channel. The restore procedure applies it before serving
  again, which updates [docs/25](docs/25-production-deployment.md) (B5 and B6) and
  [docs/24](docs/24-incident-runbook.md).
- [x] **Decision (owner):** support contact supplied; configured in ignored local `.env`.
- [x] **Decision (owner):** leave operator IDs empty for now. Env comments explain
  numeric GitHub user IDs; empty disables operator access.
- [ ] **Decision (owner):** approve the privacy and terms text. Claude drafts it
  from the actual behavior; the owner approves or has it reviewed (spec §13).
- [x] **Decision (owner):** delegated limits: 16 KiB/message, five requests per UTC
  day per user, resolved retention 90 days; all configurable in env.

**Done when:** spec §9.10's last three criteria hold:
- leaving or removal revokes access at once;
- personal deletion keeps others' work;
- repository and workspace deletion survive crashes until every step finishes;
- a webhook cannot undo a disconnection;
- a deleting workspace cannot create work or accept an invitation;
- help, support, status, retention pages and delete controls are real and
  reachable.

**Engineering complete (19 October 2026).** Automated Go, lint, browser,
real-Docker and encrypted restore checks passed, with targeted reruns documented
in the [F4 report](test-docs/2026-10-19-accounts-support-and-deletion-report.md).
Owner hands-on acceptance and pre-launch policy review remain separate steps.
The development database has not been migrated; follow docs/02 before restart.

### F5 · Detect missed GitHub App uninstalls ([docs/27](docs/27-roadmap.md), Prompt 14)

**Outcome:** previews still come down when Previa misses a webhook for an
uninstall, a suspension or a removed repository (for example while it was offline),
and reinstalling works.

- [ ] A periodic reconciliation job using App (JWT) authentication. It only
  removes access, through the existing revocation path, in one short transaction
  with the existing fences.
- [ ] **Fail safe:**
  - outages, 5xx responses, rate limits and partial listings change nothing;
  - a 401 or a wrong App ID stops the pass and alerts;
  - a circuit breaker stops a pass that would revoke too much.
- [ ] Faster detection when creating an installation token reports the
  installation gone or suspended. This wakes the reconciler; nothing is revoked
  from the request path.
- [ ] Reinstalling works after a missed deletion, for the same owner only.
- [ ] Metrics with bounded labels, an alert whenever reconciliation finds a change
  (logged at warning or error level, so Sentry shows or emails it in production;
  a Prometheus rule as well for the optional observability profile), and a
  runbook entry for webhook delivery outages.
- [ ] Every test listed in Prompt 14, plus a `make test-e2e` phase that drops the
  uninstall webhook.
- [ ] Docs: docs/04, 05, 12 and 14, docs/02 if configuration changes, the user
  guide's section 19 and checklist, the tech stack, and the schema diagram if the
  schema changes.

**Done when:** the reconciliation is enabled and documented, and every test above
passes. The common gate must also pass.

### F6 · Early-access gate (Proposed, not in the spec)

**Why it was raised:** Previa builds and runs code from the repositories of anyone
who signs in, on one server, and nothing limits who can sign up today.
[docs/25 "Account safety"](docs/25-production-deployment.md#account-safety-abuse-reports)
explains that abuse reports about previews go to the host.

- [ ] **Decision (owner):** one of:
  - (a) the operator approves each new workspace before it can connect
    repositories or build;
  - (b) an operator allowlist of GitHub accounts or organizations;
  - (c) no gate, relying on F3's free limits and docs/25's abuse controls.

  For (a) or (b), Claude first adds it to the spec, then builds it like any feature.

### The common gate (every feature)

- [ ] **Claude:** `make build`; `make lint` with 0 issues; `go test ./...` against
  PostgreSQL. A new migration is applied, rolled back and re-applied in a scratch
  schema.
- [ ] **Claude:** the browser suites: `make test-browser`, `test-workspace-browser`,
  `test-setup-browser`, `test-review-browser`, plus any new suite. These run with
  and without JavaScript and must show no CSP errors.
- [ ] **Claude:** the affected real-Docker suites: `make test-e2e` (which includes
  `make test-reference`), and `make test-runtime`, `test-routing` and
  `test-housekeeping` as affected.
- [ ] **Claude:** screenshots of every new or changed screen at 1440 and 390 px,
  light and dark, reviewed.
- [ ] **Claude:** documentation updated:
  - the DESIGN section and the feature's document in `docs/`;
  - docs/02 for migrations, configuration and commands;
  - the user guide and its manual checklist rows;
  - a test report in `test-docs/`;
  - HANDOFF.md and this file.
- [ ] **Owner:** the manual test, using the new checklist rows.
- [ ] **Owner:** asks for the commit; Claude commits on `mvp-complete`.

---

## Part 2 · Release acceptance (after every feature, before deployment)

**Outcome:** the release is shown to be complete on local and staging resources,
and both of us agree it is ready to deploy.

- [ ] **Claude:** a full automated regression cycle as described in
  [test-docs/README.md](test-docs/README.md), recorded in a new dated report.
- [ ] **Owner:** a full manual pass through the user guide's
  [Manual test checklist](technical-docs/user-guide.md#manual-test-checklist),
  including the rows added by F2–F5: Feature 5's rows R12–R18 and Feature 5B's rows
  T1–T14 (moved here from F0 and F2), and the rows F3–F5 add.
- [ ] The product journeys of spec §12.2 that can be proven before deployment:
  - new small team;
  - complete review;
  - deployment freshness;
  - offboarding and handover;
  - useful seed and reset;
  - persistent test data;
  - shared quota;
  - exit;
  - support and status;
  - retention and restore.

  The GitHub integration journey needs real GitHub and is proven in Part 3 (D7).
- [ ] Spec §12.3 product measurements: confirm each one can be read from recorded
  data. **Decision (owner):** whether a small read-only report is needed to read
  them. If so, it is specified and built first.
- [ ] Spec §12.4: no placeholder actions or undisclosed manual steps; deferred
  items are labelled as not available; documents describe shipped behavior.
- [ ] Spec §1.2 status table updated: every row done except deployment.
- [ ] **Owner and Claude:** sign off "ready to deploy".

---

## Part 3 · Deployment

Deployment follows [docs/25](docs/25-production-deployment.md) step by step. The
final setup, agreed on 18 October 2026: one OVHcloud VPS-2 (4 vCores, 8 GB RAM,
75 GB NVMe) running Ubuntu 24.04, with PostgreSQL 17, the server, the worker and
Traefik in Docker Compose, the images built on the server from a release tag;
`previa.live` for the landing page and dashboard and a separate preview domain,
both on Cloudflare DNS; Cloudflare R2 for encrypted backups; GitHub for the code,
CI, sign-in and the App; Sentry (free) for errors, uptime and backup alerts by
email; Tailscale (free) for SSH, with port 22 closed to the internet; lazydocker
for logs on the server. It costs about **€8 a month plus VAT** (the VPS and the
preview domain) plus `previa.live`'s renewal
([docs/25 "What it costs"](docs/25-production-deployment.md#what-it-costs)). It then
works through the live proofs of
[docs/22](docs/22-mvp-operations.md#live-proofs-still-required-before-a-public-rollout).

### 3.1 What Claude needs from the owner, and how to give it

The owner provides these during the deployment phase, not before.

**How to hand things over safely:**
- **Non-secret values** (names, IDs, IP addresses, email addresses): tell Claude in
  the chat.
- **Secrets** (client secrets, tokens, private keys, passwords): never paste them
  into the chat. At the step that needs one, Claude gives the exact command or
  settings page, and the owner types or uploads the value directly on the server,
  in GitHub or in Cloudflare.
- **Secrets the server generates itself** need nothing from the owner except a
  recovery copy kept in a password manager: the database password, session secret,
  webhook secret and repository-secrets keyring (`setup.sh` creates them, docs/25
  Step 5).

| # | What | Why | How the owner gives it | Needed at |
| --- | --- | --- | --- | --- |
| 1 | An OVHcloud account with two-factor authentication, its identity check completed | To rent the server | Create it; nothing to send | D2 |
| 2 | ~~Decision: a separate staging VPS~~ **Decided 2026-10-18: none.** The production VPS is the rehearsal machine before launch; later drills use a server rented by the hour | No second monthly bill | Nothing to do | D9 |
| 3 | An SSH key on the owner's workstation (docs/25 Step 0) and a way for Claude to reach the server | To run the setup steps | Until Tailscale is set up (D2), SSH over the public IP; afterwards make `ssh ops@previa-prod-01` work from this WSL terminal. Either approve each command as Claude runs it, or run the commands Claude gives and paste back the non-secret output. Tell Claude which one | D2 |
| 4 | The VPS's model, location and public IPv4 address | Inventory, DNS records | In the chat | D2 |
| 5 | A Tailscale account (free Personal plan, two-factor authentication) and its app on the owner's workstation (on Windows, and inside WSL if `ssh` from WSL cannot reach the tailnet) | SSH without port 22 open to the internet (docs/25 Step 3, "Move SSH to Tailscale") | Create it and sign in to the app; approve the server when `tailscale up` prints its login link; disable the server's key expiry | D2 |
| 6 | The preview domain: a **separate registrable domain** bought on Cloudflare Registrar (about $10–11 a year for a .com; `previa.live` is already on Cloudflare) | Previews stay off `previa.live` (docs/25 Step 2 says why) | Buy it, then tell Claude its name in the chat | D2 |
| 7 | A Cloudflare API token with Zone DNS Edit and Zone Read, scoped to the preview zone | Automatic HTTPS certificates (DNS-01) | **Secret.** Create it in Cloudflare, keep it in a password manager, and paste it yourself into `/etc/previa/dns-credential` on the server with the command Claude gives (docs/25 Step 4) | D3 |
| 8 | A private Cloudflare R2 bucket and an API token scoped to it (access key and secret) | Encrypted off-server backups | The bucket name and R2 endpoint in the chat. **The keys are secret:** type them yourself into root's `rclone config` on the server (docs/25 B2–B3) | D5 |
| 9 | A backup recovery key made with `age-keygen` on the owner's recovery workstation (docs/25 B1) | Only this key can open backups | Send only the public `age1…` recipient in the chat. The private key stays offline, in two copies, never on the server | D5 |
| 10 | A production GitHub **OAuth App** (sign-in), with the field values Claude gives (`https://previa.live/…`) | Users sign in on the real domain | Register it on GitHub. Send the client ID in the chat; type the **client secret** yourself into `/etc/previa/previa.env` | D3 |
| 11 | A production **GitHub App**, with the URLs and permissions Claude gives (docs/25 Step 4: Contents read, Pull requests write, Checks write, the Pull request event) | Repository access, webhooks, PR comments | Register it on GitHub. Send the App ID, slug and client ID in the chat. Type the **client secret** into `previa.env`; upload the **private key** with `scp` and install it with Claude's command; copy the **webhook secret** from the server's `previa.env` into GitHub yourself | D3 |
| 12 | A monitored email address | Sentry's alerts, the certificate account (`ACME_EMAIL`) and the host's abuse notices | In the chat | D3 |
| 13 | GitHub repository settings: Actions enabled, `master` the default branch (docs/25 "CI and releases"; branch protection is a habit on the free plan) | CI runs on pull requests into `master` | Set them on GitHub; tell Claude when done | D1 |
| 14 | The server's **deploy key** (docs/25 Step 3) | The server clones and pulls the private repository; images are built there, so no registry credential is needed | Claude prints the public key on the server; add it in GitHub → repository → **Settings → Deploy keys**, read-only | D2 |
| 15 | Release approval | Only `master` commits that passed CI are tagged | Merge the `main` → `master` pull request after CI passes, then create and push the version tag (or explicitly ask Claude to) | D1 |
| 16 | ~~Where alerts go~~ **Decided 2026-10-18:** Sentry emails item 12's address; no Alertmanager or chat webhook | — | Nothing to do | D8 |
| 17 | A Sentry account (free Developer plan, two-factor authentication, the EU data region for a European VPS) with a Go project named `previa` (docs/25 S1) | Errors, logs, the uptime monitor and the backup's cron monitor | Create it; send the project's DSN in the chat or paste it into `previa.env` yourself (it is not a password, but keep it out of public places) | D4 |
| 18 | A disposable GitHub repository with the production App installed and an open pull request | The live GitHub proofs (`make test-source`, `make test-reporting`) | Create it; tell Claude its installation ID, repository ID and PR number in the chat | D7 |
| 19 | ~~Decision: gVisor~~ **Decided 2026-10-18:** off for invited early-access users; revisit before open sign-up (an optional test fits the pre-launch rehearsal, D9) | — | Nothing to do now | D9 |
| 20 | **Decision:** the account-safety settings from docs/25: `BUILD_EGRESS=registries`, the preview lifetime (`ENV_TTL_HOURS`) | Fewer abuse risks | In the chat | D4 |

### 3.2 Deployment checklist

**D1 · Code and release**
- [ ] **Claude** (when asked): push `mvp-complete` to `main`.
- [ ] **Owner:** open the pull request `main` → `master` when the work is ready to
  release (each push to `main` while it is open costs a CI run). When CI passes,
  merge it.
- [ ] **Owner:** prepare the repository (item 13).
- [ ] **Claude:** rehearse the release candidate locally: `make release-images` and
  `make prod-smoke` (docs/25 "Local staging smoke test").
- [ ] **Owner:** tag a `master` commit whose CI passed (`vX.Y.Z`) and push the tag
  (item 15). The tag publishes nothing; the server builds it in D6.

**D2 · Host** (docs/25 Steps 0–3)
- [ ] Step 0: the accounts exist (items 1, 5, 17) and the non-secret inventory is
  recorded in the owner's operations notes.
- [ ] Step 1: the VPS is ordered, the `ops` user created, SSH key-only.
- [ ] Step 2: the preview domain is bought (item 6); `previa.live` and the
  wildcard preview record are DNS-only. `dig` resolves `previa.live` and a test
  label under the preview domain to the VPS.
- [ ] Step 3: the deploy key is added (item 14), the release tag is checked out
  at `/opt/previa`, `setup.sh` has prepared the host (Tailscale and lazydocker
  installed).
- [ ] Step 3: SSH works over Tailscale (`ssh ops@previa-prod-01`), the server's key
  expiry is off, port 22 is closed to the internet (`ufw status` shows SSH on
  `tailscale0` only), and `nc` from outside the tailnet times out.

**D3 · GitHub integrations and credentials** (docs/25 Step 4)
- [ ] The OAuth App and GitHub App are registered with `https://previa.live` URLs
  (items 10–11).
- [ ] The App private key and DNS token are installed with the owners and modes
  docs/25 specifies.

**D4 · Production settings** (docs/25 Step 5)
- [ ] `/etc/previa/previa.env` is complete per the docs/25 table, including
  `SENTRY_DSN` (item 17), plus any setting F3–F5 add (listed in docs/02 by then)
  and the decisions in item 20.
- [ ] Both configuration checks pass: `server check-config` and
  `worker check-config` (docs/25 Step 7).

**D5 · Backups before production** (docs/25 Step 6, B1–B3)
- [ ] The recovery key exists offline (item 9), the R2 bucket and rclone remote are
  configured (item 8), and `BACKUP_AGE_RECIPIENT` and `BACKUP_REMOTE` are set.

**D6 · First start** (docs/25 Steps 7–9, then B4)
- [ ] The three images are built on the server from the tag
  (`dc --profile images build server worker egress`) and the stack starts.
- [ ] Certificates are issued with Let's Encrypt **staging**, then switched to
  production.
- [ ] The first real backup succeeds and the backup schedule is enabled (B4).

**D7 · Verify and live proofs** (docs/25 Step 10, then docs/22)
- [ ] Step 10: every process is ready, and migrations are at the release's version.
- [ ] Hosted GitHub webhooks are delivered and discovery works against a real
  installation.
- [ ] Real source of a real pull request: `make test-source` (item 18).
- [ ] PR feedback on a real pull request: `make test-reporting` (item 18).
- [ ] The production wildcard certificate is valid over real DNS-01.
- [ ] The whole pipeline on the host with a real repository: build, publish, the
  comment, Retry and Rebuild, expiry, teardown on close and on revocation.
- [ ] The release's own features on the host:
  - workspaces;
  - the Setup tab;
  - the review page;
  - test data with Reset and test access;
  - 5B baselines and kept data;
  - usage limits;
  - support, status and deletion.
- [ ] Capacity and disk pressure on the host's real resources, at
  `MAX_CONCURRENT_BUILDS` concurrent builds.
- [ ] The spec §12.2 GitHub integration journey.

**D8 · Monitoring and alerts** (docs/25 "Monitoring with Sentry" and "Logs, SSH
and day-to-day checks")
- [ ] S2: `server sentry-test` succeeds and the test issue appears in Sentry.
- [ ] S3: the uptime monitor checks `https://previa.live/` every minute.
- [ ] S4: the alert rule emails item 12's address; the test issue's email, an
  uptime failure email (temporarily wrong URL) and the cron monitor
  `previa-backup` with an OK check-in after B4 are all confirmed.
- [ ] Host checks are understood: `free -h`, `df -h`, `docker stats` and
  `lazydocker`, with docs/25's thresholds.

**D9 · Rehearsals before launch** (docs/25 "Rehearse recovery before launch", on
the production VPS while it holds only test data)
- [ ] A full restore: backup, OS reinstall from the Control Panel, B6 recovery,
  including F4's deletion journal; the recovery time is recorded.
- [ ] An upgrade and a rollback (U1–U5, then the image rollback).
- [ ] Optional: the gVisor test (item 19).

**D10 · Account safety** (docs/25 "Account safety: abuse reports")
- [ ] The abuse-notice email is monitored (item 12).
- [ ] The decisions in item 20 are applied. The kill switch `BUILDS_PAUSED` is known.
- [ ] F6's early-access decision is in effect.

**D11 · Launch**
- [ ] The first users are invited.
- [ ] The spec §12.3 measurements are read as agreed in Part 2.

**D12 · Record the evidence**
- [ ] [docs/26](docs/26-release-report.md) is updated with the live results.
- [ ] The test-docs report, HANDOFF.md and spec §1.2 (deployment done) are updated.

---

## Part 4 · Cleanup when testing is finished

- [ ] **Claude** (only when the owner says testing is finished): remove the local
  test database container `previa-member-ux-test-postgres`.
- [ ] **Decision (owner):** the two leftover `ordering_test_*` schemas in the
  development database. Claude has not touched them.

## Not in this release

These are deferred on purpose; nobody builds them as part of this list:
- Feature 4: review requests, assignments, decisions, review queue, activity
  timeline view (spec §7).
- Step E: projecting review status and counts to GitHub (§10.1).
- Paid subscriptions (§9.4, step J).
- The third reference journey (§8.3 item 2).
- Notification integrations (§10.4).
- Several workers sharing baseline storage (§8B.4).
- The ideas in [NEXT-FEATURES.md](NEXT-FEATURES.md).
