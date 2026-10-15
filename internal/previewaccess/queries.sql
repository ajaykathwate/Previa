-- Private previews (db/migrations/0027_private_previews.sql).

-- name: SetPrivatePreviews :execrows
UPDATE repos r SET private_previews = sqlc.arg(private)::boolean
FROM installations i
WHERE r.id = sqlc.arg(repo_id) AND i.id = r.installation_id AND i.owner_user_id = sqlc.arg(owner_user_id)::bigint
  AND r.private_previews IS DISTINCT FROM sqlc.arg(private)::boolean;

-- name: OwnsRepository :one
SELECT EXISTS (SELECT 1 FROM repos r JOIN installations i ON i.id = r.installation_id
    WHERE r.id = sqlc.arg(repo_id) AND i.owner_user_id = sqlc.arg(owner_user_id)::bigint)::boolean;

-- name: AdvanceRouteTable :one
-- Private or public changes how the router serves the repository's hosts.
UPDATE route_table SET version = version + 1 WHERE singleton RETURNING version;

-- name: PreviewForHost :one
SELECT e.id AS environment_id, r.full_name, r.private_previews, i.github_installation_id,
       coalesce(i.owner_user_id, 0)::bigint AS owner_user_id
FROM environments e JOIN pull_requests p ON p.id = e.pull_request_id
JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
WHERE e.subdomain = $1;
