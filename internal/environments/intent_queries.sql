-- name: FindIntentScope :one
SELECT pr.repo_id, r.installation_id
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id WHERE pr.id = $1;

-- name: LockIntentInstallation :one
SELECT id FROM installations
WHERE id = sqlc.arg(installation_id) AND owner_user_id = sqlc.arg(user_id)
    AND status = 'active'
FOR UPDATE;

-- name: LockIntentRepository :one
SELECT id, build_mode FROM repos
WHERE id = sqlc.arg(repo_id) AND installation_id = sqlc.arg(installation_id) AND accessible
FOR UPDATE;

-- name: LockIntentPR :one
SELECT id, repo_id, generation, head_sha, state FROM pull_requests
WHERE id = sqlc.arg(pr_id) AND repo_id = sqlc.arg(repo_id) FOR UPDATE;

-- name: LockBuildIntent :one
SELECT * FROM build_intents WHERE pull_request_id = $1 FOR UPDATE;

-- name: ReadOwnedIntentView :one
SELECT pr.id, pr.generation, pr.head_sha, pr.state AS pr_state, r.build_mode,
    b.revision, b.base_revision, b.generation AS requested_generation,
    b.head_sha AS requested_sha, b.source, b.request_action, b.requested_by,
    b.idempotency_key, b.state AS intent_state, b.available_at, b.requested_at,
    b.started_at, b.completed_at
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE pr.id = sqlc.arg(pr_id) AND i.owner_user_id = sqlc.arg(user_id)
    AND i.status = 'active' AND r.accessible;

-- name: ReplaceBuildIntent :one
INSERT INTO build_intents(pull_request_id, revision, base_revision, generation,
    head_sha, source, request_action, requested_by, idempotency_key, state, available_at)
SELECT pr.id, sqlc.arg(base_revision)::bigint + 1, sqlc.arg(base_revision)::bigint,
    pr.generation, pr.head_sha, sqlc.arg(source)::text, sqlc.arg(request_action)::text,
    sqlc.narg(requested_by)::bigint, sqlc.arg(idempotency_key)::text, 'pending',
    COALESCE(sqlc.narg(available_at)::timestamptz, clock_timestamp())
FROM pull_requests pr
WHERE pr.id = sqlc.arg(pr_id) AND pr.generation = sqlc.arg(generation)
    AND pr.head_sha = sqlc.arg(head_sha) AND pr.state = 'open'
ON CONFLICT (pull_request_id) DO UPDATE
SET revision = build_intents.revision + 1, base_revision = EXCLUDED.base_revision,
    generation = EXCLUDED.generation, head_sha = EXCLUDED.head_sha,
    source = EXCLUDED.source, request_action = EXCLUDED.request_action,
    requested_by = EXCLUDED.requested_by, idempotency_key = EXCLUDED.idempotency_key,
    state = 'pending', available_at = EXCLUDED.available_at,
    requested_at = clock_timestamp(), updated_at = clock_timestamp(),
    started_at = NULL, completed_at = NULL
WHERE build_intents.revision = sqlc.arg(base_revision)::bigint
RETURNING *;

-- name: SupersedeBuildIntent :execrows
UPDATE build_intents SET state = 'superseded', completed_at = clock_timestamp(),
    updated_at = clock_timestamp()
WHERE pull_request_id = $1 AND state IN ('pending', 'queued', 'running');

-- name: AdvanceBuildIntent :one
UPDATE build_intents b SET state = sqlc.arg(next_state), updated_at = clock_timestamp(),
    started_at = CASE WHEN sqlc.arg(next_state)::text = 'running' THEN clock_timestamp() ELSE b.started_at END,
    completed_at = CASE WHEN sqlc.arg(next_state)::text IN ('succeeded', 'failed') THEN clock_timestamp() ELSE NULL END
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND pr.state = 'open' AND pr.generation = sqlc.arg(generation)
    AND b.generation = pr.generation AND b.head_sha = pr.head_sha
    AND b.revision = sqlc.arg(revision) AND b.state = sqlc.arg(previous_state)
    AND (sqlc.arg(next_state)::text <> 'running' OR b.available_at <= clock_timestamp())
RETURNING b.*;

-- name: ReadBuildIntent :one
SELECT * FROM build_intents WHERE pull_request_id = $1;
