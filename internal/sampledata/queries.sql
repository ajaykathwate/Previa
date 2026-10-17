-- Persistent test data (docs/32-persistent-test-data.md): a repository's Test
-- data settings, its baseline refreshes and its baselines.

-- name: RepositoryScope :one
-- A repository for someone who may build in it: what the Test data tab and
-- its forms need, with who may change what.
SELECT r.id, r.full_name, r.github_repo_id, i.github_installation_id, i.workspace_id,
       (r.accessible AND i.status = 'active')::boolean AS available,
       workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'manage')::boolean AS can_manage
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id)::bigint AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate');

-- name: WorkerScope :one
-- A repository as a refresh needs it, without a person.
SELECT r.id, r.github_repo_id, i.github_installation_id, i.workspace_id,
       (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = $1;

-- name: GetSettings :one
SELECT * FROM repository_sampledata_settings WHERE repo_id = $1;

-- name: LockSettings :one
SELECT * FROM repository_sampledata_settings WHERE repo_id = $1 FOR UPDATE;

-- name: LockRepository :one
-- Serializes a repository's settings changes and refresh requests.
SELECT id FROM repos WHERE id = $1 FOR UPDATE;

-- name: SaveSettings :one
INSERT INTO repository_sampledata_settings (repo_id, enabled, keep_data, use_baseline, baseline_branch, auto_refresh, updated_by)
VALUES (sqlc.arg(repo_id), sqlc.arg(enabled), sqlc.arg(keep_data), sqlc.arg(use_baseline), sqlc.narg(baseline_branch), sqlc.arg(auto_refresh), sqlc.arg(updated_by))
ON CONFLICT (repo_id) DO UPDATE
SET enabled = EXCLUDED.enabled, keep_data = EXCLUDED.keep_data, use_baseline = EXCLUDED.use_baseline,
    baseline_branch = EXCLUDED.baseline_branch, auto_refresh = EXCLUDED.auto_refresh,
    revision = repository_sampledata_settings.revision + 1, updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp()
RETURNING *;

-- name: MarkChecked :exec
UPDATE repository_sampledata_settings SET checked_at = clock_timestamp(), checked_branch = sqlc.narg(branch), checked_sha = sqlc.narg(sha)
WHERE repo_id = sqlc.arg(repo_id);

-- name: DueForFreshness :many
-- Repositories using a baseline whose branch head was not looked at since
-- before cutoff, oldest first. Without automatic refresh the look only tells
-- the tab that the branch moved.
SELECT s.repo_id, coalesce(s.baseline_branch, '')::text AS baseline_branch, s.auto_refresh, r.github_repo_id, i.github_installation_id
FROM repository_sampledata_settings s
JOIN repos r ON r.id = s.repo_id JOIN installations i ON i.id = r.installation_id
WHERE s.enabled AND s.use_baseline AND r.accessible AND i.status = 'active' AND r.github_repo_id IS NOT NULL
  AND (s.checked_at IS NULL OR s.checked_at < sqlc.arg(cutoff)::timestamptz)
ORDER BY s.checked_at NULLS FIRST
LIMIT sqlc.arg(batch)::integer;

-- name: NextSequence :one
SELECT (coalesce(max(sequence), 0) + 1)::bigint FROM baseline_refreshes WHERE repo_id = $1;

-- name: PendingRefresh :one
SELECT * FROM baseline_refreshes WHERE repo_id = $1 AND state = 'pending' FOR UPDATE;

-- name: RunningRefresh :one
SELECT * FROM baseline_refreshes WHERE repo_id = $1 AND state = 'running' FOR UPDATE;

-- name: InsertRefresh :one
INSERT INTO baseline_refreshes (repo_id, sequence, trigger, requested_by, available_at)
VALUES (sqlc.arg(repo_id), sqlc.arg(sequence), sqlc.arg(trigger), sqlc.narg(requested_by), sqlc.arg(available_at))
RETURNING *;

-- name: HastenRefresh :exec
-- A person's request joins a waiting refresh and lifts the spacing that only
-- automatic refreshes keep.
UPDATE baseline_refreshes SET available_at = LEAST(available_at, sqlc.arg(available_at)::timestamptz),
    trigger = CASE WHEN sqlc.arg(trigger)::text IN ('button', 'enabled', 'branch') THEN sqlc.arg(trigger)::text ELSE trigger END,
    requested_by = coalesce(sqlc.narg(requested_by)::bigint, requested_by)
WHERE id = sqlc.arg(id);

-- name: LastAutomaticStart :one
-- When the repository's latest other automatic refresh (a merge, the daily
-- check, an unusable baseline) started, to space them; a refresh's own
-- retries are not spaced.
SELECT max(started_at)::timestamptz FROM baseline_refreshes
WHERE repo_id = sqlc.arg(repo_id) AND id <> sqlc.arg(refresh_id)
  AND trigger IN ('merged', 'daily', 'unusable') AND started_at IS NOT NULL;

-- name: ClaimRefresh :one
-- Starts a waiting refresh, or takes over one whose worker died (the run's
-- session lock is gone).
UPDATE baseline_refreshes SET state = 'running', started_at = clock_timestamp(), attempts = attempts + 1,
    waiting_reason = NULL, log_open = true, failure_code = NULL, failure_detail = NULL
WHERE id = $1 AND state IN ('pending', 'running')
RETURNING *;

-- name: SetRefreshTarget :exec
UPDATE baseline_refreshes SET branch = sqlc.arg(branch), commit_sha = sqlc.arg(commit_sha)
WHERE id = sqlc.arg(id) AND state = 'running';

-- name: WaitRefresh :exec
-- An admitted refresh that must wait (capacity or the daily allowance)
-- keeps its sequence and waits again.
UPDATE baseline_refreshes SET state = 'pending', waiting_reason = sqlc.narg(waiting_reason), available_at = sqlc.arg(available_at),
    log_open = false
WHERE id = sqlc.arg(id) AND state IN ('pending', 'running');

-- name: FinishRefresh :one
UPDATE baseline_refreshes SET state = sqlc.arg(state), failure_code = sqlc.narg(failure_code), failure_detail = sqlc.narg(failure_detail),
    build_seconds = sqlc.narg(build_seconds), finished_at = clock_timestamp(), log_open = false, waiting_reason = NULL
WHERE id = sqlc.arg(id) AND state IN ('pending', 'running')
RETURNING *;

-- name: CancelWaitingRefreshes :exec
UPDATE baseline_refreshes SET state = 'cancelled', finished_at = clock_timestamp(), log_open = false
WHERE repo_id = $1 AND state = 'pending';

-- name: GetRefresh :one
SELECT * FROM baseline_refreshes WHERE id = $1;

-- name: RefreshByNumber :one
SELECT * FROM baseline_refreshes WHERE repo_id = $1 AND sequence = $2;

-- name: LatestRefreshes :many
SELECT * FROM baseline_refreshes WHERE repo_id = $1 ORDER BY sequence DESC LIMIT $2;

-- name: RefreshesDue :many
-- Repositories whose refreshes need their job: a waiting refresh whose time
-- came, or a running one (a job lost with its worker is replaced).
SELECT repo_id FROM baseline_refreshes
WHERE (state = 'pending' AND available_at <= clock_timestamp()) OR state = 'running'
GROUP BY repo_id
ORDER BY min(available_at)
LIMIT $1;

-- name: PruneRefreshes :execrows
-- Keeps a repository's latest finished refreshes and their logs.
DELETE FROM baseline_refreshes b
WHERE b.state IN ('succeeded', 'failed', 'cancelled')
  AND b.id NOT IN (SELECT x.id FROM baseline_refreshes x WHERE x.repo_id = b.repo_id ORDER BY x.sequence DESC LIMIT sqlc.arg(keep)::integer)
  AND NOT EXISTS (SELECT 1 FROM repository_baselines rb WHERE rb.refresh_id = b.id AND rb.status = 'current');

-- name: CurrentBaselines :many
SELECT * FROM repository_baselines WHERE repo_id = $1 AND status = 'current' ORDER BY service_kind;

-- name: LockCurrentBaselines :many
SELECT * FROM repository_baselines WHERE repo_id = $1 AND status = 'current' ORDER BY service_kind FOR UPDATE;

-- name: InsertBaseline :one
INSERT INTO repository_baselines (repo_id, refresh_id, service_kind, service_version, branch, commit_sha, config_hash,
                                  refresh_sequence, storage_key, size_bytes, checksum, key_version, status, retired_at)
VALUES (sqlc.arg(repo_id), sqlc.arg(refresh_id), sqlc.arg(service_kind), sqlc.arg(service_version), sqlc.arg(branch), sqlc.arg(commit_sha),
        sqlc.narg(config_hash), sqlc.arg(refresh_sequence), sqlc.arg(storage_key), sqlc.arg(size_bytes), sqlc.arg(checksum), sqlc.arg(key_version),
        sqlc.arg(status), CASE WHEN sqlc.arg(status)::text = 'current' THEN NULL ELSE clock_timestamp() END)
RETURNING *;

-- name: RetireBaseline :exec
UPDATE repository_baselines SET status = 'retired', retired_at = clock_timestamp() WHERE id = $1 AND status <> 'retired';

-- name: RetireRepositoryBaselines :execrows
UPDATE repository_baselines SET status = 'retired', retired_at = clock_timestamp() WHERE repo_id = $1 AND status <> 'retired';

-- name: MarkBaselineMissing :execrows
UPDATE repository_baselines SET status = 'missing', retired_at = clock_timestamp() WHERE id = $1 AND status = 'current';

-- name: RetiredBaselines :many
-- Files whose baselines no candidate may still be restoring.
SELECT id, storage_key FROM repository_baselines
WHERE status <> 'current' AND retired_at < sqlc.arg(before)::timestamptz
ORDER BY id LIMIT sqlc.arg(batch)::integer;

-- name: DeleteBaseline :exec
DELETE FROM repository_baselines WHERE id = $1 AND status <> 'current';

-- name: StorageKeys :many
SELECT storage_key FROM repository_baselines;

-- name: RunningRefreshIDs :many
SELECT id FROM baseline_refreshes WHERE state = 'running';

-- name: KnownRefreshIDs :many
SELECT id FROM baseline_refreshes WHERE id = ANY(sqlc.arg(ids)::bigint[]) AND state IN ('pending', 'running');

-- name: MergeTarget :one
-- A repository's Test data settings by its GitHub identity, for a merge
-- webhook of its installation.
SELECT s.repo_id, s.enabled, s.use_baseline, s.auto_refresh, coalesce(s.baseline_branch, '')::text AS baseline_branch
FROM repository_sampledata_settings s
JOIN repos r ON r.id = s.repo_id JOIN installations i ON i.id = r.installation_id
WHERE r.github_repo_id = sqlc.arg(github_repo_id)::bigint AND i.github_installation_id = sqlc.arg(installation_id)::bigint;
