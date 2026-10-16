-- name: SetupRepository :one
-- The repository a person sets up: only for members who can build in it.
SELECT r.id, r.full_name, r.accessible, r.build_mode, coalesce(r.github_repo_id, 0)::bigint AS github_repo_id,
 i.status AS installation_status, i.github_installation_id, coalesce(i.workspace_id, 0)::bigint AS workspace_id,
 workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'manage')::boolean AS can_manage
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = sqlc.arg(repo_id)::bigint AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate');

-- name: OpenPullRequests :many
-- The open pull requests a person can choose from, most recently updated first.
SELECT p.number, p.title, p.head_branch, p.head_sha, p.is_fork
FROM pull_requests p
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.state = 'open'
ORDER BY p.source_updated_at DESC NULLS LAST, p.number DESC
LIMIT sqlc.arg(row_limit)::integer;

-- name: CountOpenPullRequests :one
SELECT count(*) FROM pull_requests WHERE repo_id = sqlc.arg(repo_id)::bigint AND state = 'open';

-- name: LatestSetupPullRequest :one
-- The open pull request whose setup was checked most recently, to resume there.
SELECT p.number FROM repository_setup_checks c JOIN pull_requests p ON p.id = c.pull_request_id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.state = 'open'
ORDER BY c.requested_at DESC LIMIT 1;

-- name: SyncState :one
-- Whether the repository's pull request list is being refreshed from GitHub.
SELECT state, requested_at, last_success_at FROM pr_syncs WHERE repo_id = sqlc.arg(repo_id)::bigint;

-- name: SetupPullRequest :one
-- One pull request's setup facts: its head, its current build request, what
-- is deployed and its latest setup check.
SELECT p.id, p.number, p.title, p.head_branch, p.head_sha, p.generation, p.state, p.is_fork, p.build_status, p.needs_refresh,
 (p.mergeability_unresolved_at IS NOT NULL)::boolean AS mergeability_unresolved,
 (p.teardown_requested_generation = p.generation)::boolean AS teardown_requested,
 coalesce(b.state, '')::text AS build_state, coalesce(b.head_sha, '')::text AS requested_sha,
 coalesce(b.generation = p.generation AND b.head_sha = p.head_sha, false)::boolean AS request_current,
 coalesce(b.revision, 0)::bigint AS request_revision, coalesce(b.source, '')::text AS request_source,
 coalesce(b.execution_error_code, '')::text AS execution_error_code,
 coalesce(CASE WHEN b.waiting_revision = b.revision THEN b.waiting_reason END, '')::text AS waiting_reason,
 b.started_at AS build_started_at, b.completed_at AS build_completed_at,
 coalesce(e.status, '')::text AS environment_status, coalesce(e.head_sha, '')::text AS deployed_sha,
 coalesce(e.subdomain, '')::text AS hostname, e.last_active_at AS published_at,
 coalesce(e.last_error_code, '')::text AS environment_error,
 coalesce(e.applied_generation = b.generation AND e.applied_revision = b.revision, false)::boolean AS failure_current,
 coalesce(c.revision, 0)::bigint AS check_revision, coalesce(c.head_sha, '')::text AS check_sha,
 coalesce(c.state, '')::text AS check_state, coalesce(c.failure_code, '')::text AS check_failure,
 c.requested_at AS check_requested_at, c.finished_at AS check_finished_at,
 coalesce(c.result_sha, '')::text AS result_sha, c.result, c.checked_at, c.expires_at,
 coalesce(c.platform, '')::text AS check_platform
FROM pull_requests p
LEFT JOIN build_intents b ON b.pull_request_id = p.id
LEFT JOIN environments e ON e.pull_request_id = p.id
LEFT JOIN repository_setup_checks c ON c.pull_request_id = p.id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.number = sqlc.arg(number)::integer;

-- name: LatestOpen :one
-- Who last opened this deployment from Previa, preferring the viewer.
SELECT o.user_id, o.opened_at, u.username,
 coalesce(nullif(trim(u.first_name || ' ' || u.last_name), ''), u.username)::text AS display_name
FROM preview_opens o JOIN users u ON u.id = o.user_id
WHERE o.pull_request_id = sqlc.arg(pull_request_id)::bigint AND o.deployed_sha = sqlc.arg(deployed_sha)::text
ORDER BY (o.user_id = sqlc.arg(viewer_id)::bigint) DESC, o.opened_at DESC
LIMIT 1;

-- name: WorkspaceManagers :many
-- The people who manage secrets and connections, for members who cannot.
SELECT u.username, coalesce(nullif(trim(u.first_name || ' ' || u.last_name), ''), u.username)::text AS display_name, m.role
FROM workspace_memberships m JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = sqlc.arg(workspace_id)::bigint AND m.state = 'active' AND m.role IN ('owner', 'admin')
ORDER BY (m.role = 'owner') DESC, lower(u.username)
LIMIT 4;

-- name: CheckTarget :one
-- The pull request a person asks to check, if they can build in its repository.
SELECT p.id, p.head_sha, p.generation, p.state, r.accessible, i.status AS installation_status,
 coalesce(i.workspace_id, 0)::bigint AS workspace_id
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.number = sqlc.arg(number)::integer
 AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate');

-- name: LockCheck :one
SELECT id, revision, head_sha, state, requested_at, result_sha, checked_at
FROM repository_setup_checks WHERE pull_request_id = sqlc.arg(pull_request_id)::bigint FOR UPDATE;

-- name: SpendCheckBudget :one
-- Counts one check against the workspace's hourly allowance; no row when the
-- allowance is spent.
INSERT INTO setup_check_budgets (workspace_id, window_started_at, used)
VALUES (sqlc.arg(workspace_id)::bigint, clock_timestamp(), 1)
ON CONFLICT (workspace_id) DO UPDATE SET
 window_started_at = CASE WHEN setup_check_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
  THEN clock_timestamp() ELSE setup_check_budgets.window_started_at END,
 used = CASE WHEN setup_check_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
  THEN 1 ELSE setup_check_budgets.used + 1 END
WHERE setup_check_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
 OR setup_check_budgets.used < sqlc.arg(allowance)::integer
RETURNING used;

-- name: CheckBudgetReset :one
SELECT (window_started_at + interval '1 hour')::timestamptz AS resets_at FROM setup_check_budgets
WHERE workspace_id = sqlc.arg(workspace_id)::bigint;

-- name: RequestCheck :one
-- A new request replaces any earlier one; the last completed result stays.
INSERT INTO repository_setup_checks (pull_request_id, head_sha, generation, state, requested_by)
VALUES (sqlc.arg(pull_request_id)::bigint, sqlc.arg(head_sha)::text, sqlc.arg(generation)::bigint, 'queued', sqlc.narg(requested_by)::bigint)
ON CONFLICT (pull_request_id) DO UPDATE SET revision = repository_setup_checks.revision + 1,
 head_sha = EXCLUDED.head_sha, generation = EXCLUDED.generation, state = 'queued', failure_code = '',
 requested_by = EXCLUDED.requested_by, requested_at = clock_timestamp(), started_at = NULL, finished_at = NULL
RETURNING id, revision;

-- name: ClaimCheck :one
-- A worker's view of one check and the scope its result must still match.
SELECT c.id, c.revision, c.state, c.head_sha, c.generation, c.requested_at,
 p.head_sha AS current_sha, p.generation AS current_generation, p.state AS pr_state, p.is_fork,
 r.full_name, coalesce(r.github_repo_id, 0)::bigint AS github_repo_id, r.accessible,
 i.status AS installation_status, i.github_installation_id
FROM repository_setup_checks c
JOIN pull_requests p ON p.id = c.pull_request_id
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE c.id = sqlc.arg(id)::bigint
FOR UPDATE OF c;

-- name: StartCheck :exec
UPDATE repository_setup_checks SET state = 'running', started_at = clock_timestamp()
WHERE id = sqlc.arg(id)::bigint AND revision = sqlc.arg(revision)::bigint AND state IN ('queued', 'running');

-- name: CompleteCheck :execrows
-- Records a result only while it still describes the pull request: the same
-- request, head and generation, an open pull request and active access.
UPDATE repository_setup_checks c SET state = 'complete', failure_code = '', finished_at = clock_timestamp(),
 result_sha = c.head_sha, result = sqlc.arg(result)::jsonb, checked_at = clock_timestamp(),
 expires_at = clock_timestamp() + make_interval(secs => sqlc.arg(fresh_seconds)::integer), platform = sqlc.arg(platform)::text
FROM pull_requests p, repos r, installations i
WHERE c.id = sqlc.arg(id)::bigint AND c.revision = sqlc.arg(revision)::bigint AND c.state = 'running'
 AND p.id = c.pull_request_id AND r.id = p.repo_id AND i.id = r.installation_id
 AND p.head_sha = c.head_sha AND p.generation = c.generation AND p.state = 'open'
 AND r.accessible AND i.status = 'active';

-- name: FailCheck :execrows
UPDATE repository_setup_checks SET state = 'failed', failure_code = sqlc.arg(code)::text, finished_at = clock_timestamp()
WHERE id = sqlc.arg(id)::bigint AND revision = sqlc.arg(revision)::bigint AND state IN ('queued', 'running');

-- name: OpenTarget :one
-- The deployment behind a pull request's Open preview action.
SELECT p.id, p.state, coalesce(e.status, '')::text AS environment_status,
 coalesce(e.head_sha, '')::text AS deployed_sha, coalesce(e.subdomain, '')::text AS hostname
FROM pull_requests p JOIN repos r ON r.id = p.repo_id
LEFT JOIN environments e ON e.pull_request_id = p.id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.number = sqlc.arg(number)::integer
 AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'read');

-- name: RecordOpen :exec
INSERT INTO preview_opens (pull_request_id, user_id, deployed_sha)
VALUES (sqlc.arg(pull_request_id)::bigint, sqlc.arg(user_id)::bigint, sqlc.arg(deployed_sha)::text)
ON CONFLICT (pull_request_id, user_id) DO UPDATE SET deployed_sha = EXCLUDED.deployed_sha, opened_at = clock_timestamp();

-- name: FocusRepository :one
-- Where a person's setup continues: the repository they (or a teammate)
-- checked most recently, else one with an open pull request.
SELECT r.id, r.full_name FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE i.workspace_id = sqlc.arg(workspace_id)::bigint AND r.accessible AND i.status = 'active'
 AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate')
ORDER BY (SELECT max(c.requested_at) FROM repository_setup_checks c JOIN pull_requests p ON p.id = c.pull_request_id
           WHERE p.repo_id = r.id) DESC NULLS LAST,
 EXISTS (SELECT 1 FROM pull_requests p WHERE p.repo_id = r.id AND p.state = 'open') DESC,
 lower(r.full_name)
LIMIT 1;

-- name: Journey :one
-- How far the workspace's repositories that this person can build in have
-- come: a commit that passed its setup check or built, a published preview,
-- and a preview opened from Previa.
SELECT
 count(*) FILTER (WHERE r.accessible AND i.status = 'active')::bigint AS available,
 count(*)::bigint AS repositories,
 coalesce(bool_or(EXISTS (
  SELECT 1 FROM repository_setup_checks c JOIN pull_requests p ON p.id = c.pull_request_id
  WHERE p.repo_id = r.id AND p.state = 'open' AND c.result_sha = p.head_sha AND c.result ->> 'status' = 'passed'
 )), false)::boolean AS checked,
 coalesce(bool_or(EXISTS (
  SELECT 1 FROM environments e JOIN pull_requests p ON p.id = e.pull_request_id
  WHERE p.repo_id = r.id AND e.head_sha IS NOT NULL
 )), false)::boolean AS published,
 coalesce(bool_or(EXISTS (
  SELECT 1 FROM preview_opens o JOIN pull_requests p ON p.id = o.pull_request_id WHERE p.repo_id = r.id
 )), false)::boolean AS opened
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE i.workspace_id = sqlc.arg(workspace_id)::bigint AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate');
