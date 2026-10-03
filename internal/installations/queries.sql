-- name: EnsureInstallation :exec
INSERT INTO installations (github_installation_id, status)
VALUES ($1, 'active') ON CONFLICT (github_installation_id) DO NOTHING;

-- name: LockInstallation :one
SELECT id, github_installation_id, owner_user_id, status, account_login, revision
FROM installations WHERE github_installation_id = $1 FOR UPDATE;

-- name: GetInstallationRevision :one
SELECT revision FROM installations WHERE github_installation_id = $1;

-- name: LinkInstallation :exec
UPDATE installations SET owner_user_id = $2, account_login = $3,
status = $4, revision = revision + 1 WHERE id = $1;

-- name: UpdateInstallationLifecycle :exec
UPDATE installations SET status = $2, account_login = $3, revision = revision + 1 WHERE id = $1;

-- name: MarkRepositoriesUnavailable :exec
UPDATE repos SET accessible = false WHERE installation_id = $1;

-- name: RemoveRepositoryAccess :exec
UPDATE repos SET accessible = false WHERE installation_id = $1 AND github_repo_id = $2;

-- name: UpsertLinkedRepository :execrows
INSERT INTO repos (installation_id, github_repo_id, full_name, accessible)
VALUES ($1, $2, $3, true)
ON CONFLICT (full_name) DO UPDATE
SET github_repo_id = EXCLUDED.github_repo_id, accessible = true
WHERE repos.installation_id = EXCLUDED.installation_id
  AND (repos.github_repo_id IS NULL OR repos.github_repo_id = EXCLUDED.github_repo_id);

-- name: UpdateRepositoryByGitHubID :execrows
UPDATE repos SET full_name = $3, accessible = true
WHERE github_repo_id = $2 AND installation_id = $1;

-- name: ListLinkedInstallations :many
SELECT i.github_installation_id, i.status, i.account_login, r.full_name,
       r.github_repo_id, r.accessible
FROM installations i LEFT JOIN repos r ON r.installation_id = i.id
WHERE i.owner_user_id = $1
ORDER BY i.github_installation_id, r.full_name;

-- name: GetRepositoryInstallation :one
SELECT installation_id FROM repos WHERE github_repo_id = $1;

-- name: ReassignInactiveOwnedRepository :execrows
UPDATE repos SET installation_id = sqlc.arg(new_installation_id),
    full_name = sqlc.arg(full_name), accessible = true
WHERE github_repo_id = sqlc.arg(github_repo_id)
  AND EXISTS (
    SELECT 1 FROM installations old_i JOIN installations new_i
      ON new_i.id = sqlc.arg(new_installation_id)
    WHERE old_i.id = repos.installation_id AND old_i.status = 'inactive'
      AND new_i.owner_user_id IS NOT NULL
      AND old_i.owner_user_id = new_i.owner_user_id
  );
