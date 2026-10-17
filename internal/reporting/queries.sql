-- Reporting reads the PR's persisted truth and keeps its own bookkeeping in
-- pull_request_reports. It never changes lifecycle state.

-- name: ReadReportSnapshot :one
SELECT pr.id, pr.number, pr.head_sha, pr.state, pr.generation, pr.build_status, pr.needs_refresh,
       pr.is_fork, pr.github_comment_id, pr.teardown_requested_generation, pr.teardown_reason,
       r.id AS repo_id, r.public_id::text AS repo_public_id, pr.public_id::text AS pr_public_id, r.github_repo_id, r.accessible, i.github_installation_id,
       i.status AS installation_status,
       b.revision AS intent_revision, b.generation AS intent_generation, b.head_sha AS intent_head_sha,
       COALESCE(b.execution_error_code,'')::text AS intent_execution_error, b.state AS intent_state, b.available_at AS intent_available_at,
       COALESCE(CASE WHEN b.waiting_revision = b.revision THEN b.waiting_reason END, '')::text AS intent_waiting,
       e.id AS environment_id, e.status AS environment_status, e.subdomain, e.head_sha AS deployed_sha,
       e.applied_generation, e.applied_revision, e.pending_generation, e.pending_revision,
       e.last_error_code, e.expires_at,
       (SELECT count(*) FROM env_secrets s WHERE s.repo_id = r.id)::bigint AS secret_count,
       rep.wanted_version, rep.reported_version, rep.comment_digest, rep.check_head_sha,
       rep.check_revision, rep.check_run_id, rep.check_open, rep.check_digest,
       pc.services AS backend_services, COALESCE(pc.setup, false)::boolean AS backend_setup,
       pc.processes AS backend_processes, pc.docs_path AS backend_docs_path
FROM pull_requests pr
JOIN repos r ON r.id = pr.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
LEFT JOIN environments e ON e.pull_request_id = pr.id
LEFT JOIN pull_request_reports rep ON rep.pull_request_id = pr.id
-- What the serving deployment runs: the newest publication's candidate. After a
-- failed update the environment's applied revision names the failed attempt.
LEFT JOIN LATERAL (
    SELECT x.generation, x.revision FROM preview_publications x
    WHERE x.pull_request_id = pr.id ORDER BY x.sequence DESC LIMIT 1
) pub ON true
LEFT JOIN preview_components pc ON pc.pull_request_id = pr.id
    AND pc.generation = pub.generation AND pc.revision = pub.revision
WHERE pr.id = $1;

-- name: AcquireReportLease :one
-- One reporter per PR at a time. A crashed reporter's lease expires.
UPDATE pull_request_reports
SET lease_owner = sqlc.arg(owner), lease_expires_at = clock_timestamp() + sqlc.arg(lease)::interval
WHERE pull_request_id = sqlc.arg(pull_request_id)
  AND (lease_owner IS NULL OR lease_expires_at <= clock_timestamp() OR lease_owner = sqlc.arg(owner))
RETURNING wanted_version, reported_version;

-- name: ReadReportLease :one
SELECT lease_owner, lease_expires_at FROM pull_request_reports WHERE pull_request_id = $1;

-- name: ReleaseReportLease :exec
UPDATE pull_request_reports SET lease_owner = NULL, lease_expires_at = NULL
WHERE pull_request_id = sqlc.arg(pull_request_id) AND lease_owner = sqlc.arg(owner);

-- name: SaveCommentID :execrows
-- Saved right after GitHub accepts a new comment, under the lease. If this
-- write is lost, the next report finds the comment by its marker.
UPDATE pull_requests pr SET github_comment_id = sqlc.narg(comment_id)
FROM pull_request_reports rep
WHERE pr.id = sqlc.arg(pull_request_id) AND rep.pull_request_id = pr.id
  AND rep.lease_owner = sqlc.arg(owner) AND rep.lease_expires_at > clock_timestamp();

-- name: RecordReport :execrows
-- Records what was written for a version, only while the lease is held.
UPDATE pull_request_reports
SET comment_digest = sqlc.narg(comment_digest),
    check_head_sha = sqlc.narg(check_head_sha), check_revision = sqlc.narg(check_revision),
    check_run_id = sqlc.narg(check_run_id), check_open = sqlc.arg(check_open),
    check_digest = sqlc.narg(check_digest),
    reported_version = GREATEST(reported_version, sqlc.arg(version)::bigint),
    last_error_code = sqlc.narg(last_error_code),
    reported_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE pull_request_id = sqlc.arg(pull_request_id)
  AND lease_owner = sqlc.arg(owner) AND lease_expires_at > clock_timestamp();
