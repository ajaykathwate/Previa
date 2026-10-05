-- Report bookkeeping. A reportable change bumps the PR's wanted version in the
-- same transaction as the change and its durable report operation.

-- name: ReadReportTarget :one
-- The PR as the change left it, and whether it has anything to report: a
-- living comment already, a build request, or a lifetime environment.
SELECT pr.id, pr.generation, pr.head_sha, pr.state,
       COALESCE(b.revision, 0)::bigint AS revision,
       (b.generation IS NOT DISTINCT FROM pr.generation) AS current_intent,
       (pr.github_comment_id IS NOT NULL
         OR b.pull_request_id IS NOT NULL
         OR EXISTS (SELECT 1 FROM environments e WHERE e.pull_request_id = pr.id)) AS reportable
FROM pull_requests pr
LEFT JOIN build_intents b ON b.pull_request_id = pr.id
WHERE pr.id = $1;

-- name: BumpReportWanted :one
INSERT INTO pull_request_reports (pull_request_id, wanted_version)
VALUES ($1, 1)
ON CONFLICT (pull_request_id) DO UPDATE
SET wanted_version = pull_request_reports.wanted_version + 1, updated_at = clock_timestamp()
RETURNING wanted_version;
