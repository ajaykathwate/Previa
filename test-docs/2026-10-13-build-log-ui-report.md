# Build-log UI regression · 2026-10-13

Scope: copyable inline dates, source colors and filters, literal text search,
PR recent-log panel, full-page navigation, live output, and App output compatibility.
This follows the [complete application regression](2026-10-13-regression-report.md).

## Changes verified

- Previa notices use the existing `system` kind; Docker build and application
  output use `output`. Both appear by default. Filters run before database paging
  and export; search treats `%` and `_` literally and ignores case.
- Each entry has one inline date/time prefix. Leading application timestamps
  retain their original format; otherwise received UTC time is shown. Copy and
  download include dates; indentation and escaped text remain intact.
- PR log links open a panel under the main status card with the latest 300
  matching entries and a full-page link. Live frames trim the panel to 300.
  Closing releases SSE; reopening reloads fresh output. PR invalidation swaps
  preserve the open panel and applied source/search filters.
- The full page retains 500-entry sequence pagination. Sparse matching sequences
  keep correct previous/next navigation and live cursors. Filtered download reads
  the whole attempt in bounded batches, including more than 1,000 matches.
- Ownership checks apply to recent/full pages, events and downloads; invalid
  sources, oversized searches and NUL searches return 400. The writer, stored
  events, retention, epoch fences and SSE protocol retain their existing behavior.
- App output retains live connection/status updates, copying and stream resets
  while using the shared timestamp rendering. Its backend follower tests pass.

## Verification

| Check | Result |
| --- | --- |
| `go build ./...` | Passed on final code |
| `go vet ./...` | Passed on final code |
| `go tool -modfile=tools/go.mod golangci-lint run` | Passed, 0 issues |
| `TEST_DATABASE_URL=… go test -race -count=1 ./...` | All standard suites passed |
| Final focused race tests: buildlogs, appoutput, logviewer | Passed, including isolated PostgreSQL search/follower tests |
| Complete Chromium UI regression | Passed, 55/55 on final code |
| JavaScript syntax and `git diff --check` | Passed |

An intermediate run passed 54/55: the existing failed-PR-search case timed out.
Its isolated rerun passed; the test now waits for the initial live page refetch
to settle before intercepting its search response. The final complete rerun
passed 55/55 (195 seconds).

Browser coverage includes login return URLs, text-only rendering under the real
CSP, source colors, copying full dates, source/search filters, filtered full-page
links, 300-entry live bounds, close/reopen cleanup, PR DOM refresh preservation,
load-failure messaging, attempt reset, build tracker updates, pagination, and
JavaScript-free PR navigation/filtering. Responsive checks cover 320, 390, 768
and 1440 px. Screenshots are ignored artifacts in `web/tests/browser/artifacts/`.

No new configuration or migration is required. Rebuild/restart the server (or
let Air rebuild) and refresh the browser for the new templates/assets. No change
conflicts with README.md or DESIGN.md. This UI cycle does not rerun opt-in live
GitHub, real Docker, release-image or production-host checks; their preceding
results and remaining production acceptance gaps are in the linked report.
