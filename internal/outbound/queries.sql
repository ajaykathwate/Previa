-- Owner-approved outbound connections of a repository's previews. Owner
-- queries join through the installation's owner; the worker's query is
-- unscoped because the build it serves was authorized already.

-- name: OwnedRepository :one
SELECT r.id, r.full_name, (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'manage');

-- name: LockOwnedRepository :one
-- Serializes approvals of one repository, so the limit holds under
-- concurrent requests. Only an available repository takes new approvals.
SELECT r.id FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'manage') AND r.accessible AND i.status = 'active'
FOR UPDATE OF r;

-- name: ListDestinations :many
SELECT d.id, d.host, d.port, d.created_at
FROM repository_destinations d
JOIN repos r ON r.id = d.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE d.repo_id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'manage')
ORDER BY d.port, d.host;

-- name: CountDestinations :one
SELECT count(*) FROM repository_destinations WHERE repo_id = $1;

-- name: AddDestination :execrows
-- One destination per port and repository: a second one on the same port
-- inserts nothing.
INSERT INTO repository_destinations (repo_id, host, port, created_by)
VALUES (sqlc.arg(repo_id), sqlc.arg(host), sqlc.arg(port), sqlc.arg(created_by))
ON CONFLICT (repo_id, port) DO NOTHING;

-- name: RemoveDestination :one
DELETE FROM repository_destinations d
USING repos r, installations i
WHERE d.id = sqlc.arg(id) AND d.repo_id = sqlc.arg(repo_id) AND r.id = d.repo_id
  AND i.id = r.installation_id AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'manage')
RETURNING d.host, d.port;

-- name: DestinationsForRepositories :many
SELECT repo_id, host, port FROM repository_destinations
WHERE repo_id = ANY(sqlc.arg(repo_ids)::bigint[])
ORDER BY repo_id, port;

-- name: LockOutboundInstallation :one
SELECT i.id FROM installations i JOIN repos r ON r.installation_id=i.id WHERE r.id=$1 FOR UPDATE OF i;
