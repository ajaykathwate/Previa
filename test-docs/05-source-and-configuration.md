# 05 · Source and configuration

**Module:** SRC · **Code:** `internal/source`, `internal/repoconfig` · **Feature docs:**
[06 · Source and configuration](../docs/06-source-and-configuration.md)

Downloading the exact commit, unpacking an untrusted archive safely, and reading
`.previewrc.yaml`. Everything here handles attacker-controlled input from a pull
request.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Fetching the commit | TC-SRC-FET | 4 |
| Unpacking the archive | TC-SRC-EXT | 3 |
| `.previewrc.yaml` | TC-SRC-CFG | 7 |

## 1. Fetching the commit (TC-SRC-FET)

- [x] **TC-SRC-FET-001 · The exact commit is fetched and credentials stay with GitHub's API**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Prepare a commit through a fake GitHub API and a separate archive host.  
  Expected: The archive is for the exact SHA; installation credentials are never sent to the archive host, which keeps its own signed URL.  
  Evidence: `internal/source` › `TestArchivesFetchExactCommitWithoutForwardingCredentials`

- [x] **TC-SRC-FET-002 · Unsafe archive locations and redirect loops are refused**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Have GitHub point at a disallowed host, a plain-HTTP URL and a URL with credentials; have the archive host redirect elsewhere or loop, return 404 or drop the connection.  
  Expected: Unsafe locations and redirects are refused (`redirect_rejected`) and disallowed hosts are never contacted; redirects are bounded; a missing archive and a dropped connection get their own codes; errors never contain the archive URL.  
  Evidence: `internal/source` › `TestArchivesRejectUnsafeLocationsAndRedirects`

- [x] **TC-SRC-FET-003 · API failures are classified as retryable or not**  
  Type: Negative · Priority: P2 · Level: Unit  
  Steps: Return 401, 403, 404, 422 and 500 from the archive API; call it without installation authentication.  
  Expected: 401/403 mean access denied, 404/422 unavailable, 500 an API error, each with the right retryability; the call without installation auth is refused.  
  Evidence: `internal/source` › `TestArchivesClassifyAPIFailures`

- [x] **TC-SRC-FET-004 · Only canonical SHAs are fetched; size and cancellation are bounded**  
  Type: Negative · Priority: P1 · Level: Unit  
  Steps: Prepare a branch name, an uppercase SHA, a declared-oversize archive, and cancel mid-download.  
  Expected: Branch names are never fetched; SHAs must be canonical lowercase; oversize archives are refused; cancellation is reported as such.  
  Evidence: `internal/source` › `TestPrepareCancellationAndRequestValidation`

## 2. Unpacking the archive (TC-SRC-EXT)

- [x] **TC-SRC-EXT-001 · The verified tree is unpacked with safe permissions**  
  Type: Positive · Priority: P1 · Level: Unit  
  Steps: Prepare an archive with executables, a setuid file and an in-tree symlink.  
  Expected: Contents extracted; the executable bit kept, setuid dropped; the in-tree symlink recreated; the workspace is listed with its identity.  
  Evidence: `internal/source` › `TestPrepareExtractsVerifiedTreeAndConfig`

- [x] **TC-SRC-EXT-002 · Hostile archives are refused**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Feed archives with: no or a different commit declaration; `../`, nested `../`, non-canonical and absolute paths; a second top-level directory; absolute and escaping symlinks, a write through a symlink, a symlink over a directory; a hard link, a character device and a FIFO; duplicate and file-then-directory entries; too many entries; content over the extracted and compressed caps; a compression bomb; over-long paths and names; an empty, non-gzip and truncated archive.  
  Expected: Each is refused with its category (identity mismatch, unsafe, too large, invalid); nothing is written outside the workspace; failed preparations leave no workspace behind.  
  Evidence: `internal/source` › `TestPrepareRejectsHostileArchives`

- [x] **TC-SRC-EXT-003 · Crash leftovers are found and removed safely**  
  Type: Resilience · Priority: P2 · Level: Unit  
  Steps: Leave a workspace behind; list and remove it; try removing a path that is not a workspace or is outside the root; use a relative or symlinked root.  
  Expected: The leftover is listed and removed; foreign paths are refused; the root must be absolute and not a symlink.  
  Evidence: `internal/source` › `TestWorkspacesListAndRemoveCrashLeftovers`

## 3. `.previewrc.yaml` (TC-SRC-CFG)

- [x] **TC-SRC-CFG-001 · Valid configurations load with defaults applied**  
  Type: Positive · Priority: P1 · Level: Unit  
  Steps: Parse the fixture configurations, including an empty one.  
  Expected: Explicit values are kept and normalized; approved defaults are applied.  
  Evidence: `internal/repoconfig` › `TestConfigFixtures`

- [x] **TC-SRC-CFG-002 · Types are strict and ambiguous YAML is refused**  
  Type: Negative · Priority: P1 · Level: Unit  
  Steps: Use quoted, fractional, boolean and null ports; non-string Dockerfile and env values; custom tags, anchors and aliases; merge keys and complex keys; empty, comment-only and multi-document files; broken YAML and duplicate keys (25 inputs).  
  Expected: Each is refused with a field and code; nothing is coerced.  
  Evidence: `internal/repoconfig` › `TestStrictTypesAndDocuments`

- [x] **TC-SRC-CFG-003 · Validation never echoes the file and has a size limit**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Parse oversized and invalid files containing secret-looking text, including invalid backend sections.  
  Expected: Structured errors without the source text; issues aggregated per field; size limit enforced.  
  Evidence: `internal/repoconfig` › `TestSafeValidationAndSizeLimit`; `internal/repoconfig` › `TestBackendMessagesNeverEchoSource`

- [x] **TC-SRC-CFG-004 · Unsafe paths, ports and environment entries are refused**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Use absolute and `..` Dockerfile paths, invalid health paths, out-of-range ports and malformed `NAME=value` entries.  
  Expected: Unsafe values refused; safe relative paths accepted.  
  Evidence: `internal/repoconfig` › `TestPathsPortsAndEnvironmentValidation`

- [x] **TC-SRC-CFG-005 · Resource tiers are clamped to the platform's ceilings**  
  Type: Edge · Priority: P2 · Level: Unit  
  Steps: Ask for tiers above the ceilings, for the app and for processes; configure invalid ceilings.  
  Expected: Clamped with a notice; invalid ceilings refused.  
  Evidence: `internal/repoconfig` › `TestResourceTiersAndCeilings`; `internal/repoconfig` › `TestProcessTiersAreClamped`

- [x] **TC-SRC-CFG-006 · The effective hash changes only when the runtime would**  
  Type: Edge · Priority: P2 · Level: Unit  
  Steps: Reformat, reorder environment entries and services, respell defaults; then change a runtime setting or re-pin an image.  
  Expected: Same hash for equivalent files; a different hash for any runtime-affecting change.  
  Evidence: `internal/repoconfig` › `TestEffectiveConfigHash`; `internal/repoconfig` › `TestBackendConfig`

- [x] **TC-SRC-CFG-007 · Backend services, setup and processes are checked against the catalog**  
  Type: Positive / Negative · Priority: P1 · Level: Unit  
  Steps: Declare PostgreSQL and Redis versions in and out of the catalog, too many processes, duplicate names, and a setup command; read the Dockerfile and config from the same tree.  
  Expected: Catalog versions resolve to pinned images; unknown kinds or versions fail preparation with a clear message; config and Dockerfile come from the same verified tree.  
  Evidence: `internal/repoconfig` › `TestBackendConfig`; `internal/repoconfig` › `TestBackendConfigProblems`; `internal/source` › `TestPrepareConfigAndDockerfileFromTheSameTree`
