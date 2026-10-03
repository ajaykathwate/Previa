-- name: FindPROwner :one
SELECT i.owner_user_id FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id WHERE pr.id = $1;

-- name: LockSchedulingPR :one
SELECT id, generation, head_sha, state, teardown_completed_generation FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: QueueCurrentIntent :execrows
UPDATE build_intents SET state = 'queued', updated_at = clock_timestamp()
WHERE pull_request_id = $1 AND revision = $2 AND state = 'pending';

-- name: ClaimBuildIntent :execrows
UPDATE build_intents b SET state = 'running', started_at = COALESCE(started_at,clock_timestamp()), updated_at = clock_timestamp()
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND pr.state = 'open' AND pr.generation = sqlc.arg(generation) AND pr.head_sha = sqlc.arg(head_sha)
    AND b.generation = pr.generation AND b.head_sha = pr.head_sha AND b.revision = sqlc.arg(revision)
    AND b.state IN ('pending','queued','running') AND b.available_at <= sqlc.arg(now_at)
    AND (b.source = 'manual' OR sqlc.arg(build_mode)::text = 'automatic' OR b.state = 'running');

-- name: ClearBuildLogs :exec
DELETE FROM build_logs l USING environments e
WHERE l.environment_id = e.id AND e.pull_request_id = $1;

-- name: FinishBuildIntent :execrows
UPDATE build_intents b SET state = sqlc.arg(next_state), completed_at = clock_timestamp(), updated_at = clock_timestamp()
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND pr.state = 'open' AND pr.generation = sqlc.arg(generation)
    AND b.generation = pr.generation AND b.revision = sqlc.arg(revision)
    AND b.state IN ('queued','running');

-- name: CompleteTeardownRequest :execrows
UPDATE pull_requests SET teardown_completed_generation = generation
WHERE id = $1 AND generation = $2 AND state IN ('closed','merged');

-- name: ListSchedulingCandidates :many
SELECT pr.id FROM pull_requests pr LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE pr.id > sqlc.arg(after_id) AND ((pr.state = 'open' AND b.generation = pr.generation AND b.head_sha = pr.head_sha
        AND b.state IN ('pending','queued','running'))
   OR (pr.state IN ('closed','merged') AND pr.teardown_completed_generation < pr.generation)
) ORDER BY pr.id LIMIT sqlc.arg(batch_size);
