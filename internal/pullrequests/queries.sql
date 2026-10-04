-- name: FindScope :one
SELECT r.installation_id FROM repos r WHERE r.id=$1;
-- name: LockInstallation :one
SELECT id, github_installation_id, revision, status, owner_user_id FROM installations WHERE id=$1 FOR UPDATE;
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
 is_fork=EXCLUDED.is_fork,source_updated_at=EXCLUDED.source_updated_at,needs_refresh=false,mergeable=EXCLUDED.mergeable,mergeable_state=EXCLUDED.mergeable_state,build_status=EXCLUDED.build_status
RETURNING id;
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
SELECT r.id,r.full_name,r.accessible,r.build_mode,i.status,i.github_installation_id
FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE r.id=$1 AND i.owner_user_id=$2;
-- name: CountPRs :one
SELECT count(*) FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
WHERE r.id=$1 AND i.owner_user_id=$2 AND ($3::text='all' OR p.state=$3)
AND strpos(lower(p.title || ' ' || p.number::text || ' ' || p.author || ' ' || p.head_branch),lower($4))>0;
-- name: ListPRs :many
SELECT p.*,coalesce(b.state,'Not requested') AS build_state
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
WHERE r.id=$1 AND i.owner_user_id=$2 AND ($3::text='all' OR p.state=$3)
AND strpos(lower(p.title || ' ' || p.number::text || ' ' || p.author || ' ' || p.head_branch),lower($4))>0
ORDER BY p.number DESC LIMIT 10 OFFSET $5;
-- name: OwnedPR :one
SELECT p.*,(r.accessible AND i.status='active') AS available,coalesce(b.state,'Not requested') AS build_state,coalesce(b.head_sha,'') AS requested_sha,
 b.available_at,b.revision AS request_revision,coalesce(e.status,'Not deployed') AS environment_status,
 coalesce(e.head_sha,'') AS deployed_sha
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id AND e.status<>'torn_down'
WHERE r.id=$1 AND i.owner_user_id=$2 AND p.number=$3;
-- name: CountActivity :one
SELECT count(*) FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id AND e.status<>'torn_down'
WHERE i.owner_user_id=$1 AND ($2::bigint=0 OR r.id=$2)
AND (($3::boolean AND e.id IS NOT NULL) OR (NOT $3::boolean AND b.pull_request_id IS NOT NULL));
-- name: Activity :many
SELECT p.repo_id,p.number,p.head_sha,r.full_name,r.accessible,i.status AS installation_status,
 coalesce(b.state,'Not requested') AS build_state,coalesce(b.head_sha,'') AS requested_sha,b.available_at,
 coalesce(e.status,'Not deployed') AS environment_status,coalesce(e.head_sha,'') AS deployed_sha
FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id=p.id
LEFT JOIN environments e ON e.pull_request_id=p.id AND e.status<>'torn_down'
WHERE i.owner_user_id=$1 AND ($2::bigint=0 OR r.id=$2)
AND (($3::boolean AND e.id IS NOT NULL) OR (NOT $3::boolean AND b.pull_request_id IS NOT NULL))
ORDER BY p.id DESC LIMIT 10 OFFSET $4;

-- name: InitialDiscoveryRepositories :many
SELECT r.id FROM repos r JOIN installations i ON i.id=r.installation_id
WHERE i.github_installation_id=$1 AND i.owner_user_id IS NOT NULL AND i.status='active'
 AND r.accessible AND NOT EXISTS(SELECT 1 FROM pr_syncs s WHERE s.repo_id=r.id)
ORDER BY r.id FOR UPDATE OF r;

-- name: HasAutomaticIntent :one
SELECT EXISTS(SELECT 1 FROM build_intents WHERE pull_request_id=$1 AND generation=$2
 AND source='automatic' AND state IN ('pending','queued','running'));
-- name: FindPRIdentity :one
SELECT repo_id, number FROM pull_requests WHERE id=$1;
