# 14 · Security and ingress

**Module:** SECU · **Code:** `internal/platform/httpx`, `internal/app` (assembled
server), `internal/platform/logging`, `internal/platform/egress` · **Feature docs:**
[23 · Observability and hardening](../docs/23-observability-and-hardening.md)

Cross-cutting protections of the dashboard server as it runs in production
(headers, hosts, body limits, rate and concurrency limits, what is and is not
exposed), the egress proxy that is every preview's and build's only way out,
and secret hygiene in logs and dependencies. Module-specific security cases live
in their modules; this module tests the edges every request passes through.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Response hardening | TC-SECU-HDR | 2 |
| Request limits | TC-SECU-LIM | 4 |
| Exposure | TC-SECU-EXP | 3 |
| Data handling | TC-SECU-DATA | 3 |
| Egress proxy | TC-SECU-EGR | 2 |
| Dependencies | TC-SECU-DEP | 1 |

## 1. Response hardening (TC-SECU-HDR)

- [x] **TC-SECU-HDR-001 · Every page carries strict security headers and a fresh CSP nonce**  
  Type: Security · Priority: P1 · Level: Unit + Integration (assembled server)  
  Steps: Load the home page twice from the production configuration; load it over plain HTTP in development.  
  Expected: `X-Frame-Options: DENY`, `nosniff`, `strict-origin-when-cross-origin`, `Cross-Origin-Opener-Policy: same-origin`, a Permissions-Policy and HSTS; the CSP has `default-src 'self'`, `object-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'` and a nonce that differs per response; no HSTS over plain HTTP.  
  Evidence: `internal/app` › `TestServerRoutesRegression/public_pages_carry_the_security_headers_and_a_fresh_nonce`; `internal/platform/httpx` › `TestSecurityHeadersAndNonce`

- [x] **TC-SECU-HDR-002 · No page violates the Content-Security-Policy**  
  Type: Security · Priority: P1 · Level: Browser  
  Steps: Run all 43 browser scenarios under the production CSP.  
  Expected: No CSP violation and no JavaScript error on any page (any one fails its scenario).  
  Evidence: browser suite (every scenario's `afterEach` check); `web` › `TestBrowserRegression`

## 2. Request limits (TC-SECU-LIM)

- [x] **TC-SECU-LIM-001 · Requests for another host are refused**  
  Type: Security · Priority: P1 · Level: Unit + Integration (assembled server)  
  Steps: Request with foreign hosts, a preview hostname and a lookalike suffix; send `X-Forwarded-Host` from an untrusted client; forward from trusted and untrusted proxies.  
  Expected: 421 for every foreign host; an untrusted forwarded host changes nothing; client addresses come from forwarded headers only when sent by a trusted proxy.  
  Evidence: `internal/app` › `TestServerRoutesRegression/requests_for_another_host_are_refused`; `internal/platform/httpx` › `TestHostValidation`; `internal/platform/httpx` › `TestClientIPTrustsForwardedHeadersOnlyFromProxies`

- [x] **TC-SECU-LIM-002 · Oversized bodies are refused before they are read**  
  Type: Security · Priority: P1 · Level: Unit + Integration (assembled server)  
  Steps: Post a form over 256 KiB with a declared length; stream an oversized body; post to an exempt path.  
  Expected: 413 for the declared one, a read error for the streamed one; the exempt webhook path has its own 2 MiB cap.  
  Evidence: `internal/app` › `TestServerRoutesRegression/oversized_form_bodies_are_refused_before_parsing`; `internal/platform/httpx` › `TestBodyLimit`

- [x] **TC-SECU-LIM-003 · Rate and concurrency limits protect sign-in and webhooks**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Burst past the per-client rate; track many clients; exceed the concurrent-webhook bound.  
  Expected: 429 with `Retry-After` past the burst, other clients unaffected; the limiter's memory stays bounded; excess concurrent webhooks get 503 before anything is recorded.  
  Evidence: `internal/platform/httpx` › `TestRateLimitPerClient`; `internal/platform/httpx` › `TestRateLimiterStaysBounded`; `internal/platform/httpx` › `TestLimitConcurrency`

- [x] **TC-SECU-LIM-004 · A burst of 200 concurrent page loads is served cleanly**  
  Type: Performance / Concurrency · Priority: P2 · Level: Integration (assembled server, `-race`)  
  Steps: Load the home page 200 times concurrently from many client addresses.  
  Expected: Every response is 200; no data race.  
  Evidence: `internal/app` › `TestServerRoutesRegression/a_burst_of_concurrent_page_loads_is_served_without_errors`

## 3. Exposure (TC-SECU-EXP)

- [x] **TC-SECU-EXP-001 · Only built assets are served; source and secrets never are**  
  Type: Security · Priority: P1 · Level: Unit + Integration (assembled server)  
  Steps: Request the stylesheet, script and favicon; then `../go.mod`, an encoded traversal, a Go source file, `/.env` and `/.git/config`.  
  Expected: Assets with correct types; everything else 404 and never module or source content.  
  Evidence: `internal/app` › `TestServerRoutesRegression/static_assets_are_served_and_nothing_else_leaks`; `internal/app` › `TestServerRoutesRegression/operational_endpoints_and_unknown_routes_are_not_public`; `web` › `TestEmbeddedBrowserAssets`; `web` › `TestBrandAssetsAndFavicon`

- [x] **TC-SECU-EXP-002 · Health, readiness and metrics are only on the internal listener**  
  Type: Security · Priority: P1 · Level: Unit + Integration (assembled server)  
  Steps: Request `/metrics`, `/livez`, `/readyz` and `/debug/pprof/` on the public server; request them on the internal listener with a healthy and a failing check.  
  Expected: 404 publicly; on the internal listener livez is up, readyz reports each check and fails when one fails, metrics are served.  
  Evidence: `internal/app` › `TestServerRoutesRegression/operational_endpoints_and_unknown_routes_are_not_public`; `internal/platform/httpx` › `TestInternalEndpoints`; `internal/platform/httpx` › `TestProbe`

- [x] **TC-SECU-EXP-003 · Unknown routes are 404 and wrong methods 405; panics are contained**  
  Type: Negative · Priority: P3 · Level: Unit + Integration  
  Steps: Request an unknown path; send DELETE to sign-in; make a handler panic.  
  Expected: 404, 405, and a 500 that exposes no panic details, logged with the request ID, without crashing the server.  
  Evidence: `internal/app` › `TestServerRoutesRegression/operational_endpoints_and_unknown_routes_are_not_public`; `internal/platform/httpx` › `TestRouterLogsRequestAndRecoversPanic`

## 4. Data handling (TC-SECU-DATA)

- [x] **TC-SECU-DATA-001 · Hostile search and stored GitHub data stay inert**  
  Type: Security · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Store PRs with script, SQL, `%`, `_` and Unicode in titles, authors and branches; search with `' OR 1=1 --`, `'; DROP TABLE…`, `%`, `_`, markup and mixed case; search as another user.  
  Expected: Every search matches literally (no SQL injection, no LIKE wildcards); the table is intact; Unicode matches case-insensitively; another user gets "unavailable".  
  Evidence: `internal/pullrequests` › `TestHostileSearchAndTitlesAreInertData`

- [x] **TC-SECU-DATA-002 · Logs never contain secrets or credentials**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Log fields named like secrets, values shaped like tokens, and errors containing URLs with credentials.  
  Expected: All redacted precisely; ordinary fields kept; correlation fields come from the context; unknown levels refused.  
  Evidence: `internal/platform/logging` › `TestSecretKeysAndShapesAreRedacted`; `internal/platform/logging` › `TestCorrelationFieldsComeFromTheContext`; `internal/platform/logging` › `TestLevels`

- [x] **TC-SECU-DATA-003 · Configuration fails safe and never echoes credentials**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Load production settings without HTTPS, without a webhook secret, with world-readable keys; load invalid database URLs and keyrings.  
  Expected: Each is refused; errors never contain the database password or key material; partial configuration is never returned.  
  Evidence: `internal/platform/config` › `TestProductionRequiresHTTPSWebhookSecretAndPrivateKeys`; `internal/platform/config` › `TestLoadDoesNotExposeDatabaseCredentials`; `internal/platform/config` › `TestLoadServerRejectsInvalidEnvironment`; `internal/platform/githubapp` › `TestPrivateKeyPermissions`

## 5. Egress proxy (TC-SECU-EGR)

- [x] **TC-SECU-EGR-001 · The egress proxy reaches only vetted public addresses**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Classify 34 addresses: private and Docker ranges, loopback, unspecified, cloud metadata, link-local, CGNAT, documentation, benchmarking, multicast, broadcast, reserved, IPv4-mapped and IPv6 translation and tunnelling ranges; proxy and tunnel to public and internal targets.  
  Expected: Every internal range is excluded; only public destinations are reached, hop-by-hop headers are stripped, and the proxy dials only the address it vetted.  
  Evidence: `internal/platform/egress` › `TestPublicAddressExcludesEveryInternalRange`; `internal/platform/egress` › `TestEgressProxyReachesOnlyVettedPublicDestinations`

- [x] **TC-SECU-EGR-002 · Readiness probes go only where they should**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Probe a healthy, redirecting, unhealthy and closed target and bad targets or paths.  
  Expected: Correct readiness answers; redirects are not followed; bad targets and paths are refused.  
  Evidence: `internal/platform/egress` › `TestProbeHTTPAndTCPReadiness`

## 6. Dependencies (TC-SECU-DEP)

- [x] **TC-SECU-DEP-001 · No reachable known vulnerability in the code or its modules**  
  Type: Security · Priority: P1 · Level: Static  
  Steps: `make vuln` (govulncheck on every package).  
  Expected: "Your code is affected by 0 vulnerabilities." *This cycle first found 13 new advisories in Go 1.26.8's standard library and `golang.org/x/net` v0.58.0 (GO-2026-6599 to GO-2026-6617); defect D-03 raised the toolchain to Go 1.26.9 and `x/net` to v0.60.0, and the rerun is clean.*  
  Evidence: `make vuln`
