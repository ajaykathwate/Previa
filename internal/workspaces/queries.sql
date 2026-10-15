-- name: ListWorkspaces :many
SELECT w.id,w.name,w.description,w.owner_user_id,w.revision,w.profile_revision,w.created_at,u.username AS owner_login,
coalesce(l.version,0)::bigint AS logo_version,m.role,m.revision AS membership_revision
FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
JOIN users u ON u.id=w.owner_user_id LEFT JOIN workspace_logos l ON l.workspace_id=w.id
WHERE m.user_id=sqlc.arg(user_id)::bigint AND m.state='active' AND w.state='active'
ORDER BY lower(w.name),w.id;

-- name: ReadWorkspace :one
SELECT w.id,w.name,w.description,w.owner_user_id,w.revision,w.profile_revision,w.created_at,u.username AS owner_login,
coalesce(l.version,0)::bigint AS logo_version,m.role,m.revision AS membership_revision
FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
JOIN users u ON u.id=w.owner_user_id LEFT JOIN workspace_logos l ON l.workspace_id=w.id
WHERE w.id=sqlc.arg(workspace_id)::bigint AND m.user_id=sqlc.arg(user_id)::bigint AND m.state='active' AND w.state='active';

-- name: ReadRepositoryAccess :one
SELECT r.id AS repo_id,r.full_name,i.github_installation_id,i.workspace_id,m.role,m.state AS membership_state,
       u.username,u.github_id,coalesce(workspace_authorization_stamp(r.id,u.id),'')::text AS authorization_stamp,
       workspace_repository_access(r.id,u.id,sqlc.arg(ability)::text)::boolean AS allowed
FROM repos r JOIN installations i ON i.id=r.installation_id JOIN users u ON u.id=sqlc.arg(user_id)::bigint
LEFT JOIN workspace_memberships m ON m.workspace_id=i.workspace_id AND m.user_id=u.id
WHERE r.id=sqlc.arg(repo_id)::bigint;

-- name: WorkspaceRepositories :many
SELECT r.id,r.full_name,r.accessible,i.status FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE i.workspace_id=sqlc.arg(workspace_id)::bigint
ORDER BY lower(r.full_name),r.id;

-- name: WorkspaceMembers :many
SELECT m.user_id,m.role,m.revision,m.joined_at,u.username,u.avatar_url,
       coalesce(array_agg(g.repo_id ORDER BY g.repo_id) FILTER(WHERE g.repo_id IS NOT NULL),'{}'::bigint[])::bigint[] AS repository_ids
FROM workspace_memberships m JOIN users u ON u.id=m.user_id
LEFT JOIN repository_memberships g ON g.workspace_id=m.workspace_id AND g.user_id=m.user_id
WHERE m.workspace_id=sqlc.arg(workspace_id)::bigint AND m.state='active'
AND strpos(lower(u.username),lower(sqlc.arg(search)::text))>0
GROUP BY m.workspace_id,m.user_id,u.id ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,lower(u.username),m.user_id
LIMIT 20 OFFSET sqlc.arg(page_offset)::bigint;

-- name: WorkspaceInvitations :many
SELECT id,github_id,login,role,expires_at,state,revision FROM workspace_invitations
WHERE workspace_id=sqlc.arg(workspace_id)::bigint AND state='pending'
AND strpos(lower(login),lower(sqlc.arg(search)::text))>0
ORDER BY created_at DESC,id DESC LIMIT 20 OFFSET sqlc.arg(page_offset)::bigint;

-- name: CountWorkspaceMembers :one
SELECT count(*) FROM workspace_memberships m JOIN users u ON u.id=m.user_id
WHERE m.workspace_id=sqlc.arg(workspace_id) AND m.state='active' AND strpos(lower(u.username),lower(sqlc.arg(search)::text))>0;

-- name: CountWorkspaceInvitations :one
SELECT count(*) FROM workspace_invitations WHERE workspace_id=sqlc.arg(workspace_id) AND state='pending' AND strpos(lower(login),lower(sqlc.arg(search)::text))>0;

-- name: WorkspaceTransfer :one
SELECT t.*,u.username AS target_login FROM workspace_transfers t JOIN users u ON u.id=t.to_user_id WHERE t.workspace_id=$1;

-- name: WorkspaceForRepository :one
SELECT i.workspace_id FROM repos r JOIN installations i ON i.id=r.installation_id WHERE r.id=$1;

-- name: WorkspacePreference :one
SELECT coalesce(workspace_id,0)::bigint AS workspace_id FROM workspace_preferences WHERE user_id=$1;

-- name: SaveWorkspacePreference :exec
INSERT INTO workspace_preferences(user_id,workspace_id) VALUES($1,$2)
ON CONFLICT(user_id) DO UPDATE SET workspace_id=EXCLUDED.workspace_id;

-- name: CreateWorkspace :one
INSERT INTO workspaces(name,owner_user_id) VALUES($1,$2) RETURNING id;

-- name: CreateOwnerMembership :exec
INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner');

-- name: LockWorkspace :one
SELECT * FROM workspaces WHERE id=$1 AND state='active' FOR UPDATE;

-- name: LockWorkspaceInstallations :many
SELECT id FROM installations WHERE workspace_id=$1 ORDER BY id FOR UPDATE;

-- name: LockMembership :one
SELECT * FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2 FOR UPDATE;

-- name: UpdateWorkspaceProfile :exec
UPDATE workspaces SET name=$2,description=$3,profile_revision=profile_revision+1 WHERE id=$1;

-- name: AdvanceWorkspaceProfile :exec
UPDATE workspaces SET profile_revision=profile_revision+1 WHERE id=$1;

-- name: SaveWorkspaceLogo :exec
INSERT INTO workspace_logos(workspace_id,version,data) VALUES($1,$2,$3)
ON CONFLICT(workspace_id) DO UPDATE SET version=EXCLUDED.version,data=EXCLUDED.data;

-- name: DeleteWorkspaceLogo :exec
DELETE FROM workspace_logos WHERE workspace_id=$1;

-- name: ReadWorkspaceLogo :one
SELECT l.data,l.version FROM workspace_logos l JOIN workspaces w ON w.id=l.workspace_id
JOIN workspace_memberships m ON m.workspace_id=w.id
WHERE w.id=$1 AND m.user_id=$2 AND m.state='active' AND w.state='active';

-- name: AdvanceWorkspaceRevision :exec
UPDATE workspaces SET revision=revision+1 WHERE id=$1;

-- name: RevokeUnauthorizedInvitations :exec
UPDATE workspace_invitations v SET state='revoked',revision=revision+1
WHERE v.workspace_id=$1 AND v.state='pending' AND NOT EXISTS(
 SELECT 1 FROM workspace_memberships m WHERE m.workspace_id=v.workspace_id AND m.user_id=v.created_by AND m.state='active'
 AND m.role IN ('owner','admin') AND (v.role<>'admin' OR m.role='owner'));

-- name: PutMembership :exec
INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,$3)
ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role,state='active',revision=workspace_memberships.revision+1,removed_at=NULL,joined_at=clock_timestamp();

-- name: ChangeMembership :exec
UPDATE workspace_memberships SET role=$3,state=$4,revision=revision+1,
removed_at=CASE WHEN $4::text='removed' THEN clock_timestamp() ELSE NULL END WHERE workspace_id=$1 AND user_id=$2;

-- name: ClearRepositoryGrants :exec
DELETE FROM repository_memberships WHERE workspace_id=$1 AND user_id=$2;

-- name: PutRepositoryGrant :execrows
INSERT INTO repository_memberships(repo_id,workspace_id,user_id)
SELECT r.id,i.workspace_id,sqlc.arg(user_id)::bigint FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE r.id=sqlc.arg(repo_id)::bigint AND i.workspace_id=sqlc.arg(workspace_id)::bigint
ON CONFLICT(repo_id,user_id) DO UPDATE SET revision=repository_memberships.revision+1;

-- name: CreateInvitation :one
INSERT INTO workspace_invitations(workspace_id,github_id,login,role,token_hash,expires_at,created_by)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id;

-- name: PutInvitationRepository :execrows
INSERT INTO workspace_invitation_repositories(invitation_id,repo_id)
SELECT sqlc.arg(invitation_id)::bigint,r.id FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE r.id=sqlc.arg(repo_id)::bigint AND i.workspace_id=sqlc.arg(workspace_id)::bigint;

-- name: ReadInvitation :one
SELECT v.*,w.name,w.state AS workspace_state FROM workspace_invitations v JOIN workspaces w ON w.id=v.workspace_id WHERE token_hash=$1;

-- name: LockInvitation :one
SELECT * FROM workspace_invitations WHERE id=$1 FOR UPDATE;

-- name: UpdateInvitationState :exec
UPDATE workspace_invitations SET state=$2,revision=revision+1 WHERE id=$1;

-- name: ExpirePendingInvitation :exec
UPDATE workspace_invitations SET state='revoked',revision=revision+1
WHERE workspace_id=$1 AND github_id=$2 AND state='pending' AND expires_at<=clock_timestamp();

-- name: RegenerateInvitation :exec
UPDATE workspace_invitations SET token_hash=$2,expires_at=$3,revision=revision+1 WHERE id=$1 AND state='pending';

-- name: InvitationRepositories :many
SELECT repo_id FROM workspace_invitation_repositories WHERE invitation_id=$1 ORDER BY repo_id;

-- name: SaveWorkspaceTransfer :exec
INSERT INTO workspace_transfers(workspace_id,from_user_id,to_user_id,revision,expires_at) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(workspace_id) DO UPDATE SET from_user_id=EXCLUDED.from_user_id,to_user_id=EXCLUDED.to_user_id,revision=EXCLUDED.revision,expires_at=EXCLUDED.expires_at;

-- name: DeleteWorkspaceTransfer :exec
DELETE FROM workspace_transfers WHERE workspace_id=$1;

-- name: SetWorkspaceOwner :exec
UPDATE workspaces SET owner_user_id=$2,revision=revision+1 WHERE id=$1;

-- name: CancelRevokedRequests :many
WITH affected AS MATERIALIZED (
 SELECT p.id,b.state AS prior_state FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
 JOIN build_intents b ON b.pull_request_id=p.id
 LEFT JOIN environments e ON e.pull_request_id=p.id
 WHERE i.workspace_id=$1 AND b.source='manual'
 AND (b.state IN ('pending','queued','running') OR (b.state='succeeded'
  AND e.pending_generation=b.generation AND e.pending_revision=b.revision AND e.pending_container_id IS NOT NULL))
 AND NOT workspace_intent_authorized(p.id) AND NOT workspace_request_published(p.id)
 ORDER BY p.id FOR UPDATE OF p
), cancelled AS (
 UPDATE build_intents b SET state='cancelled',completed_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM affected a WHERE b.pull_request_id=a.id
 RETURNING b.pull_request_id,b.workspace_id,b.started_at,b.completed_at,b.generation,b.revision,a.prior_state
), detached AS (
 UPDATE environments e SET pending_container_id=NULL,pending_image_tag=NULL,pending_generation=NULL,
 pending_revision=NULL,pending_head_sha=NULL,
 status=CASE WHEN e.container_id IS NOT NULL THEN 'running' ELSE 'failed' END,
 last_error_code='workspace_access_revoked'
 FROM cancelled c WHERE e.pull_request_id=c.pull_request_id
 AND e.pending_generation=c.generation AND e.pending_revision=c.revision
), ledger AS (
 INSERT INTO workspace_build_usage(workspace_id,day,build_seconds,builds)
 SELECT workspace_id,(completed_at AT TIME ZONE 'UTC')::date,
 sum(GREATEST(0,floor(extract(epoch FROM completed_at-started_at))))::bigint,count(*)::integer
 FROM cancelled WHERE started_at IS NOT NULL AND workspace_id IS NOT NULL AND prior_state<>'succeeded'
 GROUP BY workspace_id,(completed_at AT TIME ZONE 'UTC')::date
 ON CONFLICT(workspace_id,day) DO UPDATE SET build_seconds=workspace_build_usage.build_seconds+EXCLUDED.build_seconds,builds=workspace_build_usage.builds+EXCLUDED.builds
)
UPDATE pull_requests p SET generation=p.generation+1 FROM cancelled c WHERE p.id=c.pull_request_id RETURNING p.id;

-- name: UserByGitHubID :one
SELECT id FROM users WHERE github_id=$1;

-- name: BuildRequestActor :one
SELECT p.repo_id,b.source,b.requested_by,workspace_intent_authorized(p.id)::boolean AS authorized
FROM pull_requests p JOIN build_intents b ON b.pull_request_id=p.id WHERE p.id=$1;

-- name: CanManageWorkspace :one
SELECT EXISTS(SELECT 1 FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
WHERE w.id=$1 AND m.user_id=$2 AND w.state='active' AND m.state='active' AND m.role IN ('owner','admin'))::boolean;

-- name: WorkspaceCounts :one
SELECT (SELECT count(*) FROM workspace_memberships WHERE workspace_id=sqlc.arg(workspace_id)::bigint AND state='active')::bigint AS members,
(SELECT count(*) FROM workspace_invitations WHERE workspace_id=sqlc.arg(workspace_id)::bigint AND state='pending')::bigint AS invitations,
(SELECT count(*) FROM repos r JOIN installations i ON i.id=r.installation_id
 WHERE i.workspace_id=sqlc.arg(workspace_id)::bigint AND workspace_repository_access(r.id,sqlc.arg(user_id)::bigint,'read'))::bigint AS repositories;

-- name: WorkspaceAdmins :many
SELECT m.user_id,u.username FROM workspace_memberships m JOIN users u ON u.id=m.user_id
WHERE m.workspace_id=$1 AND m.state='active' AND m.role='admin' ORDER BY lower(u.username),m.user_id;

-- name: InvitationScope :many
SELECT r.id,r.full_name FROM workspace_invitation_repositories g JOIN repos r ON r.id=g.repo_id JOIN installations i ON i.id=r.installation_id
JOIN workspace_invitations v ON v.id=g.invitation_id WHERE v.id=$1 AND i.workspace_id=v.workspace_id ORDER BY lower(r.full_name),r.id;

-- name: InvitationCreatorAuthorized :one
SELECT EXISTS(SELECT 1 FROM workspace_memberships m WHERE m.workspace_id=$1 AND m.user_id=$2 AND m.state='active'
AND m.role IN ('owner','admin') AND ($3::text<>'admin' OR m.role='owner'))::boolean;

-- name: CurrentRepositoryGrants :many
SELECT repo_id FROM repository_memberships WHERE workspace_id=$1 AND user_id=$2 ORDER BY repo_id;
