-- name: LockRequest :one
SELECT id, head_sha, state, generation, teardown_completed_generation,build_status,needs_refresh FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: ReadRequest :one
SELECT id, head_sha, state, generation, teardown_completed_generation,build_status,needs_refresh FROM pull_requests WHERE id = $1;

-- name: ReadEnvironment :one
SELECT container_id, image_tag, head_sha, status, applied_generation,
       pending_container_id, pending_image_tag, pending_generation, pending_head_sha, applied_revision, pending_revision,
       teardown_generation
FROM environments WHERE pull_request_id = $1 ORDER BY id DESC LIMIT 1;

-- name: ListProtectingOperationPayloads :many
SELECT o.payload FROM environment_operations o JOIN pull_requests pr ON pr.id = o.pull_request_id
WHERE o.pull_request_id = $1
  AND ((o.state = 'running' AND o.lease_expires_at > now()) OR (o.generation = pr.generation
    AND (pr.state <> 'open' OR pr.teardown_requested_generation = pr.generation
      OR EXISTS (SELECT 1 FROM build_intents b WHERE b.pull_request_id = pr.id
      AND b.revision = o.revision AND b.state IN ('running','succeeded')))))
  AND o.kind IN ('activate_route', 'remove_route')
  -- A failed route operation protects nothing: whatever the router may serve
  -- is protected by the route row itself.
  AND o.state IN ('pending', 'running', 'retryable')
ORDER BY o.id;

-- name: ApplyResult :one
-- Lock the source-of-truth row, not only the environment. PostgreSQL rechecks
-- the generation predicate after waiting for a concurrent event's row lock.
-- Keep this lock only through the state transition and durable operation enqueue.
-- External network IO is always performed by a separate River worker.
WITH current_request AS MATERIALIZED (
    SELECT pr.id FROM pull_requests pr
    WHERE pr.id = sqlc.arg(pr_id) AND pr.generation = sqlc.arg(expected_generation)
      AND pr.head_sha = sqlc.arg(head_sha)
      AND ((pr.state IN ('closed','merged') AND sqlc.arg(expected_revision)::bigint = 0)
        -- A local teardown (revocation, owner request, expiry) of an open PR.
        OR (pr.teardown_requested_generation = pr.generation
          AND sqlc.arg(expected_revision)::bigint = 0 AND sqlc.arg(result_status)::text = 'torn_down')
        OR (pr.state = 'open' AND pr.build_status='open' AND NOT pr.needs_refresh AND EXISTS (
          SELECT 1 FROM build_intents b WHERE b.pull_request_id = pr.id
            AND b.generation = pr.generation AND b.head_sha = pr.head_sha
            AND b.revision = sqlc.arg(expected_revision)
            AND b.state IN ('running','succeeded')
        )))
    FOR UPDATE
), previous AS MATERIALIZED (
    SELECT e.* FROM environments e
    JOIN current_request p ON p.id = e.pull_request_id
    FOR UPDATE OF e
)
UPDATE environments e
SET status = CASE sqlc.arg(result_status)::text
        WHEN 'running' THEN 'publishing'
        WHEN 'torn_down' THEN 'tearing_down'
        ELSE 'failed' END,
    applied_generation = CASE WHEN sqlc.arg(result_status)::text = 'failed'
        THEN sqlc.arg(expected_generation)::bigint ELSE e.applied_generation END,
    applied_revision = CASE WHEN sqlc.arg(result_status)::text = 'failed'
        THEN sqlc.arg(expected_revision)::bigint ELSE e.applied_revision END,
    -- Teardown hands the active artifact to the route-removal operation; the
    -- route row and that operation protect it until the removal converges.
    container_id = CASE WHEN sqlc.arg(result_status)::text = 'torn_down'
        THEN NULL ELSE e.container_id END,
    image_tag = CASE WHEN sqlc.arg(result_status)::text = 'torn_down'
        THEN NULL ELSE e.image_tag END,
    head_sha = CASE WHEN sqlc.arg(result_status)::text = 'torn_down'
        THEN NULL ELSE e.head_sha END,
    teardown_generation = CASE WHEN sqlc.arg(result_status)::text = 'torn_down'
        THEN sqlc.arg(expected_generation)::bigint ELSE e.teardown_generation END,
    expires_at = CASE WHEN sqlc.arg(result_status)::text = 'torn_down'
        THEN NULL ELSE e.expires_at END,
    last_error_code = CASE WHEN sqlc.arg(result_status)::text = 'failed'
        THEN sqlc.narg(error_code)::text ELSE e.last_error_code END,
    pending_revision = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.arg(expected_revision)::bigint
        WHEN sqlc.arg(result_status)::text = 'failed' THEN e.pending_revision ELSE NULL END,
    pending_container_id = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.narg(container_id)::text
        WHEN sqlc.arg(result_status)::text = 'failed' THEN e.pending_container_id
        ELSE NULL END,
    pending_image_tag = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.narg(image_tag)::text
        WHEN sqlc.arg(result_status)::text = 'failed' THEN e.pending_image_tag
        ELSE NULL END,
    pending_generation = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.arg(expected_generation)::bigint
        WHEN sqlc.arg(result_status)::text = 'failed' THEN e.pending_generation
        ELSE NULL END,
    pending_head_sha = CASE WHEN sqlc.arg(result_status)::text = 'running'
        THEN sqlc.arg(head_sha)::text
        WHEN sqlc.arg(result_status)::text = 'failed' THEN e.pending_head_sha
        ELSE NULL END
FROM previous
WHERE e.id = previous.id
RETURNING previous.container_id, previous.image_tag,
          previous.pending_container_id, previous.pending_image_tag,
          previous.pending_generation, previous.pending_head_sha;

-- name: CreateOperation :one
INSERT INTO environment_operations (
    pull_request_id, generation, revision, kind, idempotency_key, payload
)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- name: GetOperation :one
SELECT *
FROM environment_operations WHERE id = $1;

-- name: LockCurrentOperationLease :one
SELECT id FROM environment_operations
WHERE id = sqlc.arg(id)
  AND state = 'running'
  AND lease_owner = sqlc.arg(lease_owner)
  AND lease_expires_at > now()
FOR UPDATE;

-- name: FindOperationByKey :one
SELECT *
FROM environment_operations WHERE idempotency_key = $1;

-- name: ClaimOperation :one
UPDATE environment_operations
SET state = 'running', attempts = attempts + 1, lease_owner = $2,
    lease_expires_at = now() + sqlc.arg(lease_duration)::interval,
    updated_at = now(), last_error_code = NULL
WHERE id = $1 AND available_at <= now()
  AND (state IN ('pending', 'retryable')
       OR (state = 'running' AND (lease_expires_at <= now() OR lease_owner = $2)))
RETURNING *;

-- name: MarkOperationRetryable :execrows
UPDATE environment_operations
SET state = 'retryable',
    available_at = now() + sqlc.arg(backoff)::interval,
    lease_owner = NULL,
    lease_expires_at = NULL, last_error_code = sqlc.arg(last_error_code), updated_at = now()
WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND lease_expires_at > now();

-- name: MarkOperationSucceeded :execrows
UPDATE environment_operations
SET state = 'succeeded', lease_owner = NULL, lease_expires_at = NULL,
    completed_at = now(), updated_at = now(), last_error_code = NULL
WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND lease_expires_at > now();

-- name: MarkOperationSuperseded :execrows
UPDATE environment_operations
SET state = 'superseded', lease_owner = NULL, lease_expires_at = NULL,
    completed_at = now(), updated_at = now(), last_error_code = NULL
WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND lease_expires_at > now();

-- name: MarkOperationFailed :execrows
UPDATE environment_operations
SET state = 'failed', lease_owner = NULL, lease_expires_at = NULL,
    completed_at = now(), updated_at = now(), last_error_code = $3
WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND lease_expires_at > now();

-- name: ListOperationsForRecovery :many
SELECT *
FROM environment_operations
WHERE (state IN ('pending', 'retryable') AND available_at <= now())
   OR (state = 'running' AND lease_expires_at <= now())
ORDER BY available_at, id
LIMIT sqlc.arg(batch_size);

-- name: ReleaseExpiredOperationLease :execrows
UPDATE environment_operations
SET state = 'retryable', lease_owner = NULL, lease_expires_at = NULL,
    updated_at = now(), last_error_code = 'lease_expired'
WHERE id = $1 AND state = 'running' AND lease_expires_at <= now();

-- name: ListPullRequestsForReconciliation :many
SELECT id, head_sha, state, generation
FROM pull_requests
WHERE id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(batch_size);

-- name: ActivatePendingEnvironment :one
WITH locked AS MATERIALIZED (
    SELECT e.id, e.container_id, e.image_tag,
        -- When the request could first build (after Automatic's quiet period):
        -- the start of its request-to-preview latency.
        (SELECT b.available_at FROM build_intents b
         WHERE b.pull_request_id = pr.id AND b.revision = sqlc.arg(expected_revision)) AS eligible_at
    FROM environments e
    JOIN pull_requests pr ON pr.id = e.pull_request_id
    WHERE e.pull_request_id = sqlc.arg(pr_id)
      AND pr.generation = sqlc.arg(expected_generation)
      AND pr.state = 'open' AND pr.build_status='open' AND NOT pr.needs_refresh
      AND EXISTS (SELECT 1 FROM build_intents b WHERE b.pull_request_id = pr.id
        AND b.generation = pr.generation AND b.revision = sqlc.arg(expected_revision)
        AND b.state IN ('running','succeeded') AND b.head_sha = pr.head_sha)
      AND e.pending_revision = sqlc.arg(expected_revision)
      AND e.pending_generation = sqlc.arg(expected_generation)
      AND e.pending_container_id IS NOT NULL
      AND e.pending_image_tag IS NOT NULL
    FOR UPDATE OF pr, e
)
UPDATE environments e
SET status = 'running',
    applied_generation = sqlc.arg(expected_generation),
    applied_revision = sqlc.arg(expected_revision),
    container_id = e.pending_container_id,
    image_tag = e.pending_image_tag,
    head_sha = e.pending_head_sha,
    pending_container_id = NULL,
    pending_image_tag = NULL,
    pending_generation = NULL,
    pending_revision = NULL,
    pending_head_sha = NULL,
    last_error_code = NULL,
    last_active_at = now(),
    -- Each publication restarts the preview's lifetime; a failed attempt does
    -- not. A zero lifetime means previews never expire.
    expires_at = CASE WHEN sqlc.arg(ttl_seconds)::bigint > 0
        THEN now() + sqlc.arg(ttl_seconds)::bigint * interval '1 second' END
FROM locked
WHERE e.id = locked.id
RETURNING locked.container_id AS previous_container_id,
          locked.image_tag AS previous_image_tag,
          e.container_id AS active_container_id,
          e.image_tag AS active_image_tag,
          e.head_sha AS active_head_sha,
          locked.eligible_at::timestamptz AS eligible_at;

-- name: RecordPublication :one
-- The immutable reference to a candidate that just started serving
-- (docs/30-reviews.md), written in its activating transaction under the PR
-- lock, which also serializes the per-PR sequence. A retried activation of
-- the same candidate records nothing new.
INSERT INTO preview_publications (pull_request_id, repo_id, workspace_id, sequence, head_sha, generation, revision,
                                  container_id, image_tag, config_hash)
SELECT p.id, p.repo_id, i.workspace_id,
       coalesce((SELECT max(x.sequence) FROM preview_publications x WHERE x.pull_request_id = p.id), 0) + 1,
       sqlc.arg(head_sha)::text, sqlc.arg(generation)::bigint, sqlc.arg(revision)::bigint,
       sqlc.narg(container_id)::text, sqlc.narg(image_tag)::text,
       (SELECT c.config_hash FROM preview_components c
        WHERE c.pull_request_id = p.id AND c.generation = sqlc.arg(generation)::bigint AND c.revision = sqlc.arg(revision)::bigint)
FROM pull_requests p
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE p.id = sqlc.arg(pull_request_id)::bigint AND i.workspace_id IS NOT NULL
ON CONFLICT (pull_request_id, generation, revision) DO NOTHING
RETURNING id, sequence;

-- name: FinalizePendingTeardown :one
-- Completes the teardown this route removal belongs to, once the removal
-- converged. A newer teardown or a newer publication owns the environment
-- otherwise, and is left alone; the PR itself may have moved on (reopened).
WITH locked AS MATERIALIZED (
    SELECT e.id, e.container_id, e.image_tag,
           e.pending_container_id, e.pending_image_tag
    FROM environments e
    WHERE e.pull_request_id = sqlc.arg(pr_id)
      AND sqlc.arg(expected_revision)::bigint = 0
      AND e.status = 'tearing_down'
      AND e.teardown_generation = sqlc.arg(expected_generation)
    FOR UPDATE OF e
)
UPDATE environments e
SET status = 'torn_down',
    applied_generation = sqlc.arg(expected_generation),
    applied_revision = sqlc.arg(expected_revision),
    expires_at = NULL,
    container_id = NULL,
    image_tag = NULL,
    head_sha = NULL,
    pending_container_id = NULL,
    pending_image_tag = NULL,
    pending_generation = NULL,
    pending_revision = NULL,
    pending_head_sha = NULL
FROM locked
WHERE e.id = locked.id
RETURNING locked.container_id, locked.image_tag,
          locked.pending_container_id, locked.pending_image_tag;

-- name: RearmCleanupOperation :execrows
-- Cleanup is idempotent: a cleanup that failed is retried when the same
-- resource is scheduled again, rather than being left behind forever.
UPDATE environment_operations
SET state = 'pending', available_at = now(), updated_at = now(), completed_at = NULL
WHERE id = $1 AND kind = 'cleanup_artifact' AND state = 'failed';

-- name: MarkActiveMissing :execrows
-- The active preview's container no longer exists in Docker. The environment
-- records a failure without a deployed preview, so reports stop calling it
-- ready; only the container housekeeping saw missing, still active, is affected.
UPDATE environments
SET status = 'failed', last_error_code = 'preview_missing', container_id = NULL, image_tag = NULL, head_sha = NULL, expires_at = NULL
WHERE pull_request_id = sqlc.arg(pr_id) AND status = 'running' AND container_id = sqlc.arg(container_id)::text;
