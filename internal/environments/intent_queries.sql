-- name: FindIntentScope :one
SELECT pr.repo_id, r.installation_id
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id WHERE pr.id = $1;

-- name: LockIntentInstallation :one
SELECT id FROM installations
WHERE id = sqlc.arg(installation_id) AND workspace_installation_access(id,sqlc.narg(user_id)::bigint,'read')
    AND status = 'active'
FOR UPDATE;

-- name: LockIntentRepository :one
SELECT id, build_mode, private_previews FROM repos
WHERE id = sqlc.arg(repo_id) AND installation_id = sqlc.arg(installation_id) AND accessible AND workspace_repository_access(id,sqlc.narg(user_id)::bigint,'operate')
FOR UPDATE;

-- name: LockIntentPR :one
-- untrusted: the code comes from a fork, a deleted head repository or another
-- repository than the base, the same rule that withholds secrets
-- (runtime TargetForUpdate). Automatic mode never builds it without the owner.
SELECT p.id, p.repo_id, p.generation, p.head_sha, p.state, p.build_status, p.needs_refresh,
 (p.is_fork OR p.head_repo_id IS NULL OR p.head_repo_id IS DISTINCT FROM r.github_repo_id)::boolean AS untrusted
FROM pull_requests p JOIN repos r ON r.id = p.repo_id
WHERE p.id = sqlc.arg(pr_id) AND p.repo_id = sqlc.arg(repo_id) FOR UPDATE OF p;

-- name: LockBuildIntent :one
SELECT * FROM build_intents WHERE pull_request_id = $1 FOR UPDATE;

-- name: ReadOwnedIntentView :one
SELECT pr.id, pr.generation, pr.head_sha, pr.state AS pr_state, r.build_mode,
    b.revision, b.base_revision, b.generation AS requested_generation,
    b.head_sha AS requested_sha, b.source, b.request_action, b.requested_by,
    b.idempotency_key, b.state AS intent_state, b.available_at, b.requested_at,
    b.started_at, b.completed_at
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE pr.id = sqlc.arg(pr_id) AND workspace_repository_access(r.id,sqlc.narg(user_id)::bigint,'read')
    AND i.status = 'active' AND r.accessible;

-- name: ReplaceBuildIntent :one
-- start_source is a Reset test data request's chosen starting data
-- (docs/32-persistent-test-data.md); NULL for every other request.
INSERT INTO build_intents(pull_request_id, revision, base_revision, generation,
    head_sha, source, request_action, requested_by, idempotency_key, state, available_at, start_source)
SELECT pr.id, sqlc.arg(base_revision)::bigint + 1, sqlc.arg(base_revision)::bigint,
    pr.generation, pr.head_sha, sqlc.arg(source)::text, sqlc.arg(request_action)::text,
    sqlc.narg(requested_by)::bigint, sqlc.arg(idempotency_key)::text, 'pending',
    COALESCE(sqlc.narg(available_at)::timestamptz, clock_timestamp()), sqlc.narg(start_source)::text
FROM pull_requests pr
WHERE pr.id = sqlc.arg(pr_id) AND pr.generation = sqlc.arg(generation)
    AND pr.head_sha = sqlc.arg(head_sha) AND pr.state = 'open'
ON CONFLICT (pull_request_id) DO UPDATE
SET revision = build_intents.revision + 1, base_revision = EXCLUDED.base_revision,
    generation = EXCLUDED.generation, head_sha = EXCLUDED.head_sha,
    source = EXCLUDED.source, request_action = EXCLUDED.request_action,
    requested_by = EXCLUDED.requested_by, idempotency_key = EXCLUDED.idempotency_key,
    state = 'pending', available_at = EXCLUDED.available_at, start_source = EXCLUDED.start_source,
    requested_at = clock_timestamp(), updated_at = clock_timestamp(),
    started_at = NULL, completed_at = NULL, execution_job_id=NULL, execution_error_code=''
WHERE build_intents.revision = sqlc.arg(base_revision)::bigint
RETURNING *;

-- name: SupersedeBuildIntent :execrows
UPDATE build_intents SET state = 'superseded', completed_at = clock_timestamp(),
    updated_at = clock_timestamp()
WHERE pull_request_id = $1 AND state IN ('pending', 'queued', 'running');

-- name: ClearLocalTeardownForBuild :exec
-- A fresh owner-authorized build restores the preview after local teardown.
-- Keep the completed-generation fence for old teardown jobs; clear the active
-- decision so reports, expiry and subsequent owner teardowns see the new build.
UPDATE pull_requests SET teardown_requested_generation = -1, teardown_reason = NULL
WHERE id = $1 AND state = 'open' AND teardown_requested_generation = generation;

-- name: AdvanceBuildIntent :one
UPDATE build_intents b SET state = sqlc.arg(next_state), updated_at = clock_timestamp(),
    started_at = CASE WHEN sqlc.arg(next_state)::text = 'running' THEN clock_timestamp() ELSE b.started_at END,
    completed_at = CASE WHEN sqlc.arg(next_state)::text IN ('succeeded', 'failed') THEN clock_timestamp() ELSE NULL END
FROM pull_requests pr
WHERE b.pull_request_id = pr.id AND pr.id = sqlc.arg(pr_id)
    AND pr.state = 'open' AND pr.generation = sqlc.arg(generation)
    AND b.generation = pr.generation AND b.head_sha = pr.head_sha
    AND b.revision = sqlc.arg(revision) AND b.state = sqlc.arg(previous_state)
    AND (sqlc.arg(next_state)::text <> 'running' OR b.available_at <= clock_timestamp())
RETURNING b.*;

-- name: ReadBuildIntent :one
SELECT * FROM build_intents WHERE pull_request_id = $1;
