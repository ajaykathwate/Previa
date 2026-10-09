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

-- name: RecordDelivery :execrows
INSERT INTO webhook_deliveries (delivery_id, event_type, payload_hash)
VALUES (sqlc.arg(delivery_id), sqlc.arg(event_type), sqlc.arg(payload_hash))
ON CONFLICT DO NOTHING;

-- name: DeliveryMatches :one
SELECT (event_type = sqlc.arg(event_type)::text AND payload_hash = sqlc.arg(payload_hash)::text)::boolean AS matches
FROM webhook_deliveries WHERE delivery_id = sqlc.arg(delivery_id);

-- name: EnsureInstallation :exec
INSERT INTO installations (github_installation_id, status) VALUES ($1, 'active') ON CONFLICT DO NOTHING;

-- name: LockInstallationSourceTime :one
SELECT source_updated_at FROM installations WHERE github_installation_id = $1 FOR UPDATE;

-- name: RecordRepositoryEvent :execrows
-- The installation timestamp is not a selection sequence, so each repository
-- keeps its own: a newer event wins, and a removal wins a same-time re-add.
INSERT INTO installation_repository_events (installation_id, github_repo_id, source_updated_at, removed)
VALUES (sqlc.arg(installation_id), sqlc.arg(github_repo_id), sqlc.arg(source_updated_at), sqlc.arg(removed))
ON CONFLICT (installation_id, github_repo_id) DO UPDATE
SET source_updated_at = EXCLUDED.source_updated_at, removed = EXCLUDED.removed
WHERE installation_repository_events.source_updated_at < EXCLUDED.source_updated_at
   OR (installation_repository_events.source_updated_at = EXCLUDED.source_updated_at AND EXCLUDED.removed);

-- name: FinishInstallationEvent :exec
UPDATE installations
SET source_updated_at = GREATEST(source_updated_at, sqlc.arg(stamp)::timestamptz),
    cleanup_pending = cleanup_pending OR sqlc.arg(revoke)::boolean
WHERE github_installation_id = sqlc.arg(github_installation_id);
