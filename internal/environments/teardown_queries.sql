-- Teardown requests and the sweep that starts them. A teardown never needs
-- GitHub access: these locks follow installation -> repository -> PR order
-- without requiring the installation to be active or the repository accessible.

-- name: LockScopeInstallation :one
SELECT id FROM installations WHERE id = $1 FOR UPDATE;

-- name: LockScopeRepository :one
SELECT id FROM repos WHERE id = $1 FOR UPDATE;

-- name: LockTeardownRequest :one
SELECT id, head_sha, state, generation, teardown_requested_generation,
       teardown_completed_generation
FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: RequestTeardown :one
-- Advancing the generation fences every build, publication and report of an
-- earlier generation; the request is complete once teardown starts.
UPDATE pull_requests
SET generation = generation + 1, teardown_requested_generation = generation + 1,
    teardown_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id)
RETURNING id, generation, head_sha, state;

-- name: RequestRevocationTeardowns :many
-- Revocation requests teardown of every PR in the revoked repositories that
-- still holds a preview or an unfinished build request. Callers hold the
-- installation's row lock.
WITH revoked AS MATERIALIZED (
    SELECT pr.id FROM pull_requests pr
    JOIN repos r ON r.id = pr.repo_id
    JOIN installations i ON i.id = r.installation_id
    WHERE i.github_installation_id = sqlc.arg(github_installation_id)
      AND (sqlc.arg(all_repositories)::boolean
        OR r.github_repo_id = ANY(sqlc.arg(github_repo_ids)::bigint[]))
      AND (EXISTS (SELECT 1 FROM environments e WHERE e.pull_request_id = pr.id
              AND e.status NOT IN ('tearing_down', 'torn_down'))
        OR EXISTS (SELECT 1 FROM build_intents b WHERE b.pull_request_id = pr.id
              AND b.state IN ('pending', 'queued', 'running')))
    ORDER BY pr.id
    FOR UPDATE OF pr
)
UPDATE pull_requests pr
SET generation = pr.generation + 1, teardown_requested_generation = pr.generation + 1,
    teardown_reason = 'revoked'
FROM revoked
WHERE pr.id = revoked.id
RETURNING pr.id;

-- name: RequestInaccessibleTeardowns :many
-- The sweep's catch-all: PRs that are revoked now, however access was lost.
WITH revoked AS MATERIALIZED (
    SELECT pr.id FROM pull_requests pr
    JOIN repos r ON r.id = pr.repo_id
    JOIN installations i ON i.id = r.installation_id
    WHERE i.id = sqlc.arg(installation_id) AND (NOT r.accessible OR i.status <> 'active')
      AND (EXISTS (SELECT 1 FROM environments e WHERE e.pull_request_id = pr.id
              AND e.status NOT IN ('tearing_down', 'torn_down'))
        OR EXISTS (SELECT 1 FROM build_intents b WHERE b.pull_request_id = pr.id
              AND b.state IN ('pending', 'queued', 'running')))
    ORDER BY pr.id
    FOR UPDATE OF pr
)
UPDATE pull_requests pr
SET generation = pr.generation + 1, teardown_requested_generation = pr.generation + 1,
    teardown_reason = 'revoked'
FROM revoked
WHERE pr.id = revoked.id
RETURNING pr.id;

-- name: SupersedeIntents :execrows
UPDATE build_intents
SET state = 'superseded', completed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE pull_request_id = ANY(sqlc.arg(pull_request_ids)::bigint[])
  AND state IN ('pending', 'queued', 'running');

-- name: ListUnstartedTeardowns :many
SELECT id FROM pull_requests
WHERE teardown_requested_generation = generation
  AND teardown_completed_generation < generation
  AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(batch_size);

-- name: ListCleanupPendingInstallations :many
SELECT id FROM installations
WHERE cleanup_pending AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(batch_size);

-- name: LockCleanupInstallation :one
SELECT id, revision, cleanup_pending FROM installations WHERE id = $1 FOR UPDATE;

-- name: CountInstallationTeardownWork :one
-- Work that keeps an installation's cleanup marker set: teardowns not yet
-- started, environments still live in a revoked scope or still tearing down,
-- and route removals or cleanups that have not succeeded.
SELECT
    (SELECT count(*) FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
     WHERE r.installation_id = sqlc.arg(installation_id)
       AND pr.teardown_requested_generation = pr.generation
       AND pr.teardown_completed_generation < pr.generation)::bigint AS unstarted,
    (SELECT count(*) FROM environments e
     JOIN pull_requests pr ON pr.id = e.pull_request_id
     JOIN repos r ON r.id = pr.repo_id
     JOIN installations i ON i.id = r.installation_id
     WHERE r.installation_id = sqlc.arg(installation_id)
       AND (e.status = 'tearing_down'
         OR ((NOT r.accessible OR i.status <> 'active') AND e.status <> 'torn_down')))::bigint AS live,
    (SELECT count(*) FROM environment_operations o
     JOIN pull_requests pr ON pr.id = o.pull_request_id
     JOIN repos r ON r.id = pr.repo_id
     WHERE r.installation_id = sqlc.arg(installation_id)
       AND o.kind IN ('remove_route', 'cleanup_artifact')
       AND o.state IN ('pending', 'running', 'retryable', 'failed'))::bigint AS operations;

-- name: ListRevokedEnvironmentPRs :many
SELECT pr.id FROM pull_requests pr
JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE r.installation_id = sqlc.arg(installation_id) AND (NOT r.accessible OR i.status <> 'active')
  AND EXISTS (SELECT 1 FROM environments e WHERE e.pull_request_id = pr.id)
ORDER BY pr.id
LIMIT sqlc.arg(batch_size);

-- name: ClearInstallationCleanup :execrows
-- The revision guard keeps a lifecycle event that arrived during the sweep.
UPDATE installations SET cleanup_pending = false
WHERE id = sqlc.arg(id) AND revision = sqlc.arg(revision) AND cleanup_pending;
