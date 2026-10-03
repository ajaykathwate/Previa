-- name: RecordPREvent :one
INSERT INTO pull_requests (repo_id, number, head_sha, state, generation)
VALUES ($1, $2, $3, $4, 1)
ON CONFLICT (repo_id, number) DO UPDATE
SET head_sha = EXCLUDED.head_sha,
    state = EXCLUDED.state,
    generation = pull_requests.generation + 1
RETURNING id, head_sha, state, generation;

-- name: FindEventRepository :one
SELECT installation_id FROM repos WHERE id = $1;

-- name: LockEventInstallation :one
SELECT id FROM installations WHERE id = $1 AND status = 'active' AND owner_user_id IS NOT NULL FOR UPDATE;

-- name: LockEventRepository :one
SELECT id FROM repos WHERE id = $1 AND installation_id = $2 AND accessible FOR UPDATE;
