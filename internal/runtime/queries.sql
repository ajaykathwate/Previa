-- name: ExecutionTarget :one
-- Everything the runtime rechecks immediately before work that needs access or
-- secrets: lifecycle, freshness, the authorized intent and repository access.
SELECT p.id AS pull_request_id, p.repo_id, p.number, p.head_sha, p.state, p.generation, p.is_fork, p.head_repo_id,
       r.github_repo_id, r.accessible, i.github_installation_id, i.status AS installation_status, i.owner_user_id,
       b.generation AS intent_generation, b.revision AS intent_revision, b.head_sha AS intent_head_sha, b.state AS intent_state
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE p.id = $1;

-- name: AdmissionScope :one
-- The PR's base repository and account, for the advisory capacity check.
SELECT r.github_repo_id, i.owner_user_id FROM pull_requests p JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id WHERE p.id = $1;

-- name: RecordComponents :exec
-- What a ready candidate runs next to its app. Rows of earlier requests that
-- the PR serves neither as its deployed nor as its pending candidate go.
WITH recorded AS (
    INSERT INTO preview_components (pull_request_id, generation, revision, services, setup, processes, docs_path, outbound, outbound_withheld)
    VALUES (sqlc.arg(pull_request_id), sqlc.arg(generation), sqlc.arg(revision), sqlc.arg(services), sqlc.arg(setup),
            sqlc.arg(processes), sqlc.narg(docs_path), sqlc.arg(outbound), sqlc.arg(outbound_withheld))
    ON CONFLICT (pull_request_id, generation, revision) DO UPDATE
    SET services = EXCLUDED.services, setup = EXCLUDED.setup, processes = EXCLUDED.processes, docs_path = EXCLUDED.docs_path,
        outbound = EXCLUDED.outbound, outbound_withheld = EXCLUDED.outbound_withheld, recorded_at = now()
    RETURNING pull_request_id
)
DELETE FROM preview_components c
USING environments e
WHERE c.pull_request_id = sqlc.arg(pull_request_id) AND e.pull_request_id = c.pull_request_id
  AND (c.generation, c.revision) < (sqlc.arg(generation)::bigint, sqlc.arg(revision)::bigint)
  AND NOT (c.generation = e.applied_generation AND c.revision = e.applied_revision)
  AND NOT (c.generation IS NOT DISTINCT FROM e.pending_generation AND c.revision IS NOT DISTINCT FROM e.pending_revision);

-- name: ListOutboundPreviews :many
-- Pull requests whose preview network may carry outbound connections now: a
-- deployed or pending candidate, or a build starting one, in an accessible
-- repository of an active installation. Only same-repository pull requests
-- are trusted with approved destinations.
SELECT p.id AS pull_request_id, p.repo_id,
       (NOT p.is_fork AND p.head_repo_id IS NOT NULL AND p.head_repo_id = r.github_repo_id)::boolean AS trusted
FROM pull_requests p
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN environments e ON e.pull_request_id = p.id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE p.state = 'open' AND r.accessible AND i.status = 'active'
  AND (e.container_id IS NOT NULL OR e.pending_container_id IS NOT NULL OR b.state = 'running')
ORDER BY p.id;

-- name: RecordPreviewCrash :one
-- A container of the live preview the environment serves stopped. Nothing is
-- recorded for an older candidate, a preview being torn down or a crash
-- another worker already recorded.
INSERT INTO preview_crashes (container_id, event_nanos, environment_id, exit_code)
SELECT sqlc.arg(container_id)::text, sqlc.arg(event_nanos)::bigint, e.id, sqlc.arg(exit_code)::integer
FROM environments e JOIN pull_requests p ON p.id = e.pull_request_id
WHERE e.id = sqlc.arg(environment_id) AND e.status = 'running' AND e.asleep_at IS NULL
  AND e.applied_generation = sqlc.arg(generation) AND e.applied_revision = sqlc.arg(revision)
  AND p.state = 'open' AND p.teardown_requested_generation <> p.generation
ON CONFLICT DO NOTHING
RETURNING environment_id;
