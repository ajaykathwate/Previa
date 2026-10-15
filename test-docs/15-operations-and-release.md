# 15 · Operations and release

**Module:** OPS · **Code:** `internal/platform/{config,database,queue,workers,githubapp,metrics}`,
`internal/app`, `deploy/`, `.github/workflows` · **Feature docs:**
[22 · Operating the MVP](../docs/22-mvp-operations.md),
[23 · Observability and hardening](../docs/23-observability-and-hardening.md),
[25 · Production deployment](../docs/25-production-deployment.md)

Configuration, the database and queue, the worker's capabilities, GitHub App
credentials, metrics and alerts, CI, the release images and the production stack.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Configuration | TC-OPS-CFG | 3 |
| Database and queue | TC-OPS-DB | 3 |
| Worker and GitHub App | TC-OPS-WRK | 3 |
| Observability | TC-OPS-OBS | 2 |
| Build, CI and release | TC-OPS-REL | 4 |
| Production host | TC-OPS-HOST | 1 |

## 1. Configuration (TC-OPS-CFG)

- [x] **TC-OPS-CFG-001 · Each process loads only the settings it needs**  
  Type: Positive · Priority: P1 · Level: Unit  
  Steps: Load the server, worker and shared configuration with required, optional and unrelated settings.  
  Expected: Defaults applied; required settings enforced per process; the worker never needs server settings and ignores unrelated ones.  
  Evidence: `internal/platform/config` › `TestLoadServer`; `internal/platform/config` › `TestLoadWorkerWithoutServerOrFutureSettings`; `internal/platform/config` › `TestRequiredSettingsByProcess`; `internal/platform/config` › `TestLoadSharedNeedsOnlyDatabase`; `internal/platform/config` › `TestLoadServerIgnoresFutureSettings`

- [x] **TC-OPS-CFG-002 · Invalid settings are refused with the setting named**  
  Type: Negative · Priority: P1 · Level: Unit  
  Steps: Load bad URLs, ports, durations, sizes, catalogs, outbound ports, trusted proxies, preview domains that overlap the dashboard, routing without a domain, and partial GitHub App settings.  
  Expected: Each is refused naming the setting; nothing partial is returned.  
  Evidence: `internal/platform/config` › `TestLoadServerRejectsInvalidEnvironment`; `internal/platform/config` › `TestLoadWorkerRejectsInvalidEnvironment`; `internal/platform/config` › `TestServiceCatalog`; `internal/platform/config` › `TestOutboundPorts`; `internal/platform/config` › `TestTrustedProxies`; `internal/platform/config` › `TestServerRefusesOverlappingPreviewDomain`; `internal/platform/config` › `TestWorkerRouterDirectoryNeedsAPreviewDomain`; `internal/platform/config` › `TestGitHubAppConfigurationByProcess`; `internal/platform/config` › `TestOperationalSettingsAreValidated`

- [x] **TC-OPS-CFG-003 · `.env.example` documents every setting with its true default**  
  Type: Positive · Priority: P2 · Level: Unit  
  Steps: Parse `.env.example`; load it with and without its optional lines.  
  Expected: Every line has a one-line label; optional values equal the defaults the loaders apply.  
  Evidence: `internal/platform/config` › `TestEnvExampleOptionalValuesAreTheDefaults`; `internal/platform/config` › `TestOperationalDefaults`; `internal/platform/config` › `TestExecutionSettingsDefaultsAndOverrides`

## 2. Database and queue (TC-OPS-DB)

- [x] **TC-OPS-DB-001 · Migrations enforce the data model and the schema gate holds**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Insert invalid rows against every constraint; compare the required schema version with the newest migration; roll back and reapply a migration in an isolated schema.  
  Expected: PostgreSQL refuses invalid data with the right error; the required version is the newest migration (20); rollback and reapply keep existing data.  
  Evidence: `internal/platform/database` › `TestMigrationConstraintsIntegration`; `internal/platform/database` › `TestSchemaVersionMatchesTheNewestMigration`; `internal/environments` › `TestIntentMigrationRollbackAndReapplyInIsolatedSchema`

- [x] **TC-OPS-DB-002 · The connection pool validates, cancels and health-checks**  
  Type: Negative · Priority: P2 · Level: Unit + Integration  
  Steps: Open pools with invalid settings and a cancelled context; health-check a nil, live and closed pool.  
  Expected: Invalid and cancelled opens fail; the health check passes only for a live pool.  
  Evidence: `internal/platform/database` › `TestNewPoolRejectsInvalidConfiguration`; `internal/platform/database` › `TestNewPoolHonorsCancellation`; `internal/platform/database` › `TestHealthCheckRejectsNilPool`; `internal/platform/database` › `TestPoolIntegration`

- [x] **TC-OPS-DB-003 · The queue is transactional, unique and bounded**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL, River)  
  Steps: Insert jobs inside a rolled-back transaction; insert the same key twice and across kinds; fill the worker slots; send an invalid ping; read queue stats.  
  Expected: Uncommitted jobs are invisible and rolled-back ones gone; active keys are unique and released on completion; concurrency never exceeds the configuration; invalid input is cancelled without retry; queue depth and job durations are measured.  
  Evidence: `internal/platform/queue` › `TestTransactionAndCrossKindUniquenessIntegration`; `internal/platform/queue` › `TestQueueConcurrencyIntegration`; `internal/platform/queue` › `TestPingRoundTripIntegration`; `internal/platform/queue` › `TestInvalidPingIsPermanent`; `internal/platform/queue` › `TestStatsReadsQueueDepthAndJobsAreMeasured`; `internal/platform/queue` › `TestJobOutcomes`

## 3. Worker and GitHub App (TC-OPS-WRK)

- [x] **TC-OPS-WRK-001 · The worker registers only what its dependencies allow**  
  Type: Positive · Priority: P1 · Level: Unit  
  Steps: Compose the worker without the App, without routing, and with routing but no Docker.  
  Expected: Missing dependencies leave capabilities off with a stated reason; no queue is registered for them; routing without Docker refuses to start.  
  Evidence: `internal/app` › `TestPipelineRegistersOnlyWhatItsDependenciesAllow`

- [x] **TC-OPS-WRK-002 · The server sees only live workers' capabilities**  
  Type: Positive · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Record heartbeats for two workers; let one go silent; stop one; pause one.  
  Expected: Capabilities come from live workers only; silent and stopped workers stop counting; a paused worker says so.  
  Evidence: `internal/platform/workers` › `TestCapabilitiesComeFromLiveWorkersOnly`; `internal/platform/workers` › `TestPausedWorkersSayWhyNothingBuilds`

- [x] **TC-OPS-WRK-003 · App tokens are minted once, cached, refreshed and isolated**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Call GitHub as two installations concurrently; approach token expiry; record rate-limit headers and transport errors.  
  Expected: A valid App JWT with the right issuer; exactly one token per installation even under concurrency; refresh before expiry; caches never shared; rate-limit headroom and errors measured.  
  Evidence: `internal/platform/githubapp` › `TestInstallationTokenJWTCacheRefreshAndIsolation`; `internal/platform/githubapp` › `TestTransportRecordsRateLimitHeadroom`

## 4. Observability (TC-OPS-OBS)

- [x] **TC-OPS-OBS-001 · Metrics keep bounded labels and a last good snapshot**  
  Type: Positive · Priority: P2 · Level: Unit  
  Steps: Record unknown events, outcomes, codes, methods and kinds; fail a state refresh; export egress counters.  
  Expected: Unknown values are mapped to a bounded set and a success carries no failure code; a failed refresh keeps the last snapshot and is counted; egress counters export only bounded labels.  
  Evidence: `internal/platform/metrics` › `TestLabelsStayBounded`; `internal/platform/metrics` › `TestStateKeepsTheLastGoodSnapshotAndRefreshesLazily`; `internal/platform/metrics` › `TestEgressCountersAreExportedWithBoundedLabels`

- [x] **TC-OPS-OBS-002 · Alert rules, their tests and dashboards are valid**  
  Type: Positive · Priority: P2 · Level: Static (promtool)  
  Steps: `make observability-check`.  
  Expected: The Prometheus configuration and alert rules pass `promtool`, the alert unit tests pass, and every Grafana query parses.  
  Evidence: `make observability-check`

## 5. Build, CI and release (TC-OPS-REL)

- [x] **TC-OPS-REL-001 · The code builds and passes static analysis**  
  Type: Positive · Priority: P1 · Level: Static  
  Steps: `go build ./...`, `make lint`.  
  Expected: Builds; `go vet` and golangci-lint (with the complexity and style rules) report 0 issues.  
  Evidence: `go build ./...`; `make lint`

- [x] **TC-OPS-REL-002 · The CI and release workflows are valid**  
  Type: Positive · Priority: P2 · Level: Static  
  Steps: Run actionlint on `.github/workflows/ci.yml` and `release.yml`.  
  Expected: No findings.  
  Evidence: actionlint 1.7.7

- [x] **TC-OPS-REL-003 · The release images build from pinned bases**  
  Type: Positive · Priority: P1 · Level: System  
  Steps: `make release-images` (server, worker and egress targets).  
  Expected: All three images build from the digest-pinned Go 1.26.9 and distroless bases.  
  Evidence: `make release-images`

- [x] **TC-OPS-REL-004 · The production stack passes its smoke test**  
  Type: Positive / Resilience · Priority: P1 · Level: System (production Compose)  
  Steps: `make prod-smoke` with the release images.  
  Expected: All checks pass: the stack starts healthy; migrations run once at schema 20; the router serves the dashboard over HTTPS behind a trusted proxy; readiness, metrics (including build info with the pinned Go version), Prometheus and Grafana work; containers are hardened; restarts and a graceful stop work; an upgrade and rollback by tag work; an encrypted backup and destructive restore succeed with the data-loss window shown. *This cycle's first run stopped at the build-info check, which still expected Go 1.26.8 after D-03 (defect D-04); the smoke test now reads the version from the Dockerfile's pin.*  
  Evidence: `make prod-smoke` (`deploy/production/smoke.sh`)

## 6. Production host (TC-OPS-HOST)

- [ ] **TC-OPS-HOST-001 · A real host, domain and hosted GitHub**  
  Type: Positive · Priority: P1 · Level: Acceptance  
  Steps: Follow [25 · Production deployment](../docs/25-production-deployment.md) on a real host with real domains, Let's Encrypt and a hosted GitHub App; back up to object storage and restore.  
  Expected: As documented in [26 · Release report](../docs/26-release-report.md).  
  **Not run on 2026-10-13:** needs a production host and object-storage backup destination. Local production Compose backup/restore passed; see the [current report](2026-10-13-regression-report.md#live-state-and-remaining-limits).
