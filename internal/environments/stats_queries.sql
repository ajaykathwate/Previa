-- name: EnvironmentStatusCounts :many
-- Scrape-time summaries for metrics: counts only, no identifiers.
SELECT status, count(*)::bigint AS environments
FROM environments
GROUP BY status;

-- name: OperationStateCounts :many
-- Unfinished operations, and those that failed within a day (failed rows are
-- kept for diagnosis; an old failure must not page forever).
SELECT kind, state, count(*)::bigint AS operations
FROM environment_operations
WHERE state IN ('pending', 'running', 'retryable')
   OR (state = 'failed' AND updated_at > now() - interval '24 hours')
GROUP BY kind, state;

-- name: BuildRequestCounts :many
SELECT state, COALESCE(waiting_reason, '')::text AS waiting, count(*)::bigint AS requests
FROM build_intents
WHERE state IN ('pending', 'queued', 'running')
GROUP BY state, waiting_reason;
