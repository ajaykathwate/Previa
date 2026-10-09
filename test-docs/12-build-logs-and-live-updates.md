# 12 · Build logs and live updates

**Module:** LOG · **Code:** `internal/buildlogs`, `internal/platform/realtime` ·
**Feature docs:** [08 · Build logs](../docs/08-build-logs.md),
[20 · Realtime updates](../docs/20-realtime-updates.md)

The current attempt's build and start-up output, stored redacted and bounded,
shown, paged, downloaded and streamed live; and the event hub that keeps every
open page current without polling.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Log page and download | TC-LOG-PAGE | 3 |
| Live log stream | TC-LOG-LIVE | 3 |
| Storage | TC-LOG-STO | 3 |
| Live page updates | TC-LOG-RT | 2 |

## 1. Log page and download (TC-LOG-PAGE)

- [x] **TC-LOG-PAGE-001 · Logs are owner-only and rendered as text**  
  Type: Security · Priority: P1 · Level: Integration + Browser  
  Steps: Open a log containing HTML and script; open another user's log and stream; open a missing PR.  
  Expected: Output is escaped text, never markup; other users and missing PRs get 404.  
  Evidence: `internal/buildlogs` › `TestLogPageIsOwnerScopedAndRendersOutputAsText`; browser › *build logs stream live, reset for a new attempt and render output as text*

- [x] **TC-LOG-PAGE-002 · The whole attempt downloads, for its owner only**  
  Type: Positive · Priority: P2 · Level: Integration  
  Steps: Download a log; download another user's; download with no stream.  
  Expected: Every line as a text attachment; others 404; no download link without output.  
  Evidence: `internal/buildlogs` › `TestLogDownloadSendsTheWholeAttemptToItsOwner`

- [x] **TC-LOG-PAGE-003 · Logs page without JavaScript and say what they are**  
  Type: Positive / Usability · Priority: P2 · Level: Integration + Browser  
  Steps: Open the tail, earlier and first pages without JavaScript; ask for an invalid position; open logs for each request state.  
  Expected: The tail shows the newest 500 lines with an earlier-lines link; older pages link forward; invalid positions get 400; each state's summary is accurate, and an earlier attempt's logs are labelled as such.  
  Evidence: `internal/buildlogs` › `TestLogPagePaginatesWithoutJavaScript`; `internal/buildlogs` › `TestLogPageStatesAreAccurate`; browser › *build logs paginate and stay readable without JavaScript*

## 2. Live log stream (TC-LOG-LIVE)

- [x] **TC-LOG-LIVE-001 · The stream resumes without duplicates and resets for a new attempt**  
  Type: Positive · Priority: P1 · Level: Integration + Browser  
  Steps: Follow a log, disconnect and resume with the last event ID; start a new attempt.  
  Expected: No line is repeated on resume; a new attempt sends a reset and starts from line one.  
  Evidence: `internal/buildlogs` › `TestLiveLogsResumeWithoutDuplicatesAndResetForANewAttempt`; browser › *build logs stream live, reset for a new attempt and render output as text*

- [x] **TC-LOG-LIVE-002 · Live streams are capped per user, and stalled readers are dropped**  
  Type: Security / Resilience · Priority: P2 · Level: Integration  
  Steps: Open a third stream for one user; close one; stop reading from a stream.  
  Expected: The third is refused with a clear message; a closed stream frees its slot; a client that stops reading is dropped.  
  Evidence: `internal/buildlogs` › `TestLiveLogsCapConnectionsPerUser`; `internal/buildlogs` › `TestLiveLogsDropClientsThatStopReading`

- [x] **TC-LOG-LIVE-003 · Server-sent events are framed safely and resume from the last ID**  
  Type: Security · Priority: P2 · Level: Unit  
  Steps: Send multi-line data and IDs containing newlines; resume a stream; close the hub.  
  Expected: Multi-line data is framed correctly and newlines are stripped from IDs; streams resume from `Last-Event-ID` and end when the hub closes.  
  Evidence: `internal/platform/realtime` › `TestSSEFramesMultilineDataAndStripsIDNewlines`; `internal/platform/realtime` › `TestStreamResumesFromLastEventID`; `internal/platform/realtime` › `TestStreamSendsReadyChangesAndEndsWithHub`

## 3. Storage (TC-LOG-STO)

- [x] **TC-LOG-STO-001 · Stored lines are redacted, sanitized and bounded**  
  Type: Security · Priority: P1 · Level: Unit + Integration  
  Steps: Write output with secrets, terminal escape sequences, an endless escape string, CRLF, progress bars, overlong lines and invalid UTF-8.  
  Expected: Secrets redacted; escape sequences stripped and endless ones abandoned; lines split and normalized; overlong lines cut with a marker; invalid UTF-8 replaced; lines stored in order.  
  Evidence: `internal/buildlogs` › `TestWriterStoresRedactedSanitizedLinesInOrder`; `internal/buildlogs` › `TestControlStripperRemovesTerminalSequences`; `internal/buildlogs` › `TestLineSplitterBoundsAndNormalizesLines`

- [x] **TC-LOG-STO-002 · Caps, outages and newer attempts are handled**  
  Type: Resilience · Priority: P1 · Level: Integration (PostgreSQL)  
  Steps: Exceed the size and line caps; fail storage mid-attempt; start a newer attempt while an old writer runs; admit attempts concurrently.  
  Expected: Truncation with a marker and counts; storage gaps marked and reported; the old writer stops when a newer attempt owns the stream; attempts never mix; a rejected build keeps the existing logs.  
  Evidence: `internal/buildlogs` › `TestWriterTruncatesAtCapsWithMarkerAndCounts`; `internal/buildlogs` › `TestWriterRecoversFromStorageFailuresAndMarksGaps`; `internal/buildlogs` › `TestWriterStopsWhenANewerAttemptOwnsTheStream`; `internal/buildlogs` › `TestStoreConcurrentAdmissionNeverMixesAttempts`; `internal/environments` › `TestRejectedBuildPreservesLogs`

- [x] **TC-LOG-STO-003 · Retention deletes only expired attempts**  
  Type: Positive · Priority: P3 · Level: Integration (PostgreSQL)  
  Steps: Run the prune job with current, recent and expired attempts.  
  Expected: Only attempts past retention are deleted.  
  Evidence: `internal/buildlogs` › `TestPruneDeletesOnlyExpiredAttempts`

## 4. Live page updates (TC-LOG-RT)

- [x] **TC-LOG-RT-001 · Database changes reach open pages without polling**  
  Type: Positive · Priority: P2 · Level: Integration (PostgreSQL) + Browser  
  Steps: Change PRs, intents and environments while two tabs are open.  
  Expected: Triggers notify for each committed change (not for no-ops or rollbacks); the other tab updates without polling.  
  Evidence: `internal/platform/realtime` › `TestTriggersNotifyListenerIntegration`; browser › *live updates push changes across tabs without polling*

- [x] **TC-LOG-RT-002 · The hub routes, coalesces, replays and resyncs correctly**  
  Type: Edge · Priority: P2 · Level: Unit  
  Steps: Publish bursts on several topics; subscribe from a cursor; fall behind the history; close the hub; decode foreign payloads.  
  Expected: Events reach only their topic, bursts coalesce to the newest, catch-up replays from the cursor, gaps beyond history resync, closing delivers pending events, foreign payloads are ignored.  
  Evidence: `internal/platform/realtime` › `TestHubRoutesByTopicAndCoalescesBursts`; `internal/platform/realtime` › `TestHubSubscribeReplaysFromCursor`; `internal/platform/realtime` › `TestHubResyncsWhenHistoryCannotCoverGapOrMarksResync`; `internal/platform/realtime` › `TestHubResyncIsNeverDowngraded`; `internal/platform/realtime` › `TestHubCloseEndsSubscriptions`; `internal/platform/realtime` › `TestDecodeIgnoresForeignPayloads`; `internal/platform/realtime` › `TestCursorRoundTrip`
