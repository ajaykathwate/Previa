-- Idle previews sleep and wake (db/migrations/0026_preview_sleep.sql).

-- name: ListIdlePreviews :many
-- Live previews with no visit, publication or wake since idle_before, and
-- no build of their pull request on the way.
SELECT e.id FROM environments e
JOIN pull_requests p ON p.id = e.pull_request_id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE e.status = 'running' AND e.container_id IS NOT NULL AND e.asleep_at IS NULL AND e.pending_container_id IS NULL
  AND p.state = 'open' AND p.teardown_requested_generation <> p.generation
  AND greatest(e.last_active_at, coalesce(e.last_visited_at, e.last_active_at), coalesce(e.woke_at, e.last_active_at)) < sqlc.arg(idle_before)
  AND NOT coalesce(b.state IN ('pending', 'queued', 'running'), false)
ORDER BY e.id LIMIT sqlc.arg(batch_size);

-- name: LockPreview :exec
-- Serializes sleeping and waking one preview across workers. Keyed by
-- instance: advisory locks are shared by a database's schemas.
SELECT pg_advisory_xact_lock(hashtext('previa.idle.' || instance || '.' || sqlc.arg(environment_id)::bigint::text)) FROM platform_instance;

-- name: LockIdlePreview :one
-- ListIdlePreviews' conditions again, under the environment's row lock.
SELECT e.id, e.pull_request_id, e.applied_generation, e.applied_revision FROM environments e
JOIN pull_requests p ON p.id = e.pull_request_id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE e.id = sqlc.arg(environment_id)
  AND e.status = 'running' AND e.container_id IS NOT NULL AND e.asleep_at IS NULL AND e.pending_container_id IS NULL
  AND p.state = 'open' AND p.teardown_requested_generation <> p.generation
  AND greatest(e.last_active_at, coalesce(e.last_visited_at, e.last_active_at), coalesce(e.woke_at, e.last_active_at)) < sqlc.arg(idle_before)
  AND NOT coalesce(b.state IN ('pending', 'queued', 'running'), false)
FOR UPDATE OF e;

-- name: MarkAsleep :exec
UPDATE environments SET asleep_at = clock_timestamp(), wake_requested_at = NULL, wake_error = '' WHERE id = $1;

-- name: AdvanceRouteTable :one
-- A sleep or wake changes how the router serves the host.
UPDATE route_table SET version = version + 1 WHERE singleton RETURNING version;

-- name: LockSleeping :one
SELECT e.id, e.pull_request_id, e.applied_generation, e.applied_revision FROM environments e
WHERE e.id = $1 AND e.asleep_at IS NOT NULL AND e.status = 'running'
FOR UPDATE;

-- name: MarkAwake :execrows
UPDATE environments SET asleep_at = NULL, woke_at = clock_timestamp(), wake_requested_at = NULL, wake_error = ''
WHERE id = $1 AND asleep_at IS NOT NULL;

-- name: FailWake :exec
UPDATE environments SET wake_requested_at = NULL, wake_error = sqlc.arg(note)::text
WHERE id = sqlc.arg(environment_id) AND asleep_at IS NOT NULL;

-- name: PreviewByHost :one
-- The public wake page's preview, by its DNS label.
SELECT e.id, e.status, e.asleep_at, e.woke_at, e.wake_requested_at, e.wake_error, p.state AS pr_state
FROM environments e JOIN pull_requests p ON p.id = e.pull_request_id
WHERE e.subdomain = $1;

-- name: RequestWake :execrows
UPDATE environments SET wake_requested_at = clock_timestamp(), wake_error = ''
WHERE id = $1 AND asleep_at IS NOT NULL;
