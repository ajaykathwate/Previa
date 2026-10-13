-- Route controller state. Acceptance locks the PR scope first (installation,
-- repository, PR), then the environment, the route row and finally the
-- route-table counter, so acceptances serialize in commit order.

-- name: LockRouteEnvironment :one
SELECT id, subdomain, status, pending_generation, pending_revision,
       pending_container_id, pending_image_tag
FROM environments WHERE pull_request_id = $1 FOR UPDATE;

-- name: LockRoute :one
SELECT * FROM environment_routes WHERE environment_id = $1 FOR UPDATE;

-- name: NextRouteTableVersion :one
UPDATE route_table SET version = version + 1 WHERE singleton RETURNING version;

-- name: SaveActiveRoute :execrows
-- The WHERE clause repeats the caller's locked check: a route version only
-- ever moves forward in (generation, revision) order.
INSERT INTO environment_routes (
    environment_id, pull_request_id, generation, revision, state, container_id,
    image_tag, backend, port, network_id, table_version, operation_id
) VALUES (
    sqlc.arg(environment_id), sqlc.arg(pull_request_id), sqlc.arg(generation),
    sqlc.arg(revision), 'active', sqlc.arg(container_id), sqlc.arg(image_tag),
    sqlc.arg(backend), sqlc.arg(port), sqlc.arg(network_id),
    sqlc.arg(table_version), sqlc.arg(operation_id)
)
ON CONFLICT (environment_id) DO UPDATE
SET generation = EXCLUDED.generation, revision = EXCLUDED.revision, state = 'active',
    unconfirmed_targets = CASE
        WHEN environment_routes.container_id IS NULL
          OR (environment_routes.served_generation, environment_routes.served_revision)
             IS NOT DISTINCT FROM (environment_routes.generation, environment_routes.revision)
        THEN environment_routes.unconfirmed_targets
        ELSE environment_routes.unconfirmed_targets || jsonb_build_array(jsonb_build_object(
            'container_id', environment_routes.container_id, 'image_tag', environment_routes.image_tag))
    END,
    container_id = EXCLUDED.container_id, image_tag = EXCLUDED.image_tag,
    backend = EXCLUDED.backend, port = EXCLUDED.port, network_id = EXCLUDED.network_id,
    table_version = EXCLUDED.table_version, operation_id = EXCLUDED.operation_id,
    accepted_at = clock_timestamp()
WHERE (environment_routes.generation, environment_routes.revision)
    < (EXCLUDED.generation, EXCLUDED.revision);

-- name: SaveRemovedRoute :execrows
INSERT INTO environment_routes (
    environment_id, pull_request_id, generation, revision, state, table_version, operation_id
) VALUES (
    sqlc.arg(environment_id), sqlc.arg(pull_request_id), sqlc.arg(generation), 0,
    'removed', sqlc.arg(table_version), sqlc.arg(operation_id)
)
ON CONFLICT (environment_id) DO UPDATE
SET generation = EXCLUDED.generation, revision = 0, state = 'removed',
    unconfirmed_targets = CASE
        WHEN environment_routes.container_id IS NULL
          OR (environment_routes.served_generation, environment_routes.served_revision)
             IS NOT DISTINCT FROM (environment_routes.generation, environment_routes.revision)
        THEN environment_routes.unconfirmed_targets
        ELSE environment_routes.unconfirmed_targets || jsonb_build_array(jsonb_build_object(
            'container_id', environment_routes.container_id, 'image_tag', environment_routes.image_tag))
    END,
    container_id = NULL, image_tag = NULL, backend = NULL, port = NULL, network_id = NULL,
    table_version = EXCLUDED.table_version, operation_id = EXCLUDED.operation_id,
    accepted_at = clock_timestamp()
WHERE (environment_routes.generation, environment_routes.revision)
    < (EXCLUDED.generation, 0::bigint);

-- name: MarkRouteServed :execrows
-- Records that the router loaded the accepted version. The file only moves
-- forward, so the router can no longer serve anything accepted before it. It
-- changes nothing when a newer version was accepted in the meantime.
UPDATE environment_routes
SET served_generation = generation, served_revision = revision,
    served_container_id = container_id, served_image_tag = image_tag,
    served_at = clock_timestamp(), unconfirmed_targets = '[]'::jsonb
WHERE environment_id = sqlc.arg(environment_id)
  AND generation = sqlc.arg(generation) AND revision = sqlc.arg(revision);

-- name: WakeForNewRoute :exec
-- A newly accepted version serves running containers: the preview is awake.
UPDATE environments SET asleep_at = NULL, wake_requested_at = NULL, wake_error = ''
WHERE id = $1 AND asleep_at IS NOT NULL;

-- name: ReadRoute :one
SELECT * FROM environment_routes WHERE pull_request_id = $1;

-- name: ReadRouteTable :many
-- One statement, one snapshot: the version and every active route at it. The
-- outer join returns the version even when no route is active.
SELECT t.version AS table_version, r.environment_id, e.subdomain,
       r.generation, r.revision, r.backend, r.port, r.network_id,
       coalesce(e.asleep_at IS NOT NULL, false)::boolean AS asleep,
       coalesce(rp.private_previews, false)::boolean AS private
FROM route_table t
LEFT JOIN environment_routes r ON r.state = 'active'
LEFT JOIN environments e ON e.id = r.environment_id
LEFT JOIN pull_requests p ON p.id = r.pull_request_id
LEFT JOIN repos rp ON rp.id = p.repo_id
ORDER BY r.environment_id NULLS FIRST;

-- name: ListRouteTargets :many
-- Containers and images the router serves, or may still serve until the
-- accepted version is confirmed. Cleanup never removes them.
SELECT container_id, image_tag, served_container_id, served_image_tag, unconfirmed_targets
FROM environment_routes WHERE pull_request_id = $1;

-- name: RouteVersionNewerThan :one
SELECT EXISTS (
    SELECT 1 FROM environment_routes
    WHERE pull_request_id = sqlc.arg(pull_request_id)
      AND (generation, revision) > (sqlc.arg(generation)::bigint, sqlc.arg(revision)::bigint)
) AS newer;

-- name: FailPendingActivation :one
-- A candidate that could not be published fails the current attempt. The
-- active preview, if any, keeps serving.
WITH locked AS MATERIALIZED (
    SELECT e.id, e.pending_container_id, e.pending_image_tag
    FROM environments e JOIN pull_requests pr ON pr.id = e.pull_request_id
    WHERE e.pull_request_id = sqlc.arg(pr_id)
      AND pr.generation = sqlc.arg(expected_generation) AND pr.state = 'open'
      AND e.pending_generation = sqlc.arg(expected_generation)
      AND e.pending_revision = sqlc.arg(expected_revision)
      AND e.pending_container_id = sqlc.arg(container_id)
      AND e.pending_image_tag = sqlc.arg(image_tag)
    FOR UPDATE OF pr, e
)
UPDATE environments e
SET status = 'failed', applied_generation = sqlc.arg(expected_generation),
    applied_revision = sqlc.arg(expected_revision), pending_container_id = NULL,
    pending_image_tag = NULL, pending_generation = NULL, pending_revision = NULL,
    pending_head_sha = NULL, last_error_code = sqlc.arg(error_code)::text
FROM locked
WHERE e.id = locked.id
RETURNING locked.pending_container_id, locked.pending_image_tag;

-- name: ReadProvisioningTarget :one
SELECT pr.number, r.github_repo_id,
       EXISTS (SELECT 1 FROM environments e WHERE e.pull_request_id = pr.id) AS provisioned
FROM pull_requests pr JOIN repos r ON r.id = pr.repo_id
WHERE pr.id = $1;

-- name: ProvisionEnvironment :execrows
-- One lifetime environment per PR. A hostname collision inserts nothing and
-- the caller allocates another label.
INSERT INTO environments (pull_request_id, subdomain) VALUES ($1, $2)
ON CONFLICT DO NOTHING;
