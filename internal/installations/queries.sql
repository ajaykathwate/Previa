-- name: EnsureInstallation :exec
INSERT INTO installations (github_installation_id, status)
VALUES ($1, 'active') ON CONFLICT (github_installation_id) DO NOTHING;

-- name: LockInstallation :one
SELECT id, github_installation_id, owner_user_id, status, account_login, revision, workspace_id
FROM installations WHERE github_installation_id = $1 FOR UPDATE;

-- name: GetInstallationRevision :one
SELECT revision FROM installations WHERE github_installation_id = $1;

-- name: LinkInstallation :exec
UPDATE installations SET owner_user_id = coalesce(owner_user_id,$2), workspace_id=sqlc.narg(workspace_id)::bigint, account_login = $3,
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
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_installation_access(i.id,sqlc.narg(owner_user_id)::bigint,'read')
AND (r.id IS NULL OR workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read'))
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
      AND new_i.workspace_id IS NOT NULL
      AND old_i.workspace_id = new_i.workspace_id
  );

-- name: GetWorkspaceSummary :one
SELECT
 (SELECT count(*) FROM installations own_i WHERE workspace_installation_access(own_i.id,sqlc.narg(owner_user_id)::bigint,'read') AND (sqlc.arg(workspace_id)::bigint=0 OR own_i.workspace_id=sqlc.arg(workspace_id)::bigint)) AS installations,
 count(*) AS repositories,
 count(*) FILTER (WHERE r.accessible AND i.status = 'active') AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read');

-- name: CountRepositoryPage :one
SELECT count(*) FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'read')
 AND strpos(lower(r.full_name), lower(sqlc.arg(search)::text)) > 0
 AND (sqlc.arg(access)::text = 'all'
   OR (sqlc.arg(access)::text = 'available' AND r.accessible AND i.status = 'active')
   OR (sqlc.arg(access)::text = 'unavailable' AND NOT (r.accessible AND i.status = 'active')));

-- name: ListRepositoryPage :many
-- One directory page with what each card shows: build mode, open PRs, live
-- previews and when PRs were last discovered.
SELECT r.id, r.full_name, r.accessible, i.status, i.account_login, i.github_installation_id, r.build_mode,
 (SELECT count(*) FROM pull_requests p WHERE p.repo_id = r.id AND p.state = 'open')::bigint AS open_prs,
 (SELECT count(*) FROM pull_requests p JOIN environments e ON e.pull_request_id = p.id
   WHERE p.repo_id = r.id AND p.state = 'open' AND e.head_sha IS NOT NULL
     AND e.status IN ('running', 'failed', 'publishing'))::bigint AS live_previews,
 s.last_success_at
FROM repos r JOIN installations i ON i.id = r.installation_id
LEFT JOIN pr_syncs s ON s.repo_id = r.id
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'read')
 AND strpos(lower(r.full_name), lower(sqlc.arg(search)::text)) > 0
 AND (sqlc.arg(access)::text = 'all'
   OR (sqlc.arg(access)::text = 'available' AND r.accessible AND i.status = 'active')
   OR (sqlc.arg(access)::text = 'unavailable' AND NOT (r.accessible AND i.status = 'active')))
ORDER BY lower(r.full_name), r.id
LIMIT sqlc.arg(page_size)::bigint OFFSET sqlc.arg(page_offset)::bigint;

-- name: ReadOwnedBuildPolicy :one
SELECT r.id, r.build_mode
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'manage')
    AND i.status = 'active' AND r.accessible;

-- name: FindPolicyInstallation :one
SELECT installation_id FROM repos WHERE id = $1;

-- name: DefaultWorkspace :one
SELECT workspace_default($1::bigint)::bigint AS workspace_id;

-- name: LockLinkWorkspace :one
SELECT id FROM workspaces WHERE id=$1 AND state='active' FOR UPDATE;

-- name: LockPolicyInstallation :one
SELECT id FROM installations
WHERE id = sqlc.arg(installation_id) AND workspace_installation_access(id,sqlc.narg(user_id)::bigint,'manage')
    AND status = 'active'
FOR UPDATE;

-- name: LockPolicyRepository :one
SELECT id, build_mode FROM repos
WHERE id = sqlc.arg(repo_id) AND installation_id = sqlc.arg(installation_id) AND accessible
AND workspace_repository_access(id,sqlc.arg(user_id)::bigint,'manage') FOR UPDATE;

-- name: SetRepositoryBuildMode :exec
UPDATE repos SET build_mode = $2 WHERE id = $1;

-- name: CancelUnstartedAutomaticIntents :many
WITH locked_prs AS MATERIALIZED (
    SELECT pr.id FROM pull_requests pr JOIN build_intents b ON b.pull_request_id = pr.id
    WHERE pr.repo_id = $1 AND b.source = 'automatic' AND b.state IN ('pending', 'queued')
    ORDER BY pr.id FOR UPDATE OF pr
)
UPDATE build_intents b SET state = 'cancelled', completed_at = clock_timestamp(),
    updated_at = clock_timestamp()
FROM locked_prs pr
WHERE b.pull_request_id = pr.id AND b.source = 'automatic'
    AND b.state IN ('pending', 'queued')
RETURNING b.pull_request_id;

-- name: AuthorizedLinkWorkspace :one
SELECT EXISTS(SELECT 1 FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
 WHERE w.id=$1 AND m.user_id=$2 AND w.state='active' AND m.state='active' AND m.role IN ('owner','admin'))::boolean;
