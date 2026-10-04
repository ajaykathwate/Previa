-- name: RecordPREvent :one
INSERT INTO pull_requests (repo_id, number, head_sha, state, generation,mergeable,mergeable_state,build_status)
SELECT sqlc.arg(repo_id)::bigint, sqlc.arg(number)::integer, sqlc.arg(head_sha)::text, sqlc.arg(state)::text, 1,sqlc.narg(mergeable)::boolean,sqlc.arg(mergeable_state)::text,sqlc.arg(build_status)::text
WHERE sqlc.arg(state)::text='open' OR EXISTS(SELECT 1 FROM pull_requests WHERE repo_id=sqlc.arg(repo_id) AND number=sqlc.arg(number))
ON CONFLICT (repo_id, number) DO UPDATE
SET head_sha = EXCLUDED.head_sha,
    state = EXCLUDED.state,
    generation = pull_requests.generation + 1,mergeable=EXCLUDED.mergeable,mergeable_state=EXCLUDED.mergeable_state,build_status=EXCLUDED.build_status
RETURNING id, head_sha, state, generation;

-- name: FindEventRepository :one
SELECT installation_id FROM repos WHERE id = $1;

-- name: LockEventInstallation :one
SELECT id FROM installations WHERE id = $1 AND status = 'active' AND owner_user_id IS NOT NULL FOR UPDATE;

-- name: LockEventRepository :one
SELECT id FROM repos WHERE id = $1 AND installation_id = $2 AND accessible FOR UPDATE;
