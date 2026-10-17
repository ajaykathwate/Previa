-- name: InsertEvent :execrows
-- One event about a pull request; its workspace and repository derive from
-- it. A repeated dedup key records nothing, and a pull request whose
-- installation belongs to no workspace has no timeline to add to.
INSERT INTO activity_events (workspace_id, repo_id, pull_request_id, publication_id, actor_user_id, kind, visibility, metadata, dedup_key)
SELECT i.workspace_id, p.repo_id, p.id, sqlc.narg(publication_id)::bigint, sqlc.narg(actor_user_id)::bigint,
       sqlc.arg(kind)::text, sqlc.arg(visibility)::text, sqlc.arg(metadata)::jsonb, sqlc.narg(dedup_key)::text
FROM pull_requests p
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE p.id = sqlc.arg(pull_request_id)::bigint AND i.workspace_id IS NOT NULL
ON CONFLICT (workspace_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING;

-- name: InsertRepositoryEvent :execrows
-- One event about a repository rather than a pull request, such as a test
-- data baseline refresh (docs/32); its workspace derives from it.
INSERT INTO activity_events (workspace_id, repo_id, actor_user_id, kind, visibility, metadata, dedup_key)
SELECT i.workspace_id, r.id, sqlc.narg(actor_user_id)::bigint,
       sqlc.arg(kind)::text, sqlc.arg(visibility)::text, sqlc.arg(metadata)::jsonb, sqlc.narg(dedup_key)::text
FROM repos r
JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id)::bigint AND i.workspace_id IS NOT NULL
ON CONFLICT (workspace_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING;

-- name: PruneAgedEvents :one
-- Deletes up to batch_size events older than cutoff, oldest first, and moves
-- each affected pull request's pruned marker forward.
WITH doomed AS (
    SELECT id FROM activity_events
    WHERE occurred_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY occurred_at
    LIMIT sqlc.arg(batch_size)::integer
), deleted AS (
    DELETE FROM activity_events e USING doomed d WHERE e.id = d.id
    RETURNING e.pull_request_id, e.occurred_at
), marked AS (
    INSERT INTO activity_pruned (pull_request_id, pruned_through)
    SELECT pull_request_id, max(occurred_at) FROM deleted WHERE pull_request_id IS NOT NULL GROUP BY pull_request_id
    ON CONFLICT (pull_request_id) DO UPDATE
    SET pruned_through = GREATEST(activity_pruned.pruned_through, EXCLUDED.pruned_through)
)
SELECT count(*) FROM deleted;

-- name: PruneOverflowEvents :one
-- Deletes up to batch_size of the oldest events beyond each pull request's
-- newest per_pull_request, and moves the pruned marker forward.
WITH crowded AS (
    SELECT pull_request_id FROM activity_events
    WHERE pull_request_id IS NOT NULL
    GROUP BY pull_request_id
    HAVING count(*) > sqlc.arg(per_pull_request)::integer
    LIMIT 100
), ranked AS (
    SELECT e.id, row_number() OVER (PARTITION BY e.pull_request_id ORDER BY e.occurred_at DESC, e.id DESC) AS n
    FROM activity_events e JOIN crowded c ON c.pull_request_id = e.pull_request_id
), doomed AS (
    SELECT id FROM ranked WHERE n > sqlc.arg(per_pull_request)::integer LIMIT sqlc.arg(batch_size)::integer
), deleted AS (
    DELETE FROM activity_events e USING doomed d WHERE e.id = d.id
    RETURNING e.pull_request_id, e.occurred_at
), marked AS (
    INSERT INTO activity_pruned (pull_request_id, pruned_through)
    SELECT pull_request_id, max(occurred_at) FROM deleted GROUP BY pull_request_id
    ON CONFLICT (pull_request_id) DO UPDATE
    SET pruned_through = GREATEST(activity_pruned.pruned_through, EXCLUDED.pruned_through)
)
SELECT count(*) FROM deleted;
