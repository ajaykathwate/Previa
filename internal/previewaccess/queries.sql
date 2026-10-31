-- Private previews (db/migrations/0027_private_previews.sql).

-- name: SetPrivatePreviews :execrows
UPDATE repos r SET private_previews = sqlc.arg(private)::boolean
FROM installations i
WHERE r.id = sqlc.arg(repo_id) AND i.id = r.installation_id AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage')
  AND r.private_previews IS DISTINCT FROM sqlc.arg(private)::boolean;

-- name: OwnsRepository :one
SELECT EXISTS (SELECT 1 FROM repos r JOIN installations i ON i.id = r.installation_id
    WHERE r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage'))::boolean;

-- name: AdvanceRouteTable :one
-- Private or public changes how the router serves the repository's hosts.
UPDATE route_table SET version = version + 1 WHERE singleton RETURNING version;

-- name: PreviewForHost :one
SELECT e.id AS environment_id,r.id AS repo_id,r.full_name,r.private_previews,i.github_installation_id,
 w.revision AS authorization_revision
FROM environments e JOIN pull_requests p ON p.id=e.pull_request_id
JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
JOIN workspaces w ON w.id=i.workspace_id AND w.state='active'
WHERE e.subdomain=$1 AND r.accessible AND i.status='active';

-- name: PreviewAuthorization :one
SELECT w.revision AS authorization_revision,
 workspace_repository_access(r.id,sqlc.arg(user_id)::bigint,'preview')::boolean AS allowed,
 EXISTS(SELECT 1 FROM workspace_memberships m WHERE m.workspace_id=w.id AND m.user_id=sqlc.arg(user_id)::bigint)::boolean AS membership_exists
FROM environments e JOIN pull_requests p ON p.id=e.pull_request_id
JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
JOIN workspaces w ON w.id=i.workspace_id AND w.state='active'
WHERE e.subdomain=sqlc.arg(subdomain)::text AND r.accessible AND i.status='active';

-- name: LockPreviewInstallation :one
SELECT i.id FROM installations i JOIN repos r ON r.installation_id=i.id WHERE r.id=$1 FOR UPDATE OF i;
