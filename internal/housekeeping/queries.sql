-- Housekeeping reads lifecycle state to decide what to recover, expire,
-- confirm with GitHub or prune. Lifecycle changes always go through the
-- environments package; housekeeping only records its own bookkeeping.

-- name: ListExpiredPreviews :many
-- Live previews past their expiry. A PR whose current build request is still
-- pending, queued or running is left for now: its publication sets a new
-- expiry, or the next pass finds it again.
SELECT e.pull_request_id, e.expires_at
FROM environments e
JOIN pull_requests pr ON pr.id = e.pull_request_id
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE e.expires_at <= sqlc.arg(now_at)
  AND e.status IN ('running', 'failed') AND e.container_id IS NOT NULL
  AND pr.state = 'open' AND pr.teardown_requested_generation <> pr.generation
  AND NOT COALESCE(b.generation = pr.generation AND b.state IN ('pending', 'queued', 'running'), false)
ORDER BY e.expires_at, e.pull_request_id
LIMIT sqlc.arg(batch_size);

-- name: ListPullRequestsToConfirm :many
-- Open PRs with live work (a preview, or a build request not yet finished)
-- whose installation is active and repository accessible, least recently
-- confirmed first, and none confirmed within the interval.
SELECT pr.id
FROM pull_requests pr
JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN environments e ON e.pull_request_id = pr.id
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
LEFT JOIN pr_reconciliations c ON c.pull_request_id = pr.id
WHERE pr.state = 'open' AND r.accessible AND i.status = 'active'
  AND pr.teardown_requested_generation <> pr.generation
  AND ((e.status IN ('running', 'failed', 'publishing') AND e.container_id IS NOT NULL)
       OR b.state IN ('pending', 'queued', 'running'))
  AND (c.reconciled_at IS NULL OR c.reconciled_at <= sqlc.arg(stale_before))
ORDER BY c.reconciled_at NULLS FIRST, pr.id
LIMIT sqlc.arg(batch_size);

-- name: RecordReconciliation :exec
INSERT INTO pr_reconciliations (pull_request_id, reconciled_at, outcome)
VALUES (sqlc.arg(pull_request_id), sqlc.arg(reconciled_at), sqlc.arg(outcome))
ON CONFLICT (pull_request_id) DO UPDATE SET reconciled_at = EXCLUDED.reconciled_at, outcome = EXCLUDED.outcome;

-- name: KnownPullRequests :many
SELECT id FROM pull_requests WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: ActiveRepositoryIDs :many
-- GitHub IDs of the repositories this instance may still build.
SELECT r.github_repo_id::bigint AS github_repo_id
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.accessible AND i.status = 'active' AND r.github_repo_id IS NOT NULL;

-- name: CountWaitingForDisk :one
SELECT count(*) FROM build_intents
WHERE waiting_reason = 'disk' AND waiting_revision = revision AND state IN ('pending', 'queued', 'running');
