-- name: RecordPREvent :one
INSERT INTO pull_requests (repo_id, number, head_sha, state, generation)
VALUES ($1, $2, $3, $4, 1)
ON CONFLICT (repo_id, number) DO UPDATE
SET head_sha = EXCLUDED.head_sha,
    state = EXCLUDED.state,
    generation = pull_requests.generation + 1
RETURNING id, head_sha, state, generation;
