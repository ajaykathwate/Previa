-- Housekeeping reads lifecycle state to decide what to recover, expire,
-- confirm with GitHub or prune. Lifecycle changes always go through the
-- environments package; housekeeping only records its own bookkeeping and
-- deletes history whose retention ended (records.go).

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

-- name: DeleteOldDeliveries :execrows
-- Webhook delivery records only catch duplicates, which GitHub sends within
-- hours.
DELETE FROM webhook_deliveries d WHERE d.delivery_id IN (
    SELECT w.delivery_id FROM webhook_deliveries w WHERE w.received_at < sqlc.arg(cutoff) LIMIT sqlc.arg(batch_size));

-- name: DeleteOldOperations :execrows
-- Finished operations. A route operation of a PR's current generation may
-- protect what its preview serves (ListProtectingOperationPayloads), so it
-- stays while the PR's preview can be live; a failed cleanup stays as the
-- sign of something left behind, until its PR's records go.
DELETE FROM environment_operations d WHERE d.id IN (
    SELECT o.id FROM environment_operations o JOIN pull_requests pr ON pr.id = o.pull_request_id
    WHERE o.state IN ('succeeded', 'superseded', 'failed')
      AND coalesce(o.completed_at, o.updated_at) < sqlc.arg(cutoff)
      AND NOT (o.state = 'failed' AND o.kind IN ('remove_route', 'cleanup_artifact'))
      AND (o.kind NOT IN ('activate_route', 'remove_route') OR o.generation < pr.generation
           OR (pr.state <> 'open' AND pr.teardown_completed_generation >= pr.generation))
    LIMIT sqlc.arg(batch_size));

-- name: DeleteOldUsage :execrows
DELETE FROM account_build_usage WHERE day < sqlc.arg(before)::date;

-- name: ListRetiredPullRequests :many
-- Closed and merged PRs that GitHub last updated before cutoff, whose
-- teardown finished and which have no work left: nothing running, pending or
-- leased, and no report waiting to be written.
SELECT p.id FROM pull_requests p
WHERE p.state IN ('closed', 'merged') AND coalesce(p.source_updated_at, p.created_at) < sqlc.arg(cutoff)
  AND p.teardown_completed_generation >= p.generation
  AND NOT EXISTS (SELECT 1 FROM environment_operations o WHERE o.pull_request_id = p.id AND o.state IN ('pending', 'running', 'retryable'))
  AND NOT EXISTS (SELECT 1 FROM environment_routes rt WHERE rt.pull_request_id = p.id AND rt.state = 'active')
  AND NOT EXISTS (SELECT 1 FROM build_intents b WHERE b.pull_request_id = p.id AND b.state IN ('pending', 'queued', 'running'))
  AND NOT EXISTS (SELECT 1 FROM pull_request_reports rp WHERE rp.pull_request_id = p.id
      AND (rp.lease_owner IS NOT NULL OR rp.reported_version < rp.wanted_version))
  AND (sqlc.narg(pr_id)::bigint IS NULL OR p.id = sqlc.narg(pr_id))
ORDER BY p.id LIMIT sqlc.arg(batch_size);

-- name: LockPullRequest :one
-- A webhook that reopens the PR takes this lock first, or waits for the
-- deletion and recreates the PR.
SELECT id FROM pull_requests WHERE id = $1 FOR UPDATE;

-- name: DeleteRetiredBuildLogs :exec
DELETE FROM build_logs l USING environments e WHERE l.environment_id = e.id AND e.pull_request_id = $1;

-- name: DeleteRetiredRoutes :exec
DELETE FROM environment_routes WHERE pull_request_id = $1;

-- name: DeleteRetiredOperations :exec
DELETE FROM environment_operations WHERE pull_request_id = $1;

-- name: DeleteRetiredReports :exec
DELETE FROM pull_request_reports WHERE pull_request_id = $1;

-- name: DeleteRetiredReconciliations :exec
DELETE FROM pr_reconciliations WHERE pull_request_id = $1;

-- name: DeleteRetiredEnvironments :exec
-- Log streams go with their environment.
DELETE FROM environments WHERE pull_request_id = $1;

-- name: DeleteRetiredPullRequest :execrows
-- Build requests, preview records and dismissals go with their PR.
DELETE FROM pull_requests WHERE id = $1;

-- name: DeleteStaleOutputWatches :execrows
-- App output nobody watched for an hour, with its lines. A running follower
-- removes its own when its viewer leaves; this catches the rest.
DELETE FROM app_output_watches WHERE seen_at < sqlc.arg(cutoff) AND state <> 'following';

-- name: PruneWorkspaceBuildUsage :execrows
DELETE FROM workspace_build_usage WHERE day<sqlc.arg(before)::date;
