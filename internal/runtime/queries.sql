-- name: ExecutionTarget :one
-- Everything the runtime rechecks immediately before work that needs access or
-- secrets: lifecycle, freshness, the authorized intent and repository access.
SELECT p.id AS pull_request_id, p.repo_id, p.number, p.head_sha, p.state, p.generation, p.is_fork, p.head_repo_id,
       r.github_repo_id, r.accessible, i.github_installation_id, i.status AS installation_status,
       b.generation AS intent_generation, b.revision AS intent_revision, b.head_sha AS intent_head_sha, b.state AS intent_state
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE p.id = $1;

-- name: BaseRepository :one
SELECT r.github_repo_id FROM pull_requests p JOIN repos r ON r.id = p.repo_id WHERE p.id = $1;

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
