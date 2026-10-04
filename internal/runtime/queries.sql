-- name: ExecutionTarget :one
-- Everything the runtime rechecks immediately before work that needs access or
-- secrets: lifecycle, freshness, the authorized intent and repository access.
SELECT p.id AS pull_request_id, p.repo_id, p.number, p.head_sha, p.state, p.generation, p.is_fork, p.head_repo_id,
       r.github_repo_id, r.accessible, i.github_installation_id, i.status AS installation_status,
       b.generation AS intent_generation, b.revision AS intent_revision, b.head_sha AS intent_head_sha, b.state AS intent_state
FROM pull_requests p JOIN repos r ON r.id = p.repo_id JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
WHERE p.id = $1;
