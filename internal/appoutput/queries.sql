-- Live app output (db/migrations/0025_app_output.sql).

-- name: OwnedPreview :one
-- The owner's pull request and its preview; live when its app container runs.
SELECT p.id AS pull_request_id, p.repo_id, p.number, p.title, r.full_name,
       coalesce(e.id, 0)::bigint AS environment_id,
       coalesce(e.status = 'running' AND e.container_id IS NOT NULL, false)::boolean AS live,
       coalesce(e.head_sha, '')::text AS deployed_sha
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
LEFT JOIN environments e ON e.pull_request_id = p.id
WHERE r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'operate') AND p.number = sqlc.arg(number);

-- name: RequestWatch :one
-- A viewer opened the output: note it and ask for a follower unless one is
-- running and answering.
INSERT INTO app_output_watches AS w (environment_id) VALUES ($1)
ON CONFLICT (environment_id) DO UPDATE SET seen_at = clock_timestamp(),
    state = CASE WHEN w.state = 'following' AND w.updated_at >= clock_timestamp() - interval '45 seconds' THEN w.state ELSE 'requested' END,
    note = CASE WHEN w.state = 'following' AND w.updated_at >= clock_timestamp() - interval '45 seconds' THEN w.note ELSE '' END
RETURNING state;

-- name: TouchWatch :one
-- A viewer is still connected. A follower that stopped answering (its worker
-- stopped) is replaced.
UPDATE app_output_watches SET seen_at = clock_timestamp(),
    state = CASE WHEN state = 'following' AND updated_at < clock_timestamp() - interval '45 seconds' THEN 'requested' ELSE state END
WHERE environment_id = $1
RETURNING state;

-- name: ReadWatch :one
SELECT epoch, state, note, next_sequence FROM app_output_watches WHERE environment_id = $1;

-- name: FirstOutputSequence :one
SELECT coalesce(min(sequence), 0)::bigint FROM app_output_lines WHERE environment_id = $1 AND epoch = $2;

-- name: OutputLines :many
SELECT sequence, at, line FROM app_output_lines
WHERE environment_id = $1 AND epoch = $2 AND sequence >= $3
ORDER BY sequence LIMIT $4;

-- name: LiveContainer :one
SELECT container_id::text FROM environments WHERE id = $1 AND status = 'running' AND container_id IS NOT NULL;

-- name: BeginFollowing :one
UPDATE app_output_watches SET state = 'following', epoch = epoch + 1, next_sequence = 0, note = '', updated_at = clock_timestamp()
WHERE environment_id = $1 AND state = 'requested'
RETURNING epoch;

-- name: ClearOutputLines :exec
DELETE FROM app_output_lines WHERE environment_id = $1;

-- name: AllocateOutput :one
UPDATE app_output_watches SET next_sequence = next_sequence + sqlc.arg(lines)::bigint, updated_at = clock_timestamp()
WHERE environment_id = sqlc.arg(environment_id) AND epoch = sqlc.arg(epoch) AND state = 'following'
RETURNING (next_sequence - sqlc.arg(lines)::bigint)::bigint AS first_sequence;

-- name: InsertOutputLines :exec
INSERT INTO app_output_lines (environment_id, epoch, sequence, at, line)
SELECT sqlc.arg(environment_id)::bigint, sqlc.arg(epoch)::bigint, sqlc.arg(first_sequence)::bigint + t.ord - 1, a.at, t.line
FROM unnest(sqlc.arg(texts)::text[]) WITH ORDINALITY AS t(line, ord)
JOIN unnest(sqlc.arg(times)::timestamptz[]) WITH ORDINALITY AS a(at, ord) ON a.ord = t.ord;

-- name: TrimOutputLines :exec
-- Keeps the newest lines while watching.
DELETE FROM app_output_lines WHERE environment_id = $1 AND epoch = $2 AND sequence < $3;

-- name: FollowerHeartbeat :one
-- The follower is alive; reports whether anyone watched within a minute.
UPDATE app_output_watches SET updated_at = clock_timestamp()
WHERE environment_id = $1 AND epoch = $2 AND state = 'following'
RETURNING (seen_at > clock_timestamp() - interval '60 seconds')::boolean AS watched;

-- name: StopFollowing :exec
UPDATE app_output_watches SET state = 'stopped', note = sqlc.arg(note)::text, updated_at = clock_timestamp()
WHERE environment_id = sqlc.arg(environment_id) AND epoch = sqlc.arg(epoch) AND state IN ('requested', 'following');
