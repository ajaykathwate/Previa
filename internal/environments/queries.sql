-- name: LockRequest :one
SELECT id, head_sha, state, generation FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: ReadRequest :one
SELECT id, head_sha, state, generation FROM pull_requests WHERE id = $1;

-- name: ReadEnvironment :one
SELECT container_id, image_tag, head_sha, status, applied_generation
FROM environments WHERE pull_request_id = $1 ORDER BY id DESC LIMIT 1;

-- name: ApplyResult :one
-- Lock the source-of-truth row, not only the environment. PostgreSQL rechecks
-- the generation predicate after waiting for a concurrent event's row lock.
-- Keep this lock until the result and its publication have committed.
WITH current_request AS MATERIALIZED (
    SELECT pr.id FROM pull_requests pr
    WHERE pr.id = sqlc.arg(pr_id) AND pr.generation = sqlc.arg(expected_generation)
    FOR UPDATE
), previous AS MATERIALIZED (
    SELECT e.* FROM environments e
    JOIN current_request p ON p.id = e.pull_request_id
    WHERE e.status <> 'torn_down'
    FOR UPDATE OF e
)
UPDATE environments e
SET status = sqlc.arg(result_status),
    applied_generation = sqlc.arg(expected_generation),
    container_id = CASE sqlc.arg(result_status)::text
        WHEN 'running' THEN sqlc.narg(container_id)::text
        WHEN 'torn_down' THEN NULL ELSE e.container_id END,
    image_tag = CASE sqlc.arg(result_status)::text
        WHEN 'running' THEN sqlc.narg(image_tag)::text
        WHEN 'torn_down' THEN NULL ELSE e.image_tag END,
    head_sha = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.arg(head_sha)::text ELSE e.head_sha END
FROM previous
WHERE e.id = previous.id
RETURNING previous.container_id, previous.image_tag;
