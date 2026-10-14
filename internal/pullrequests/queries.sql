-- name: FindScope :one
SELECT r.installation_id FROM repos r WHERE r.id=$1;
-- name: LockInstallation :one
SELECT id, github_installation_id, revision, status, owner_user_id, workspace_id FROM installations WHERE id=$1 FOR UPDATE;
-- name: LockRepository :one
SELECT id, installation_id, github_repo_id, full_name, accessible, build_mode, observation_version FROM repos WHERE id=$1 FOR UPDATE;
-- name: FindGitHubRepository :one
SELECT r.id FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE r.github_repo_id=$1 AND i.github_installation_id=$2;
-- name: LockPR :one
SELECT * FROM pull_requests WHERE repo_id=$1 AND number=$2 FOR UPDATE;
-- name: SaveObservation :one
INSERT INTO pull_requests (repo_id,number,head_sha,state,generation,title,author,head_branch,html_url,head_repo_id,is_fork,source_updated_at,mergeable,mergeable_state,build_status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (repo_id,number) DO UPDATE SET head_sha=EXCLUDED.head_sha,state=EXCLUDED.state,
 generation=EXCLUDED.generation,title=EXCLUDED.title,author=EXCLUDED.author,
 head_branch=EXCLUDED.head_branch,html_url=EXCLUDED.html_url,head_repo_id=EXCLUDED.head_repo_id,
 is_fork=EXCLUDED.is_fork,source_updated_at=EXCLUDED.source_updated_at,needs_refresh=false,mergeable=EXCLUDED.mergeable,mergeable_state=EXCLUDED.mergeable_state,build_status=EXCLUDED.build_status,
 mergeability_unresolved_at=NULL
RETURNING id;
-- name: MarkMergeabilityUnresolved :execrows
-- The bounded mergeability check gave up on exactly this observation.
UPDATE pull_requests SET mergeability_unresolved_at=clock_timestamp()
WHERE repo_id=$1 AND number=$2 AND generation=$3 AND head_sha=$4 AND state='open' AND build_status='checking';
-- name: ClearMergeabilityUnresolved :exec
UPDATE pull_requests SET mergeability_unresolved_at=NULL WHERE id=$1 AND mergeability_unresolved_at IS NOT NULL;
-- name: RequireRefresh :exec
UPDATE pull_requests SET needs_refresh=true,generation=generation+1 WHERE id=$1;
-- name: BumpObservation :exec
UPDATE repos SET observation_version=observation_version+1 WHERE id=$1;
-- name: InvalidateObservedIntent :exec
UPDATE build_intents SET state='superseded',completed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE pull_request_id=$1 AND generation<>$2 AND state IN ('pending','queued','running');
-- name: ReadSync :one
SELECT * FROM pr_syncs WHERE repo_id=$1;
-- name: LockSync :one
SELECT * FROM pr_syncs WHERE repo_id=$1 FOR UPDATE;
-- name: StartSync :one
INSERT INTO pr_syncs(repo_id,state) VALUES($1,'queued')
ON CONFLICT(repo_id) DO UPDATE SET revision=pr_syncs.revision+1,state='queued',next_page=1,fetched=0,
 requested_at=clock_timestamp(),completed_at=NULL,message=''
RETURNING *;
-- name: FinishSyncPage :exec
UPDATE pr_syncs SET next_page=$3, fetched=fetched+$4,state=$5,message='',
 completed_at=CASE WHEN $5::text='complete' THEN clock_timestamp() ELSE NULL END,
 last_success_at=CASE WHEN $5::text='complete' THEN clock_timestamp() ELSE last_success_at END
WHERE repo_id=$1 AND revision=$2;
-- name: FailSync :exec
UPDATE pr_syncs SET state='failed',message=$3 WHERE repo_id=$1 AND revision=$2 AND next_page=$4 AND state<>'complete';
-- name: MarkSyncing :exec
UPDATE pr_syncs SET state='syncing',message='' WHERE repo_id=$1 AND revision=$2;
-- name: OwnedRepository :one
SELECT r.id,r.full_name,r.accessible,r.build_mode,r.private_previews,i.status,i.github_installation_id
FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE r.id=$1 AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read');
-- name: CountPRs :one
SELECT count(*) FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
WHERE r.id=sqlc.arg(id)::bigint AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read') AND (sqlc.arg(column_3)::text::text='all' OR p.state=sqlc.arg(column_3)::text)
AND strpos(lower(p.title || ' ' || p.number::text || ' ' || p.author || ' ' || p.head_branch),lower(sqlc.arg(lower)::text))>0;
-- name: ListPRs :many
-- One page of a repository's PRs with their current request and preview, so
-- the list can say which PRs have a live preview and whether the request is
-- for the latest commit.
SELECT p.*,coalesce(b.state,'Not requested') AS build_state,
 coalesce(b.generation = p.generation AND b.head_sha = p.head_sha, false)::boolean AS request_current,
 coalesce(e.status,'Not deployed') AS environment_status, coalesce(e.head_sha,'') AS deployed_sha,
 coalesce(e.subdomain,'')::text AS hostname
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id
WHERE r.id=sqlc.arg(id)::bigint AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read') AND (sqlc.arg(column_3)::text::text='all' OR p.state=sqlc.arg(column_3)::text)
AND strpos(lower(p.title || ' ' || p.number::text || ' ' || p.author || ' ' || p.head_branch),lower(sqlc.arg(lower)::text))>0
ORDER BY p.number DESC LIMIT 10 OFFSET sqlc.arg(row_offset)::integer;
-- name: OwnedPR :one
-- The PR's three lifecycle columns (observed, requested, deployed) and what
-- the owner may do next. A torn-down environment is shown as removed.
SELECT p.*,(r.accessible AND i.status='active') AS available,coalesce(b.state,'Not requested') AS build_state,coalesce(b.head_sha,'') AS requested_sha,
 b.available_at,b.revision AS request_revision,
 coalesce(b.generation = p.generation AND b.head_sha = p.head_sha, false)::boolean AS request_current,
 coalesce(b.source,'')::text AS request_source,
 b.requested_at, b.started_at, b.completed_at, coalesce(u.username,'')::text AS requested_by_login,
 coalesce(CASE WHEN b.waiting_revision = b.revision THEN b.waiting_reason END,'')::text AS waiting_reason,
 coalesce(e.id,0)::bigint AS environment_id,
 coalesce(e.status,'Not deployed') AS environment_status,
 coalesce(e.head_sha,'') AS deployed_sha, coalesce(e.subdomain,'')::text AS hostname,
 e.expires_at, e.last_active_at AS published_at, coalesce(e.last_error_code,'')::text AS failure_code,
 e.last_visited_at, coalesce(e.visits,0)::bigint AS visits, e.asleep_at,
 coalesce(e.applied_generation = b.generation AND e.applied_revision = b.revision, false)::boolean AS failure_current,
 (p.teardown_requested_generation = p.generation)::boolean AS teardown_requested,
 pc.services AS backend_services, coalesce(pc.setup, false)::boolean AS backend_setup, pc.processes AS backend_processes,
 coalesce(pc.docs_path, '')::text AS backend_docs_path, coalesce(pc.outbound, 0)::integer AS backend_outbound,
 coalesce(pc.outbound_withheld, false)::boolean AS backend_outbound_withheld
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN users u ON u.id=b.requested_by
LEFT JOIN environments e ON e.pull_request_id=p.id
LEFT JOIN preview_components pc ON pc.pull_request_id=p.id AND pc.generation=e.applied_generation AND pc.revision=e.applied_revision
WHERE r.id=sqlc.arg(id)::bigint AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'read') AND p.number=sqlc.arg(number)::integer;
-- name: CountActivity :one
SELECT count(*) FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id AND e.status<>'torn_down'
LEFT JOIN build_failure_dismissals d ON d.pull_request_id=p.id AND d.user_id=sqlc.arg(owner_user_id) AND d.revision=b.revision
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.arg(owner_user_id)::bigint,'read') AND (sqlc.arg(repo_id)::bigint=0 OR r.id=sqlc.arg(repo_id))
AND ((sqlc.arg(previews)::boolean AND e.head_sha IS NOT NULL) OR (NOT sqlc.arg(previews)::boolean AND b.pull_request_id IS NOT NULL))
AND (sqlc.arg(failures)::text = '' OR (b.state = 'failed' AND (
 (sqlc.arg(failures) = 'attention' AND p.state = 'open' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at >= sqlc.arg(attention_since))
 OR (sqlc.arg(failures) = 'older' AND p.state = 'open' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at < sqlc.arg(attention_since))
 OR (sqlc.arg(failures) = 'dismissed' AND d.pull_request_id IS NOT NULL))));
-- name: Activity :many
-- Builds (current requests) or Previews (environments that serve a commit:
-- live, updating or kept after a failed update; an address reserved by a
-- build that never went live is not a preview), most recently active first. failures narrows Builds to failed requests that need
-- attention (recent), older ones, or the ones this owner dismissed; the
-- same conditions as DeliverySummary.
SELECT p.repo_id,p.number,p.head_sha,p.title,r.full_name,r.accessible,i.status AS installation_status,
 coalesce(b.state,'Not requested') AS build_state,coalesce(b.head_sha,'') AS requested_sha,b.available_at,
 b.started_at, b.completed_at, coalesce(b.source,'')::text AS request_source,
 coalesce(b.generation = p.generation AND b.head_sha = p.head_sha, false)::boolean AS request_current,
 coalesce(b.revision, 0)::bigint AS request_revision,
 (d.pull_request_id IS NOT NULL)::boolean AS failure_dismissed,
 e.last_active_at AS published_at, e.last_visited_at, e.asleep_at,
 coalesce(e.status,'Not deployed') AS environment_status,coalesce(e.head_sha,'') AS deployed_sha,
 coalesce(CASE WHEN b.waiting_revision = b.revision THEN b.waiting_reason END,'')::text AS waiting_reason,
 coalesce(e.subdomain,'')::text AS hostname, e.expires_at, coalesce(e.last_error_code,'')::text AS failure_code,
 coalesce(e.applied_generation = b.generation AND e.applied_revision = b.revision, false)::boolean AS failure_current,
 p.state AS pr_state
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id AND e.status<>'torn_down'
LEFT JOIN build_failure_dismissals d ON d.pull_request_id=p.id AND d.user_id=sqlc.arg(owner_user_id) AND d.revision=b.revision
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.arg(owner_user_id)::bigint,'read') AND (sqlc.arg(repo_id)::bigint=0 OR r.id=sqlc.arg(repo_id))
AND ((sqlc.arg(previews)::boolean AND e.head_sha IS NOT NULL) OR (NOT sqlc.arg(previews)::boolean AND b.pull_request_id IS NOT NULL))
AND (sqlc.arg(failures)::text = '' OR (b.state = 'failed' AND (
 (sqlc.arg(failures) = 'attention' AND p.state = 'open' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at >= sqlc.arg(attention_since))
 OR (sqlc.arg(failures) = 'older' AND p.state = 'open' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at < sqlc.arg(attention_since))
 OR (sqlc.arg(failures) = 'dismissed' AND d.pull_request_id IS NOT NULL))))
ORDER BY (CASE WHEN sqlc.arg(previews)::boolean THEN e.last_active_at ELSE b.updated_at END) DESC NULLS LAST, p.id DESC
LIMIT 10 OFFSET sqlc.arg(row_offset);

-- name: DismissFailure :execrows
-- Dismisses the PR's failed request, only while that revision is its current
-- request and failed; dismissing again replaces an older dismissal.
INSERT INTO build_failure_dismissals (user_id, pull_request_id, revision)
SELECT sqlc.arg(user_id), b.pull_request_id, b.revision FROM build_intents b
WHERE b.pull_request_id = sqlc.arg(pull_request_id) AND b.revision = sqlc.arg(revision) AND b.state = 'failed'
ON CONFLICT (user_id, pull_request_id) DO UPDATE SET revision = EXCLUDED.revision, dismissed_at = clock_timestamp();

-- name: RestoreFailure :exec
DELETE FROM build_failure_dismissals WHERE user_id = sqlc.arg(user_id) AND pull_request_id = sqlc.arg(pull_request_id);

-- name: InitialDiscoveryRepositories :many
SELECT r.id FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE i.github_installation_id=$1 AND i.workspace_id IS NOT NULL AND i.status='active'
 AND r.accessible AND NOT EXISTS(SELECT 1 FROM pr_syncs s WHERE s.repo_id=r.id)
ORDER BY r.id FOR UPDATE OF r;

-- name: HasAutomaticIntent :one
SELECT EXISTS(SELECT 1 FROM build_intents WHERE pull_request_id=$1 AND generation=$2
 AND source='automatic' AND state IN ('pending','queued','running'));
-- name: FindPRIdentity :one
SELECT repo_id, number FROM pull_requests WHERE id=$1;

-- name: DeliverySummary :one
-- Overview counts across the owner's repositories: open PRs, current requests
-- by state and live previews. A failed request needs attention when it
-- finished at or after attention_since and the owner has not dismissed it;
-- earlier undismissed ones are counted as older.
SELECT
 count(*)::bigint AS open_prs,
 count(*) FILTER (WHERE b.state IN ('pending','queued') AND b.generation = p.generation)::bigint AS waiting,
 count(*) FILTER (WHERE b.state = 'running' AND b.generation = p.generation)::bigint AS building,
 count(*) FILTER (WHERE b.state = 'failed' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at >= sqlc.arg(attention_since))::bigint AS failed,
 count(*) FILTER (WHERE b.state = 'failed' AND b.generation = p.generation AND d.pull_request_id IS NULL AND b.completed_at < sqlc.arg(attention_since))::bigint AS failed_older,
 count(*) FILTER (WHERE e.status IN ('running','failed','publishing') AND e.head_sha IS NOT NULL)::bigint AS live
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id
LEFT JOIN build_failure_dismissals d ON d.pull_request_id=p.id AND d.user_id=sqlc.arg(owner_user_id) AND d.revision=b.revision
WHERE (sqlc.arg(workspace_id)::bigint=0 OR i.workspace_id=sqlc.arg(workspace_id)::bigint) AND workspace_repository_access(r.id,sqlc.arg(owner_user_id)::bigint,'read') AND p.state='open';

-- name: CanOperateRepository :one
SELECT workspace_repository_access($1::bigint,$2::bigint,'operate')::boolean;
