# 12 · Routing and HTTPS

`internal/routing` gives every pull request a stable public HTTPS hostname and
publishes healthy candidates from the preview runtime ([10](10-preview-runtime.md))
through Traefik v3. This document explains the hostname scheme, why routes come from
a fenced controller instead of Docker labels, how a version becomes served, what is
guaranteed and what is not, TLS, origin separation and the unknown-host page. The
worker runs the controller whenever it can build ([22](22-mvp-operations.md)); the
tests listed at the end prove it, and `make test-e2e` proves it inside the whole
pipeline.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| Route source | **Traefik's file provider fed by Previa's route controller**, not the Docker provider (owner decision; DESIGN §11 updated) | Docker labels are fixed when a container is created, so every candidate would be routed before it was accepted, and labels cannot be fenced by (generation, revision). The file provider also means Traefik needs no Docker socket. |
| Fence | PostgreSQL row per lifetime environment, moved only forward in (generation, revision) order; removals are kept as tombstones | The durable publication decision ([19](19-generation-ordering.md#the-decision)): one logical controller, accepted version stored durably, conditional on the PR's current state. |
| Hostname | `pr-<number>-<GitHub repo ID, base 36>-<8 random chars>.<PREVIEW_BASE_DOMAIN>`, allocated once per lifetime environment (owner decision) | The repository ID is immutable (renames and same-named repositories never collide); the random part keeps private repositories' previews unguessable, which "public by URL obscurity" needs. |
| Certificates | One wildcard certificate per preview domain, Let's Encrypt **staging by default**, DNS-01 through Traefik's ACME client (lego) | No per-preview issuance or rate limits; staging first proves DNS credentials without risking production limits. |
| TLS proof here | Local Pebble/DNS regression tests and live Let's Encrypt issuance for the development wildcard | Pebble keeps automated tests isolated; the live development router also verifies normal browser/client trust with a production certificate. |
| Unknown and removed hosts | A redirect to the dashboard's public `/previews/unavailable` page (owner decision) | Nothing new to run, and the dashboard never answers on untrusted preview hostnames. |

## Hostnames

Admission provisions the PR's lifetime environment on its first admitted build
(`environments.Repository.WithHostnames(routing.NewLabel)`), in the same transaction
that claims the build and opens its log stream. The label is allocated there, once:
rebuilds, failures, teardown and reopening keep it, `environments.subdomain` is
unique, and environment rows are never deleted, so a hostname is never reassigned to
another PR. Labels are DNS-safe by construction and by a database check
(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, at most 36 characters in practice). A
repository without a GitHub ID cannot be provisioned. `expires_at` is now nullable:
expiry is scheduled by housekeeping (Prompt 9).

## The route controller

```
activate_route operation (outbox)                    remove_route operation
  1. runtime.Endpoint: the candidate is this PR's     1. AcceptRouteRemoval: tombstone
     preview of exactly this generation, revision        at (generation, 0) unless a newer
     and image; running; ready right now (5 probes)      version (a reopened PR) exists
  2. router.Attach: Traefik joins previa-pr-<id>
  3. AcceptRoute (one transaction, PR scope locked):
     PR open at this generation and commit, intent
     at this revision running/succeeded, access
     intact, candidate is the pending artifact,
     route version strictly older → move forward
  4. publish: render the whole accepted table; write  2. publish (same)
     it only if its version is newer than the file's
  5. await: Traefik's API shows the router with this  3. await: the router is gone
     version's service (preview-<env>-g<g>-r<r>)
  6. MarkRouteServed                                  4. MarkRouteServed
then the operation worker records the outcome: activation sets the environment
running and schedules the previous artifacts' cleanup; removal finishes the teardown
([13](13-teardown-and-cleanup.md)).
```

- **Fence.** `environment_routes` holds one row per lifetime environment: the
  accepted `(generation, revision)`, state `active` or `removed`, the target
  (container, image, backend name, port, network) and the served version.
  Acceptance takes the usual lock order (installation, repository, PR), then the
  environment, the route row and a single `route_table` counter, whose row lock
  serializes acceptances in commit order. Every acceptance bumps the counter; a
  snapshot's version therefore covers every acceptance committed before it.
- **Materialization.** Traefik's configuration is a view of the accepted table, not
  the source of truth. The controller renders the full table (JSON, which Traefik's
  YAML reader accepts) into `previa-routes.yml` in `ROUTER_DYNAMIC_DIR`, with the
  version in its first line. Writers on the host serialize on a lock file and replace
  the file atomically (temporary file, fsync, rename), and only when their version is
  newer, so a late or concurrent writer can never move the file backwards. Explicit
  startup/recovery syncs may refresh changed worker settings at the same version;
  identical content is left untouched, and an older table is still refused. Version
  0 (nothing accepted yet) also carries the fallback router.
- **Confirmation.** Traefik's API listens on `127.0.0.1:8080` inside the router
  container only. The worker, which alone holds the Docker socket, reads one router
  with `docker exec <router> wget …/api/http/routers/preview-<env>@file`. A version
  is served when that router names the version's service (removal: when the router is
  gone). A newer accepted version ends the wait as superseded.
- **Protection.** Cleanup keeps the accepted target, the last served target, and
  every target accepted since the last confirmation (`unconfirmed_targets`): the
  router may have loaded any of them. A confirmation clears that list, because the
  file can no longer go back to them.
- **Recovery.** All state is in PostgreSQL. A retried operation re-verifies the
  candidate, finds its version already accepted (idempotent), rewrites the file if it
  is behind and confirms. `Controller.Sync` rewrites a missing or stale file, or
  changed settings at the same version, at startup and from the reconciler
  (`Reconciler.WithRoutes`). It also restores the router's preview-network
  attachments from the accepted routes, including when the file version is
  unchanged. After a restart of the same Traefik container, routes come back from
  the file without Previa running. Recreating the container loses its dynamic
  Docker attachments; worker startup or the one-minute recovery pass restores them.
- **Failure classes.** A missing, foreign, stopped or unready candidate fails
  permanently: the activation fails, the environment keeps its active preview and
  becomes `failed`, the candidate is cleaned up and a failure report is queued.
  Router, Docker, file and database errors and a convergence timeout (30 s) are
  retryable. Route removal is retried until it succeeds ([13](13-teardown-and-cleanup.md)).
  Missing private-preview auth configuration fails permanently with
  `private_preview_unconfigured` before accepting the candidate, while the
  controller refreshes existing routes to deny access; retry the build after the
  operator configures the workers. River retries arriving before the
  durable backoff deadline or another attempt's lease expiry snooze without
  consuming an attempt or recording a failure.

### What is guaranteed, and the limits

Guaranteed, by the PostgreSQL fence and the forward-only file:

- An older generation or revision never replaces a newer accepted route, and a late
  activation never resurrects a removed one. A removal never removes a newer route
  (a reopened PR's).
- Only candidates that passed the pre-publication check, and that the PR still wants,
  are ever written.
- The router's view only moves forward: it may lag, but it never returns to an
  older route table, including across controller and Traefik restarts.
- Nothing the router serves or may still serve is removed by cleanup, and old
  artifacts retire only after a newer version is confirmed.

Not guaranteed (stated plainly):

- **Per-request freshness.** Between acceptance and Traefik's reload (about a second:
  file watch plus a 250 ms throttle) requests still reach the previous target, which
  stays protected. Traefik's file provider has no compare-and-set; the fence lives in
  PostgreSQL and in the file's version, not on the traffic path.
- **Database and router agree only after convergence.** If newer work supersedes an
  activation after its route was accepted, the router serves that candidate while the
  environment's active artifact is still the previous one, until the next version
  converges. Both stay protected.
- **Availability during outages.** If Traefik is down, nothing is served; if the
  file is deleted, Traefik drops every route (unknown hosts get Traefik's plain 404)
  until `Sync` rewrites it.
- **One host.** Writers coordinate with a host-local file lock; workers that write
  routes must share the router's filesystem.

## Network path and Docker access

- Traefik joins each PR's `previa-pr-<id>` network (attached by the worker at
  activation, removed with the network at teardown) and dials the candidate by
  container name through Docker's DNS on that network. The control plane never joins
  preview networks.
- Traefik has **no Docker socket and no Docker provider** (REQUIREMENTS.md's "only the worker
  touches the socket" holds). The worker attaches networks and reads the API through
  the socket it already holds; no socket proxy is needed.
- Previews can reach the router's public entry points on their network, and through
  them other previews' public URLs, exactly as anyone on the internet can (previews
  are public by URL). They cannot reach its API (loopback only), and the
  container-to-container path between previews stays blocked.

## TLS and DNS

- **DNS:** a wildcard record `*.PREVIEW_BASE_DOMAIN` pointing at the host.
- **Certificate:** the `websecure` entry point declares the resolver and the single
  wildcard domain, so all preview routers share one certificate obtained over DNS-01.
  Routers must not carry their own `tls` block: it would replace the entry point's
  resolver (found while verifying).
- **Staging first:** `ACME_CA_SERVER` defaults to Let's Encrypt staging. Its
  certificates are intentionally untrusted by browsers; Chrome shows
  `NET::ERR_CERT_AUTHORITY_INVALID`, and HSTS can prevent continuing. Check staging
  issuance, then switch the local router to production using the settings below.
  Select separate certificate storage rather than deleting the persistent volume.
- **Protected credentials:** the DNS provider token is a Docker secret file
  (`ACME_DNS_CREDENTIAL_FILE`, `chmod 600`, outside Git) that lego reads through
  `<VARIABLE>_FILE`; the container environment holds only the path.
- **ACME storage:** `acme.json` lives on the persistent `acme` volume with mode 600, so
  restarts reuse the certificate (verified: same serial after a restart).
- **HTTP** port 80 redirects to HTTPS; `websecure` deletes alias headers
  (`aliasHeadersStrategy=delete`).

For trusted HTTPS with `deploy/traefik/compose.yml`, set these in `.env`:

```sh
ACME_CA_SERVER=https://acme-v02.api.letsencrypt.org/directory
ACME_STORAGE_FILE=/acme/acme-production.json
```

Run `ENV_FILE=.env make router-up`. The local router's `ACME_STORAGE_FILE` is an
absolute path inside its persistent `/acme` volume; the default remains
`/acme/acme.json`. A separate file lets Traefik create a production account and
certificate while preserving staging state. Reusing a staging file can keep
serving its untrusted certificate. Keep the chosen production path on subsequent
starts so certificate renewal reuses the same account and keys. This setting is
for the local/staging Compose file; the production stack has separate preview and
dashboard storage paths.

Changing certificate settings recreates the router. `make router-up` restores
its existing preview network attachments so already-running previews keep working.
Use that target instead of directly recreating the container with Compose.
After issuance, `curl -I https://<preview-host>/` must verify the certificate
without `-k`; the response may be the app or the private preview sign-in redirect.
No app rebuild is needed for a certificate change.

## Origins and cookies

- The dashboard (`APP_BASE_URL`) must not be the preview domain or below it; the
  server and worker refuse that configuration. A separate registrable domain (as
  `githubusercontent.com` is for `github.com`) isolates previews fully; the server
  warns when they share one.
- Over HTTPS the session and OAuth state cookies are `__Host-` prefixed (host-only,
  `Path=/`, Secure), so a preview on a sibling subdomain cannot plant them. The CSRF
  token lives in the server-side session.
- Unknown and removed preview hosts are redirected (302, no cookies) to
  `/previews/unavailable` on the dashboard: a public 404 page in the signal theme that
  reads no session, sets no cookie, is never cached and says nothing about which host
  existed.
- A sleeping preview's host is routed (302) to `/previews/wake/<host>/<path>` on the
  dashboard ([10](10-preview-runtime.md#idle-previews-sleep)). That page is public as
  well, reads no session, and sends visitors back only to the host of a known preview,
  so it is no open redirect.

## Private previews

An owner can switch on **Private previews** for a repository (repository page,
confirmed in a modal). Its previews are then open only to people who sign in to
Previa with GitHub and can see the repository on GitHub. Changing it advances the
route table and queues `route_sync`, which a worker turns into the router's file
within seconds.

1. The router puts Traefik's `forwardAuth` in front of each private preview: before
   every request it asks the server at `PREVIEW_ACCESS_URL` (`/previews/auth`), which
   checks a cookie and current PostgreSQL authorization for that host. A valid current grant answers 200 and the request goes
   through.
2. Without one, the visitor is redirected to `/previews/access` on the dashboard. It
   signs them in (GitHub sign-in returns them there) and asks GitHub, with the App's
   installation token, for their permission on the repository only when they are
   outsiders. Active workspace Owners/Admins and explicitly granted members get
   preview access locally, including Reviewers without private-source permission.
   Removed local members cannot fall back to outsider access.
3. The visitor returns to `https://<host>/__previa/access?t=<token>`. The token is
   signed (HMAC with a key derived from `SESSION_SECRET`), names the host and the
   user, and is valid for one minute. The check turns it into the
   `__Host-previa-access` cookie (host-only, Secure, HttpOnly, SameSite=Lax, at most 12 hours)
   and redirects to the path they first asked for.

Notes:

- A cookie opens only the preview host it was issued for. Losing repository access
  takes effect when the source-only outsider cookie expires (12 hours at most); local member/grant
  revocation is checked on every request, including token redemption. Workspace
  authorization revisions also invalidate existing cookies. F4 also checks the
  user authorization epoch and a digest of the issuing dashboard session on every
  request: logout, idle expiry, End other sessions and account deletion revoke
  these grants. The raw dashboard session token is never sent to a preview.
- On a public repository everyone signed in to GitHub can see it, so private
  previews mostly matter for private repositories.
- Without the GitHub App on the server, only locally authorized workspace members
  get in; outsider GitHub permission cannot be verified.
- The check sees the host, path and signed cookie and reads current grants from
  PostgreSQL; it stores no per-preview session.
- Enabling private previews requires all live routing workers to advertise a
  configured `PREVIEW_ACCESS_URL` (migration 0029). The UI explains missing setup
  and disables enabling; POST enforces the same guard. Paused routing workers count,
  discovery-only and stale workers do not. A configured URL still needs to be
  reachable from Traefik; this capability is not a health probe.
- With `PREVIEW_ACCESS_URL` unset, an existing private preview fails closed: its host is sent
  to the unavailable page. Production's Compose file sets it to the server on the
  control network; see [02](02-development-setup.md) for one machine.
- The app also receives the cookie. It is signed and valid only for this preview's
  host, so it lets nobody into anything else.

## Configuration and operation

| Variable | Process | Default | Meaning |
| --- | --- | --- | --- |
| `PREVIEW_BASE_DOMAIN` | server, worker | empty (routing off) | Preview hostnames live below it. |
| `APP_BASE_URL` | server; worker when routing | — | Dashboard origin; the worker builds the unavailable page URL from it. |
| `ROUTER_DYNAMIC_DIR` | worker | — | Directory shared with the router (Linux filesystem, not `/mnt/c`). |
| `ROUTER_CONTAINER` | worker | `previa-router` | Router container to attach and query. |
| `ROUTER_ENTRYPOINT` | worker | `websecure` | Traefik entry point with the wildcard certificate. |
| `ROUTER_API_ADDRESS` | worker | `127.0.0.1:8080` | Traefik API inside the router container (must be loopback). |
| `ROUTER_METRICS_ADDRESS` | worker | the API address | Traefik's Prometheus metrics inside the router container, for preview visit counts. |
| `PREVIEW_ACCESS_URL` | worker | empty | The server's `/previews/auth` as the router reaches it; empty makes private previews serve nobody. |
| `ACME_EMAIL`, `ACME_DNS_PROVIDER`, `ACME_DNS_CREDENTIAL_VARIABLE`, `ACME_DNS_CREDENTIAL_FILE`, `ACME_CA_SERVER`, `ACME_DNS_RESOLVERS` | router (compose) | staging CA | ACME account, lego DNS provider and its credential file. |
| `ACME_STORAGE_FILE` | local/staging router (compose) | `/acme/acme.json` | Certificate storage inside the persistent `/acme` volume; use a separate file when switching from staging to production. |

```sh
make route-sync     # write the accepted route table from PostgreSQL (cmd/route-sync)
make router-up      # checks the settings, runs route-sync, starts deploy/traefik/compose.yml
make router-down
make test-routing   # real HTTPS routing, swaps, fences, restarts, teardown, revocation (~5 min)
```

Traefik requests the wildcard certificate only once some router uses the HTTPS entry
point; the table's fallback router does, which is why `router-up` writes the table
first. The worker validates routing settings at start, starts the egress proxy and writes
the accepted route table to the route file before it accepts work. Without a real
domain and DNS token the router cannot obtain a certificate; use `make test-routing`
or `make test-e2e` (Traefik's default certificate) on a machine without one.

## Verified

`make test-routing` runs against a real Traefik v3.7.13 whose wildcard certificate a
local Pebble issues over DNS-01, real hardened previews, the real outbox and
PostgreSQL (a throwaway schema). Images come from a fast commit-based builder (real
builds are covered by `make test-builder`). It verified:

| Scenario | Result |
| --- | --- |
| HTTPS with the Pebble wildcard certificate, verified against Pebble's root | served |
| Unknown host | 302 to the unavailable page, no cookies |
| Plain HTTP | permanent redirect to HTTPS |
| New commit | swapped behind the same hostname; old container and image removed |
| Candidate stops before publication | never routed; old preview kept; environment `failed`; candidate removed; failure reported |
| Late activation of an older generation (straight to the controller) | refused by the fence; file not rewritten |
| Same-generation Rebuild, then the older revision replayed | refused by the fence |
| Close, reopen, late removal of the pre-close route | refused; the reopened PR's route kept; pre-close artifacts removed |
| Close while an activation waits for confirmation | removal wins; activation superseded; everything removed |
| Controller crash after acceptance | restarted controller rewrites the file; the operation resumes and finishes |
| Traefik restart | routes reloaded from the file; the certificate from `acme.json` (same serial), no new issuance |
| Routes file deleted | routes dropped (404) until `Sync` rewrites it |
| Private preview without auth URL, then configured at the same route-table version | explicit missing-configuration failure; deny route refreshed; anonymous visitor refused, fixture-authorized visitor served |

Traefik renews at start any certificate it thinks is near expiry, judged by
`certificatesduration` (default 90 days, matching Let's Encrypt). Pebble issues
six-day certificates, so the test router sets 144 hours; a CA with other lifetimes
needs the matching value, or every router restart re-issues.

Unit tests cover hostnames, rendering (including that routers carry no TLS block and
the fallback rule only matches subdomains), the forward-only file under concurrency,
the controller's failure classes with fakes, routing configuration and origin checks,
`__Host-` cookies and the unavailable page. PostgreSQL tests cover the fence
(newer-only, idempotent retries, tombstones, reopen, same-generation revisions),
protection of accepted, served and unconfirmed targets, failed activations and
provisioning with hostname collisions.

**Live proof (2026-10-13):** inspected the existing Let's Encrypt staging certificate
for `*.dev.previa.live`, then issued a trusted production certificate over Cloudflare
DNS-01 using separate storage. Ordinary `curl` and OpenSSL verified its chain and
hostname. The private preview returned its expected sign-in redirect after fixing
the WSL server's IPv4 listener; existing preview network attachments survived the
router recreation. Both certificate files retained mode 600.

**Not verified here:** multi-host workers.

## Code and tests

| Path | Role |
| --- | --- |
| `internal/routing/controller.go` | The route controller: verify, attach, accept, publish, confirm; `Sync`. |
| `cmd/route-sync`, `internal/app/route_sync.go` | Maintenance command that writes the accepted table (used by `make router-up`). |
| `internal/routing/render.go`, `file.go`, `lock_*.go` | Traefik configuration, forward-only file, host lock. |
| `internal/routing/traefik.go` | Network attachment and the loopback API through Docker. |
| `internal/routing/hostname.go`, `unavailable.go`, `web/pages/routing` | Labels; the unavailable page. |
| `internal/environments/routes.go`, `route_queries.sql` | The fence: accept, remove, served, table snapshot, protection. |
| `internal/runtime/endpoint.go` | Pre-publication candidate check. |
| `deploy/traefik/compose.yml` | Router deployment (staging CA default, secret file, loopback API). |
| `db/migrations/0014_routes_and_teardown.sql` | Route rows, table counter, teardown requests, hostname check. |


### Missed access-revocation deliveries (F5)

App-authenticated installation reconciliation runs every five minutes and after
a durable token-mint failure signal. Confirmed deletion/suspension/repository
removal uses the existing transactional webhook revocation and cleanup path.
Outages, wrong credentials, incomplete listings and circuits never authorize
revocation. Polling never restores access or starts builds; verified same-workspace
reinstall can recover missed deletion. See [35](35-installation-recovery-and-early-access.md).

## Google reviewers (F7)

When configured, private-preview authentication offers both login methods and
returns to the requested host/path. Google reviewers use existing local workspace
membership and repository grants, including session-bound cookie and removal
checks. A Google-only outsider never receives the GitHub-reader fallback based on
a display name or a URL. [36](36-google-sign-in-and-reviewer-invitations.md).
