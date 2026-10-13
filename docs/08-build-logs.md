# 08 · Build logs

Each pull request has one log: the output of its **current** build attempt, kept
in PostgreSQL, streamed live to its owner, and replaced when the next attempt is
admitted. This document covers how attempts are told apart, what is stored and
bounded, how the live page resumes without losing or repeating lines, and what
happens when storage fails. The preview runtime ([10](10-preview-runtime.md)) writes
build output from the image builder ([09](09-image-builds.md)) and then the app's
startup output, the latter through a second writer whose redactor also knows the
injected secrets. When a live preview's container later stops, the runtime adds its
last output to the same log. A running preview's output can also be watched live
(**App output**); that output is never stored beyond the watch. Nothing writes
sample output.

**App output** shows the running app's stdout/stderr, such as request logs and
runtime errors; **Build logs** show image construction and startup. App output
requires the worker's `app_output` queue. Followers deduplicate per watch epoch,
so a stale running follower can be replaced without accepting its late writes.
The App output page reports unavailable storage/queue errors explicitly.

PR comment links to Build logs preserve their full local URL through GitHub
sign-in, including query parameters. Signing in directly still opens Overview.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| History | **Current attempt only.** Admission of the next attempt deletes the previous lines in the same transaction | Previews are disposable; the useful log is the one for what is running or just failed. |
| Attempt identity | A per-environment **epoch** that increases on every admission, including a River retry of the same revision | Generation and revision alone repeat on a retry; the epoch never does, so stale writers and browser cursors are always detectable. |
| Limits (Balanced) | 5 MiB and 20,000 lines per attempt, 8 KiB per line, kept 14 days after the attempt finishes (`BUILD_LOG_RETENTION_DAYS`, 1–90) | Bounds database growth while keeping full logs for normal builds; a failed log's value fades within days (owner decision). |
| Deletion | The owner can delete a finished attempt's logs at any time | Tidying up, or a value that slipped past redaction. Backups keep deleted rows until they expire (`BACKUP_RETENTION_DAYS`). |
| Line times | Each line keeps the time the writer received it | Shown in the reader's time zone on the page; a UTC prefix in downloads. |
| Redaction | Before anything is stored or streamed, using [07](07-repository-secrets.md)'s streaming redactor | A value must never reach the database, the page or the stream. |
| Build impact | Writes never block a build; if storage falls behind, the excess is dropped, counted and marked | Logs are for diagnosis; they must not stall or fail the build they describe. |
| Live updates | Server-Sent Events with the log's own cursor; the realtime hub only wakes the stream ([20](20-realtime-updates.md)) | Lossless resume and replacement detection need log positions, not hub positions. |

## Storage and attempts

`build_log_streams` (migration 13) has one row per lifetime environment describing
the attempt stored now: `epoch`, generation, revision, commit, `state`
(`open`/`finished`), the next sequence number, stored bytes and three counters:
`truncated` with `omitted_lines` (caps reached) and `lost_lines` (storage failed).
`build_logs` rows carry `(environment_id, epoch, sequence, kind, line)`; `kind` is
`output` or `system` (Previa's own notices). Database checks bound a line to 16 KiB.

```
admission (environments.claimBuild, one transaction)
  claim intent ─► ResetBuildLogStream: epoch+1, counters reset, state open
               └► ClearBuildLogs: delete every earlier line
  returns Attempt{EnvironmentID, LogEpoch} ─► Runtime.Build(request, attempt)
rejected or stale job: nothing above happens; the old logs and epoch stay
```

- Taking the stream row lock before deleting lines means a concurrent late append
  either commits first (and is deleted) or finds the epoch changed and stores nothing.
- Admission provisions the PR's lifetime environment (with its hostname) in the same
  transaction when the repository has a hostname allocator
  ([12](12-routing-and-https.md)), so the first attempt already has a stream. Without
  an environment row an attempt gets `Attempt{}` and has no log.
- `environments.Service.WithBuildLogs` marks the stream finished when execution
  returns, so viewers know no more output is coming.

## Writing: `buildlogs.Writer`

`Store.Open(ctx, Binding{EnvironmentID, Epoch}, redactor, limits)` returns a writer
for an open attempt (`ErrSuperseded` otherwise, `ErrNoStream` without one). A later
writer for the same attempt (startup after the build) continues its sequence and caps.

Every byte goes through, in order:

1. **Control stripping**: ANSI colours, cursor moves, titles and other control
   characters are removed. This runs before redaction, so a value split by colour
   codes is still recognised. An unterminated escape string is abandoned after 4 KiB.
2. **Redaction** of injected values and platform credentials. The redactor holds
   back only a tail that could still become a known value, so a paused
   `Listening on :8080` appears at once.
3. **Line splitting**: LF and CRLF end lines; a lone CR keeps only the text after
   it, as a terminal shows a progress bar; invalid UTF-8 becomes U+FFFD; a line over
   8 KiB is cut with `… [line truncated: N more bytes not stored]`. A line that
   pauses without a newline for 1 second is stored as it is.
4. **Caps**: when the next line would pass 20,000 lines or 5 MiB, a system line
   `Log limit reached (5 MiB or 20,000 lines). Later output is not stored.` is stored,
   and later lines are counted in `omitted_lines`.
5. **Batching**: a background flusher stores up to 200 lines or 64 KiB per
   transaction, every 250 ms or sooner when a batch fills.

Each batch is one transaction: `AllocateLines` updates the stream row **only if its
epoch is still the writer's and it is open**, reserving a contiguous sequence range,
then the lines are inserted. A writer whose attempt was replaced stores nothing,
`Write` returns `ErrSuperseded`, and `Done()` closes so the producer can stop the
superseded work (the builder does).

**Storage failures.** The writer keeps up to 5,000 lines / 2 MiB waiting while the
database is slow or down, retrying with backoff up to 5 seconds. Beyond that, lines are
dropped and counted; when storage accepts lines again, a system line
`N lines could not be stored because log storage was unavailable or too slow.` marks
the gap and `lost_lines` records it. `Close` returns a `*LostLinesError`; it never fails
the build. If storage never returns before the build ends, the count is known only to
the build process (and its error), not to the page.

## Reading and live updates

The page is `/repositories/{id}/pull-requests/{number}/logs`, owner-only (the same
installation-owner scope as PR pages; retained PRs stay readable after access is
revoked, with a notice). It shows the newest 500 lines; without JavaScript, **Earlier
lines** / **Later lines** links page by sequence (`?before=`, `?after=`) and
**Refresh** reloads. Lines are rendered as text nodes: build output can never become
markup. Lines that look like errors are tinted; **Copy** copies the lines shown and
`…/logs/download` sends the attempt's whole output as `text/plain` (an attachment
named `<repository>-pr-<number>-build.log`, read in batches of 1,000 lines, owner-only,
`no-store`), each line prefixed with its UTC time (`2026-10-12T09:30:01.500Z`).
Every line shows its time (`HH:MM:SS`) in the reader's time zone; the full date is
its tooltip. Copy copies the text only. The PR page quotes the last twelve lines of a failed attempt through
`Store.Tail`, which returns nothing unless the stored stream belongs to that
request's generation and revision.

`…/logs/events` streams Server-Sent Events:

| Event | Data | Id |
| --- | --- | --- |
| `lines` | `{"epoch": E, "lines": [{"n": seq, "text": …, "system": true?, "at": RFC 3339}]}`, at most 200 per frame | `E.next` (next sequence) |
| `reset` | `{"epoch": E}`: a new attempt replaced the logs; clear the view | `E.0` |
| `state` | Status label, tone, summary, notices, commit, empty-state text | current cursor |

- The page starts the stream at `?since=E.next` from what it rendered; browsers
  resume with `Last-Event-ID`. A cursor from another epoch (or none) gets `reset`
  and a full replay of the current attempt, so an old cursor can never skip the
  start of a replacement build.
- Wake-ups: a trigger on `build_log_streams` raises `buildlog:<environment>` for
  every append, reset, finish and prune; repository events (intent and environment
  changes) also wake the stream. Each wake runs short queries; no connection or
  transaction is held per viewer.
- Bounds: 256 open log streams per server and 8 per user (`503` with `Retry-After`
  beyond), a 10-second write timeout per frame (a client that stops reading is
  dropped), heartbeats every 25 seconds, and a 5-minute lifetime after which the
  browser reconnects through authentication. Ownership is rechecked on repository
  events and at least every 30 seconds.
- The browser follows new output only while the reader is at the bottom, shows
  Live / Reconnecting / Paused / unavailable, and pauses the connection in hidden
  tabs. Status changes are announced (`role="status"`); individual lines are not.

States shown, all from recorded data: **No build requested**; **Waiting** (Automatic
quiet period, with the eligible time); **Queued**; **Running** (live output);
**Succeeded / Failed / Cancelled / Superseded**; a notice when the stored logs belong
to an earlier attempt than the current request; **Logs truncated** with the omitted
count; lost lines; **expired**; **Logs deleted**; and revoked access.

## Retention and deletion

The worker always registers `build_log_prune`, a River periodic job (hourly and at
start, leader only, one at a time). It deletes the lines of attempts that finished
more than `BUILD_LOG_RETENTION_DAYS` (default 14) days ago (or started then, if they
never finished) in batches of 5,000 and marks those streams `pruned_at`, so the page
says the logs expired instead of showing nothing. The server reads the same setting
for that sentence.

**Delete logs** (owner only, CSRF-protected, confirmed in a modal) removes a
finished attempt's lines at once: `POST …/logs/delete` with the attempt's `epoch`.
It refuses a running attempt and an attempt the owner did not see (a newer epoch).
An attempt whose worker crashed (its stream still open but no longer its PR's
running request) can be deleted too. Deleting finishes the stream, so a late writer
stores nothing, and marks it `pruned_at` and `deleted_at`: the page says **You
deleted this attempt's logs** and offers neither a download nor deletion. The next
attempt starts a clean stream. Data is fully gone once backups that hold it expire
(`BACKUP_RETENTION_DAYS`, 30 days by default).

Closed and merged pull requests, with their environment rows, logs, operations and
reports, are deleted 90 days after GitHub last updated them, once their teardown
finished ([15](15-housekeeping-and-capacity.md#retention-of-records)).

## After the attempt: a stopped live preview

When a container of a live preview (its app or a process) stops, the worker's crash
watch ([10](10-preview-runtime.md#stops-of-a-live-preview)) adds a section to the
finished log of the attempt that built it: why it stopped (exit code, out of
memory), whether Docker restarts it, and its last 200 lines (64 KiB), redacted as
the startup output was. It goes through the same writer, so the stream's caps still
apply, and it is fenced like any write: nothing is added to a newer attempt's,
expired or deleted logs.

## Live app output

`/repositories/{id}/pull-requests/{number}/output` (owner-only, linked as **App
output** on the pull request page while its preview runs) shows what the app prints,
live, including its own request logs: the place to look when a preview answers 500.

- Opening it records a watch (`app_output_watches`) and queues `app_output_follow`
  on a worker. The follower checks that the container is this preview's app
  (labels), follows `docker logs` from its last 200 lines with Docker's timestamps,
  strips control sequences, redacts what the startup log redacted (the container's
  `io.previa.redact-env` label names the secret variables; URL passwords too) and
  stores lines in batches every half second, keeping the newest 1,000.
- The page streams them like the build log (`lines`, `reset`, `state` events, ids
  `epoch.next`); its open stream renews the watch every 15 seconds. A minute after
  the last viewer leaves, the follower stops and deletes the lines; housekeeping
  removes stopped watches an hour later. A follower stops after two hours even when
  watched (reload to resume), and when the container stops or is replaced.
- At most 16 followers run per worker and 4 live views per user. A follower whose
  worker stopped is replaced on the viewer's next renewal.

## Code and tests

| Path | Role |
| --- | --- |
| `db/migrations/0013_build_log_streams.sql` | Streams, line epoch/kind, size check, realtime trigger. |
| `internal/environments/admission.go`, `scheduling_queries.sql` | Epoch reset and line clearing inside admission; `Attempt` for the runtime. |
| `internal/platform/textlines`, `internal/buildlogs/writer.go` | Control stripping, line bounds, caps, batching, fencing, loss accounting, line times. |
| `internal/buildlogs/store.go`, `queries.sql`, `prune.go` | Fenced appends, finished-stream sections, owner-scoped reads, finish, deletion, retention job. |
| `internal/appoutput` | Live app output: watches, the follower job, the page and its stream. |
| `internal/buildlogs/status.go`, `handler.go` | Status wording, page, download, live stream, stream caps. |
| `web/pages/builds/logs.*`, `web/static/js/ui.js` (`initBuildLogs`), `web/styles/logs.css` | Page, live client, styles. |

```sh
go test -count=1 -race ./internal/buildlogs                     # writer, lines, handler
TEST_DATABASE_URL=... go test -count=1 ./internal/buildlogs ./internal/environments
```

Covered: control sequences split across writes; CR/CRLF, long lines, invalid UTF-8;
redaction across writes and across colour codes; caps and markers; storage outages
with gap notices and lost counts; late writers fenced in a race with admission; a
retried revision getting a new epoch; rejected and rolled-back admissions keeping
logs; continuation across writers; finish and retention; the realtime trigger;
owner scoping; escaped script-looking output; no-JavaScript paging; status states;
resume without duplicates; reset for a replacement attempt; per-user stream caps;
and dropping a client that stops reading; the owner-only download of a whole
attempt; and `Tail` refusing another generation or revision. The browser suite covers
live append, following, reset and paging without JavaScript.
