-- name: FindPROwner :one
SELECT w.owner_user_id FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id JOIN workspaces w ON w.id=i.workspace_id WHERE pr.id = $1;

-- name: LockSchedulingPR :one
SELECT id, generation, head_sha, state, teardown_completed_generation FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: QueueCurrentIntent :execrows
UPDATE build_intents SET state = 'queued', updated_at = clock_timestamp()
WHERE pull_request_id = $1 AND revision = $2 AND state = 'pending';

-- name: ClaimBuildIntent :execrows
-- Admission ends a wait for a build slot; a running request that waits for
-- preview capacity or disk keeps its note until a result is applied. An
-- automatic request never starts for a fork's code (see LockIntentPR).
UPDATE build_intents b SET state = 'running', started_at = COALESCE(started_at,clock_timestamp()), updated_at = clock_timestamp(),
    waiting_revision = CASE WHEN b.state IN ('pending','queued') THEN NULL ELSE b.waiting_revision END,
    waiting_reason = CASE WHEN b.state IN ('pending','queued') THEN NULL ELSE b.waiting_reason END,
    waiting_since = CASE WHEN b.state IN ('pending','queued') THEN NULL ELSE b.waiting_since END,
    waiting_checked_at = CASE WHEN b.state IN ('pending','queued') THEN NULL ELSE b.waiting_checked_at END
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND (b.source = 'manual' OR b.state = 'running'
         OR NOT (pr.is_fork OR pr.head_repo_id IS NULL OR pr.head_repo_id IS DISTINCT FROM r.github_repo_id))
    AND workspace_intent_authorized(pr.id) AND pr.state = 'open' AND pr.build_status='open' AND NOT pr.needs_refresh AND pr.generation = sqlc.arg(generation) AND pr.head_sha = sqlc.arg(head_sha)
    AND b.generation = pr.generation AND b.head_sha = pr.head_sha AND b.revision = sqlc.arg(revision)
    AND b.state IN ('pending','queued','running') AND b.available_at <= sqlc.arg(now_at)
    AND (b.source = 'manual' OR sqlc.arg(build_mode)::text = 'automatic' OR b.state = 'running');

-- name: ResetBuildLogStream :one
-- Opens the admitted attempt's log stream. epoch increases even when the same
-- revision is admitted again (a River retry), which fences writers and browser
-- cursors from the earlier attempt. Taking this row lock before deleting lines
-- makes a concurrent late append either commit first (and be deleted) or fail.
INSERT INTO build_log_streams (environment_id, epoch, generation, revision, head_sha)
SELECT e.id, 1, sqlc.arg(generation)::bigint, sqlc.arg(revision)::bigint, sqlc.arg(head_sha)::text
FROM environments e WHERE e.pull_request_id = sqlc.arg(pr_id)
ON CONFLICT (environment_id) DO UPDATE SET epoch = build_log_streams.epoch + 1,
    generation = EXCLUDED.generation, revision = EXCLUDED.revision, head_sha = EXCLUDED.head_sha,
    state = 'open', next_sequence = 0, stored_bytes = 0, truncated = false, omitted_lines = 0,
    lost_lines = 0, opened_at = clock_timestamp(), finished_at = NULL, pruned_at = NULL, deleted_at = NULL,
    updated_at = clock_timestamp()
RETURNING environment_id, epoch;

-- name: ClearBuildLogs :exec
DELETE FROM build_logs l USING environments e
WHERE l.environment_id = e.id AND e.pull_request_id = $1;

-- name: FinishBuildIntent :execrows
UPDATE build_intents b SET state = sqlc.arg(next_state), completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    waiting_revision = NULL, waiting_reason = NULL, waiting_since = NULL, waiting_checked_at = NULL
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND workspace_publication_authorized(pr.id) AND pr.state = 'open' AND pr.build_status='open' AND NOT pr.needs_refresh AND pr.generation = sqlc.arg(generation)
    AND b.generation = pr.generation AND b.revision = sqlc.arg(revision)
    AND b.state IN ('queued','running');

-- name: CompleteTeardownRequest :execrows
UPDATE pull_requests SET teardown_completed_generation = generation
WHERE id = $1 AND generation = $2
  AND (state IN ('closed','merged') OR teardown_requested_generation = generation);

-- name: ListSchedulingCandidates :many
SELECT pr.id FROM pull_requests pr LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE pr.id > sqlc.arg(after_id) AND ((pr.state = 'open' AND b.generation = pr.generation AND b.head_sha = pr.head_sha
        AND b.state IN ('pending','queued','running'))
   OR (pr.state IN ('closed','merged') AND pr.teardown_completed_generation < pr.generation)
) ORDER BY pr.id LIMIT sqlc.arg(batch_size);

-- name: LockBuildSlots :exec
-- Serializes build admission across workers, so the running count is exact.
-- Keyed by instance: advisory locks are shared by a database's schemas.
SELECT pg_advisory_xact_lock(hashtext('previa.build_slots.' || instance)) FROM platform_instance;

-- name: CountRunningBuilds :one
-- An admitted build holds its slot until its final attempt ends, including
-- between River retries; another attempt of the same PR's request reuses it.
SELECT count(*) FROM build_intents WHERE state = 'running' AND pull_request_id <> $1;

-- name: MarkIntentWaiting :execrows
-- Notes that the current request waits for capacity or disk space. Reports
-- follow only when the note changes. waiting_since keeps the start of the
-- wait while the same revision keeps waiting, whatever the reason.
UPDATE build_intents SET waiting_revision = sqlc.arg(revision)::bigint, waiting_reason = sqlc.arg(reason)::text, updated_at = clock_timestamp(),
    waiting_since = CASE WHEN waiting_revision = sqlc.arg(revision)::bigint AND waiting_since IS NOT NULL THEN waiting_since ELSE clock_timestamp() END,
    waiting_checked_at = clock_timestamp()
WHERE pull_request_id = sqlc.arg(pr_id) AND generation = sqlc.arg(generation)::bigint AND revision = sqlc.arg(revision)::bigint
  AND state IN ('pending', 'queued', 'running')
  AND (waiting_revision IS DISTINCT FROM sqlc.arg(revision)::bigint OR waiting_reason IS DISTINCT FROM sqlc.arg(reason)::text);

-- name: ClearIntentWaiting :execrows
UPDATE build_intents SET waiting_revision = NULL, waiting_reason = NULL, waiting_since = NULL, waiting_checked_at = NULL, updated_at = clock_timestamp()
WHERE pull_request_id = $1 AND revision = $2 AND waiting_revision IS NOT NULL;

-- name: TouchIntentWaiting :exec
-- A waiting request looked again: it still holds its turn.
UPDATE build_intents SET waiting_checked_at = clock_timestamp()
WHERE pull_request_id = sqlc.arg(pr_id) AND waiting_revision = sqlc.arg(revision)::bigint;

-- name: ReadIntentWait :one
SELECT state, waiting_since FROM build_intents WHERE pull_request_id = $1;

-- name: CountWorkspaceRunningBuilds :one
-- Running builds of other PRs in one stable workspace.
SELECT count(*) FROM build_intents b
JOIN pull_requests p ON p.id = b.pull_request_id
JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
WHERE b.state = 'running' AND b.pull_request_id <> sqlc.arg(pr_id) AND i.workspace_id = sqlc.arg(workspace_id)::bigint;

-- name: WorkspaceBuildSeconds :one
SELECT coalesce((SELECT build_seconds FROM workspace_build_usage WHERE workspace_id = sqlc.arg(workspace_id) AND day = sqlc.arg(day)), 0)::bigint;

-- name: OlderWaiterFromIdleWorkspace :one
-- Whether another account with nothing building has waited longer for a
-- build slot: then the last free slot is its turn. Only waits that looked
-- again since live_after count, so an abandoned wait never holds a turn.
SELECT EXISTS (
    SELECT 1 FROM build_intents b
    JOIN pull_requests p ON p.id = b.pull_request_id
    JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
    WHERE b.state IN ('pending', 'queued') AND b.waiting_reason = 'capacity' AND b.waiting_revision = b.revision
      AND b.waiting_since < sqlc.arg(since) AND b.waiting_checked_at > sqlc.arg(live_after)
      AND i.workspace_id IS DISTINCT FROM sqlc.arg(workspace_id)::bigint
      AND NOT EXISTS (
          SELECT 1 FROM build_intents rb
          JOIN pull_requests rp ON rp.id = rb.pull_request_id
          JOIN repos rr ON rr.id = rp.repo_id JOIN installations ri ON ri.id = rr.installation_id
          WHERE rb.state = 'running' AND ri.workspace_id = i.workspace_id)
)::boolean;

-- name: NextWaitingBuild :one
-- The waiting request whose turn is next when a slot frees: accounts with
-- nothing building first, then the longest wait.
SELECT b.pull_request_id FROM build_intents b
JOIN pull_requests p ON p.id = b.pull_request_id
JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
WHERE b.state IN ('pending', 'queued') AND b.waiting_reason IN ('capacity', 'account') AND b.waiting_revision = b.revision
  AND b.waiting_checked_at > sqlc.arg(live_after)
ORDER BY EXISTS (
    SELECT 1 FROM build_intents rb
    JOIN pull_requests rp ON rp.id = rb.pull_request_id
    JOIN repos rr ON rr.id = rp.repo_id JOIN installations ri ON ri.id = rr.installation_id
    WHERE rb.state = 'running' AND ri.workspace_id = i.workspace_id), b.waiting_since, b.pull_request_id
LIMIT 1;

-- name: RecordFinishedBuildTime :exec
-- Adds a finished build's time to its account's day (UTC).
INSERT INTO workspace_build_usage (workspace_id, day, build_seconds, builds)
SELECT b.workspace_id, (b.completed_at AT TIME ZONE 'UTC')::date,
       GREATEST(0, floor(extract(epoch FROM b.completed_at - b.started_at)))::bigint, 1
FROM build_intents b
JOIN pull_requests p ON p.id = b.pull_request_id
JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
WHERE b.pull_request_id = sqlc.arg(pr_id) AND b.revision = sqlc.arg(revision)
  AND b.completed_at IS NOT NULL AND b.started_at IS NOT NULL AND b.workspace_id IS NOT NULL
ON CONFLICT (workspace_id, day) DO UPDATE SET build_seconds = workspace_build_usage.build_seconds + EXCLUDED.build_seconds,
    builds = workspace_build_usage.builds + 1;

-- name: FindPRWorkspace :one
SELECT i.workspace_id FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id WHERE p.id=$1;
