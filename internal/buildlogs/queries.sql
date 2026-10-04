-- name: OwnedTarget :one
-- The PR, its lifetime environment and current build intent, scoped to the
-- installation owner. Retained PRs stay readable after access is revoked.
SELECT p.id AS pull_request_id, p.repo_id, p.number, p.title, p.head_sha, p.state AS pr_state,
       r.full_name, (r.accessible AND i.status = 'active') AS available,
       e.id AS environment_id, b.generation AS intent_generation, b.revision AS intent_revision,
       b.head_sha AS intent_head_sha, b.state AS intent_state, b.available_at AS intent_available_at
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
LEFT JOIN environments e ON e.pull_request_id = p.id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE r.id = $1 AND i.owner_user_id = $2 AND p.number = $3;

-- name: ReadStream :one
SELECT * FROM build_log_streams WHERE environment_id = $1;

-- name: OpenStream :one
SELECT next_sequence, stored_bytes, truncated FROM build_log_streams
WHERE environment_id = $1 AND epoch = $2 AND state = 'open';

-- name: AllocateLines :one
-- Fences the writer and reserves a contiguous sequence range in one statement.
-- A concurrent admission bumps epoch under this row lock, so a late writer from
-- an earlier attempt matches no row and stores nothing.
UPDATE build_log_streams SET next_sequence = next_sequence + sqlc.arg(lines)::bigint,
    stored_bytes = stored_bytes + sqlc.arg(bytes)::bigint,
    truncated = truncated OR sqlc.arg(truncated)::boolean,
    omitted_lines = omitted_lines + sqlc.arg(omitted)::bigint,
    lost_lines = lost_lines + sqlc.arg(lost)::bigint,
    updated_at = clock_timestamp()
WHERE environment_id = sqlc.arg(environment_id) AND epoch = sqlc.arg(epoch) AND state = 'open'
RETURNING (next_sequence - sqlc.arg(lines)::bigint)::bigint AS first_sequence;

-- name: InsertLines :exec
INSERT INTO build_logs (environment_id, epoch, sequence, kind, line)
SELECT sqlc.arg(environment_id)::bigint, sqlc.arg(epoch)::bigint, sqlc.arg(first_sequence)::bigint + k.ord - 1, k.kind, t.line
FROM unnest(sqlc.arg(kinds)::text[]) WITH ORDINALITY AS k(kind, ord)
JOIN unnest(sqlc.arg(texts)::text[]) WITH ORDINALITY AS t(line, ord) ON t.ord = k.ord;

-- name: FinishStream :execrows
UPDATE build_log_streams SET state = 'finished', finished_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE environment_id = $1 AND epoch = $2 AND state = 'open';

-- name: LinesFrom :many
SELECT sequence, kind, line FROM build_logs
WHERE environment_id = $1 AND epoch = $2 AND sequence >= $3
ORDER BY sequence LIMIT $4;

-- name: LinesBefore :many
SELECT sequence, kind, line FROM build_logs
WHERE environment_id = $1 AND epoch = $2 AND sequence < $3
ORDER BY sequence DESC LIMIT $4;

-- name: ExpiredStreams :many
SELECT environment_id, epoch FROM build_log_streams
WHERE pruned_at IS NULL AND coalesce(finished_at, opened_at) < sqlc.arg(cutoff)
ORDER BY environment_id LIMIT sqlc.arg(batch_size);

-- name: DeleteStreamLines :execrows
DELETE FROM build_logs d WHERE d.id IN (
    SELECT l.id FROM build_logs l
    WHERE l.environment_id = sqlc.arg(environment_id) AND l.epoch = sqlc.arg(epoch) LIMIT sqlc.arg(batch_size));

-- name: MarkStreamPruned :execrows
UPDATE build_log_streams SET pruned_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE environment_id = $1 AND epoch = $2 AND pruned_at IS NULL;
