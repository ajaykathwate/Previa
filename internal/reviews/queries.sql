-- name: ReviewTarget :one
-- A pull request's review context, for someone who may review it: its
-- source facts, what the preview serves now, the newest deployment and the
-- current build request (to say when a newer commit is on its way or failed).
-- The serving deployment's components (setup, services, docs) come from the
-- newest publication: after a failed update the environment's applied
-- revision is the failed attempt's, while the earlier deployment serves.
SELECT p.id, p.repo_id, p.number, p.title, p.author, p.head_branch, p.state, p.head_sha, p.description, p.html_url, p.is_fork,
       coalesce(p.teardown_reason, '')::text AS teardown_reason, r.full_name,
       workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate')::boolean AS can_operate,
       workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'manage')::boolean AS can_manage,
       coalesce(e.status, '')::text AS environment_status, coalesce(e.head_sha, '')::text AS deployed_sha,
       coalesce(e.subdomain, '')::text AS hostname, e.expires_at, e.asleep_at,
       coalesce(e.applied_generation, -1)::bigint AS applied_generation, coalesce(e.applied_revision, -1)::bigint AS applied_revision,
       coalesce(b.state, '')::text AS intent_state, coalesce(b.head_sha, '')::text AS intent_sha,
       coalesce(pc.docs_path, '')::text AS docs_path,
       coalesce(pc.setup, false)::boolean AS setup_declared, pc.setup_ms,
       coalesce(jsonb_array_length(pc.services) > 0, false)::boolean AS has_services,
       coalesce(pc.data_source, '')::text AS data_source, coalesce(pc.seed, false)::boolean AS seed_declared,
       -- Reset test data may start from the repository baseline (docs/32).
       coalesce(tds.enabled AND tds.use_baseline, false)::boolean AS uses_baseline,
       coalesce((SELECT rb.branch || '@' || left(rb.commit_sha, 7) FROM repository_baselines rb
                 WHERE rb.repo_id = r.id AND rb.status = 'current' ORDER BY rb.built_at DESC LIMIT 1), '')::text AS baseline_label,
       coalesce(e.last_error_code, '')::text AS failure_code,
       coalesce(b.revision, 0)::bigint AS request_revision,
       coalesce(b.generation = p.generation AND b.head_sha = p.head_sha, false)::boolean AS request_current,
       p.build_status, p.needs_refresh, (p.teardown_requested_generation = p.generation)::boolean AS teardown_requested,
       (r.accessible AND i.status = 'active')::boolean AS available,
       coalesce(pub.id, 0)::bigint AS publication_id, coalesce(pub.sequence, 0)::integer AS publication_sequence,
       coalesce(pub.head_sha, '')::text AS publication_sha, pub.activated_at AS publication_at,
       coalesce(pub.backfilled, false)::boolean AS publication_backfilled,
       coalesce(pub.generation, -1)::bigint AS publication_generation, coalesce(pub.revision, -1)::bigint AS publication_revision,
       (SELECT count(*) FROM preview_feedback_threads t WHERE t.pull_request_id = p.id AND t.state = 'open')::bigint AS open_findings
FROM pull_requests p
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
LEFT JOIN environments e ON e.pull_request_id = p.id
LEFT JOIN build_intents b ON b.pull_request_id = p.id
LEFT JOIN LATERAL (
    SELECT x.id, x.sequence, x.head_sha, x.activated_at, x.backfilled, x.generation, x.revision
    FROM preview_publications x WHERE x.pull_request_id = p.id
    ORDER BY x.sequence DESC LIMIT 1
) pub ON true
LEFT JOIN preview_components pc ON pc.pull_request_id = p.id AND pc.generation = pub.generation AND pc.revision = pub.revision
LEFT JOIN repository_sampledata_settings tds ON tds.repo_id = r.id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint AND p.number = sqlc.arg(number)::integer
  AND workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'preview');

-- name: GetInstructions :one
SELECT summary, steps, expected, coalesce(start_path, '')::text AS start_path, coalesce(docs_path, '')::text AS docs_path,
       revision, updated_by, updated_at
FROM pr_review_instructions WHERE pull_request_id = $1;

-- name: GetCounters :one
SELECT threads, messages FROM pr_review_counters WHERE pull_request_id = $1;

-- name: CountThreads :one
-- Findings matching the author, deployment and literal-text filters, by state.
SELECT count(*) FILTER (WHERE t.state = 'open')::bigint AS open,
       count(*) FILTER (WHERE t.state = 'resolved')::bigint AS resolved,
       count(*)::bigint AS total
FROM preview_feedback_threads t
LEFT JOIN preview_publications pub ON pub.id = t.publication_id
WHERE t.pull_request_id = sqlc.arg(pull_request_id)::bigint
  AND (sqlc.arg(author_id)::bigint = 0 OR t.author_user_id = sqlc.arg(author_id)::bigint)
  AND (sqlc.arg(deployment)::integer = 0 OR pub.sequence = sqlc.arg(deployment)::integer)
  AND (sqlc.arg(search)::text = '' OR EXISTS (
      SELECT 1 FROM preview_feedback_messages m
      WHERE m.thread_id = t.id AND m.removed_at IS NULL
        AND (strpos(lower(m.body), lower(sqlc.arg(search)::text)) > 0
          OR strpos(lower(coalesce(m.details, '')), lower(sqlc.arg(search)::text)) > 0
          OR strpos(lower(coalesce(m.page_path, '')), lower(sqlc.arg(search)::text)) > 0)));

-- name: ListThreads :many
-- One page of findings, newest first, with the same filters plus state
-- ('all' includes removed findings, which render as tombstones).
SELECT t.id, t.number, t.state, t.revision, t.author_user_id, t.created_at,
       coalesce(t.state_changed_by, 0)::bigint AS state_changed_by, t.state_changed_at, coalesce(t.removal_reason, '')::text AS removal_reason,
       coalesce(pub.sequence, 0)::integer AS publication_sequence, coalesce(pub.head_sha, '')::text AS publication_sha,
       coalesce(rpub.sequence, 0)::integer AS resolved_sequence, coalesce(rpub.head_sha, '')::text AS resolved_sha
FROM preview_feedback_threads t
LEFT JOIN preview_publications pub ON pub.id = t.publication_id
LEFT JOIN preview_publications rpub ON rpub.id = t.resolved_publication_id
WHERE t.pull_request_id = sqlc.arg(pull_request_id)::bigint
  AND (sqlc.arg(state)::text = 'all' OR t.state = sqlc.arg(state)::text)
  AND (sqlc.arg(author_id)::bigint = 0 OR t.author_user_id = sqlc.arg(author_id)::bigint)
  AND (sqlc.arg(deployment)::integer = 0 OR pub.sequence = sqlc.arg(deployment)::integer)
  AND (sqlc.arg(search)::text = '' OR EXISTS (
      SELECT 1 FROM preview_feedback_messages m
      WHERE m.thread_id = t.id AND m.removed_at IS NULL
        AND (strpos(lower(m.body), lower(sqlc.arg(search)::text)) > 0
          OR strpos(lower(coalesce(m.details, '')), lower(sqlc.arg(search)::text)) > 0
          OR strpos(lower(coalesce(m.page_path, '')), lower(sqlc.arg(search)::text)) > 0)))
ORDER BY t.number DESC
LIMIT sqlc.arg(page_size)::integer OFFSET sqlc.arg(page_offset)::integer;

-- name: GetThread :one
-- One finding by its number, for its own link.
SELECT t.id, t.number, t.state, t.revision, t.author_user_id, t.created_at,
       coalesce(t.state_changed_by, 0)::bigint AS state_changed_by, t.state_changed_at, coalesce(t.removal_reason, '')::text AS removal_reason,
       coalesce(pub.sequence, 0)::integer AS publication_sequence, coalesce(pub.head_sha, '')::text AS publication_sha,
       coalesce(rpub.sequence, 0)::integer AS resolved_sequence, coalesce(rpub.head_sha, '')::text AS resolved_sha
FROM preview_feedback_threads t
LEFT JOIN preview_publications pub ON pub.id = t.publication_id
LEFT JOIN preview_publications rpub ON rpub.id = t.resolved_publication_id
WHERE t.pull_request_id = sqlc.arg(pull_request_id)::bigint AND t.number = sqlc.arg(number)::integer;

-- name: ListMessages :many
-- The messages of some findings in order: each one's opening message and its
-- latest replies (latest of them; a large number returns all).
WITH ranked AS (
    SELECT m.*, row_number() OVER (PARTITION BY m.thread_id ORDER BY m.position DESC) AS from_end
    FROM preview_feedback_messages m WHERE m.thread_id = ANY(sqlc.arg(thread_ids)::bigint[])
)
SELECT r.id, r.thread_id, r.position, r.author_user_id, r.body, coalesce(r.page_path, '')::text AS page_path,
       coalesce(r.details, '')::text AS details, r.revision, r.created_at, r.edited_at, r.removed_at,
       coalesce(r.removed_by, 0)::bigint AS removed_by, coalesce(r.removal_reason, '')::text AS removal_reason,
       coalesce(pub.sequence, 0)::integer AS publication_sequence, coalesce(pub.head_sha, '')::text AS publication_sha,
       (SELECT count(*) FROM preview_feedback_messages c WHERE c.thread_id = r.thread_id)::integer AS thread_messages
FROM ranked r
LEFT JOIN preview_publications pub ON pub.id = r.publication_id
WHERE r.position = 1 OR r.from_end <= sqlc.arg(latest)::integer
ORDER BY r.thread_id, r.position;

-- name: ListThreadAuthors :many
-- Who started this pull request's findings, for the author filter.
SELECT DISTINCT author_user_id FROM preview_feedback_threads WHERE pull_request_id = $1 LIMIT 100;

-- name: ListThreadDeployments :many
-- The deployments this pull request's findings were recorded against.
SELECT DISTINCT pub.sequence, pub.head_sha
FROM preview_feedback_threads t JOIN preview_publications pub ON pub.id = t.publication_id
WHERE t.pull_request_id = $1
ORDER BY pub.sequence DESC LIMIT 100;

-- name: ReviewPeople :many
-- How people in a review are shown, and whether each is still an active
-- member of the repository's workspace.
SELECT u.id, u.username, u.first_name, u.last_name, u.avatar_url, u.custom_avatar, u.avatar_version,
       coalesce(u.public_id::text, '')::text AS public_id,
       EXISTS (SELECT 1 FROM repos r JOIN installations i ON i.id = r.installation_id
               JOIN workspace_memberships m ON m.workspace_id = i.workspace_id AND m.user_id = u.id AND m.state = 'active'
               WHERE r.id = sqlc.arg(repo_id)::bigint)::boolean AS member
FROM users u WHERE u.id = ANY(sqlc.arg(ids)::bigint[]);

-- name: LockReviewPullRequest :one
-- A review write's first lock, the same one any row referencing the pull
-- request takes. It never blocks GitHub updates of the pull request.
SELECT id, state FROM pull_requests
WHERE repo_id = sqlc.arg(repo_id)::bigint AND number = sqlc.arg(number)::integer
FOR KEY SHARE;

-- name: LockReviewer :one
-- The writer's active membership, locked for share: removing the member waits
-- for this write, and every later write sees the removal.
SELECT workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'preview')::boolean AS can_review,
       workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'operate')::boolean AS can_operate,
       workspace_repository_access(r.id, sqlc.arg(user_id)::bigint, 'manage')::boolean AS can_manage
FROM repos r
JOIN installations i ON i.id = r.installation_id
JOIN workspace_memberships m ON m.workspace_id = i.workspace_id AND m.user_id = sqlc.arg(user_id)::bigint AND m.state = 'active'
WHERE r.id = sqlc.arg(repo_id)::bigint
FOR SHARE OF m;

-- name: EnsureCounters :exec
INSERT INTO pr_review_counters (pull_request_id) VALUES ($1) ON CONFLICT (pull_request_id) DO NOTHING;

-- name: LockCounters :one
-- Serializes a pull request's review writes.
SELECT threads, messages FROM pr_review_counters WHERE pull_request_id = $1 FOR UPDATE;

-- name: CountWrite :exec
UPDATE pr_review_counters
SET threads = threads + sqlc.arg(threads)::integer, messages = messages + sqlc.arg(messages)::integer
WHERE pull_request_id = sqlc.arg(pull_request_id)::bigint;

-- name: CurrentPublication :one
-- The pull request's newest deployment, and whether its preview serves it
-- now (a failed update keeps serving the previous one).
SELECT x.id, x.sequence, x.head_sha,
       coalesce(e.status IN ('running', 'publishing', 'failed') AND e.head_sha IS NOT NULL
           AND e.applied_generation = x.generation AND e.applied_revision = x.revision, false)::boolean AS serving
FROM preview_publications x
LEFT JOIN environments e ON e.pull_request_id = x.pull_request_id
WHERE x.pull_request_id = $1
ORDER BY x.sequence DESC LIMIT 1;

-- name: PublicationBySequence :one
SELECT id, sequence, head_sha FROM preview_publications WHERE pull_request_id = $1 AND sequence = $2;

-- name: ThreadByKey :one
SELECT number FROM preview_feedback_threads WHERE pull_request_id = $1 AND idempotency_key = $2;

-- name: CreateThread :one
INSERT INTO preview_feedback_threads (pull_request_id, number, publication_id, author_user_id, idempotency_key)
VALUES (sqlc.arg(pull_request_id)::bigint, sqlc.arg(number)::integer, sqlc.narg(publication_id)::bigint,
        sqlc.arg(author_user_id)::bigint, sqlc.arg(idempotency_key)::text)
RETURNING id;

-- name: AddMessage :one
INSERT INTO preview_feedback_messages (thread_id, pull_request_id, position, author_user_id, publication_id, body, page_path, details, idempotency_key)
VALUES (sqlc.arg(thread_id)::bigint, sqlc.arg(pull_request_id)::bigint, sqlc.arg(position)::integer, sqlc.arg(author_user_id)::bigint,
        sqlc.narg(publication_id)::bigint, sqlc.arg(body)::text, sqlc.narg(page_path)::text, sqlc.narg(details)::text,
        sqlc.narg(idempotency_key)::text)
RETURNING id;

-- name: LockThread :one
-- A finding for a change to it, with its message counts.
SELECT t.id, t.state, t.revision, t.author_user_id,
       (SELECT count(*) FROM preview_feedback_messages m WHERE m.thread_id = t.id)::integer AS messages,
       (SELECT count(*) FROM preview_feedback_messages m
        WHERE m.thread_id = t.id AND m.position > 1 AND m.removed_at IS NULL)::integer AS visible_replies
FROM preview_feedback_threads t
WHERE t.pull_request_id = sqlc.arg(pull_request_id)::bigint AND t.number = sqlc.arg(number)::integer
FOR UPDATE;

-- name: ReplyByKey :one
SELECT position FROM preview_feedback_messages WHERE thread_id = $1 AND idempotency_key = $2;

-- name: LockMessage :one
SELECT id, position, author_user_id, body, coalesce(page_path, '')::text AS page_path, coalesce(details, '')::text AS details,
       revision, (removed_at IS NOT NULL)::boolean AS removed
FROM preview_feedback_messages
WHERE thread_id = sqlc.arg(thread_id)::bigint AND position = sqlc.arg(position)::integer
FOR UPDATE;

-- name: EditMessage :exec
UPDATE preview_feedback_messages
SET body = sqlc.arg(body)::text, page_path = sqlc.narg(page_path)::text, details = sqlc.narg(details)::text,
    revision = revision + 1, edited_at = clock_timestamp()
WHERE id = sqlc.arg(id)::bigint;

-- name: RemoveMessage :exec
-- Erases a message's text; who removed it (and a moderator's reason) stays.
UPDATE preview_feedback_messages
SET body = '', page_path = NULL, details = NULL, revision = revision + 1, removed_at = clock_timestamp(),
    removed_by = sqlc.arg(removed_by)::bigint, removal_reason = sqlc.narg(removal_reason)::text
WHERE id = sqlc.arg(id)::bigint AND removed_at IS NULL;

-- name: RemoveThreadMessages :exec
UPDATE preview_feedback_messages
SET body = '', page_path = NULL, details = NULL, revision = revision + 1, removed_at = clock_timestamp(),
    removed_by = sqlc.arg(removed_by)::bigint, removal_reason = sqlc.narg(removal_reason)::text
WHERE thread_id = sqlc.arg(thread_id)::bigint AND removed_at IS NULL;

-- name: SetThreadState :exec
UPDATE preview_feedback_threads
SET state = sqlc.arg(state)::text, revision = revision + 1, state_changed_by = sqlc.arg(changed_by)::bigint,
    state_changed_at = clock_timestamp(), resolved_publication_id = sqlc.narg(resolved_publication_id)::bigint,
    removal_reason = sqlc.narg(removal_reason)::text
WHERE id = sqlc.arg(id)::bigint;

-- name: LockInstructions :one
SELECT revision, summary, steps, expected, coalesce(start_path, '')::text AS start_path, coalesce(docs_path, '')::text AS docs_path
FROM pr_review_instructions WHERE pull_request_id = $1 FOR UPDATE;

-- name: SaveInstructions :one
INSERT INTO pr_review_instructions (pull_request_id, summary, steps, expected, start_path, docs_path, updated_by)
VALUES (sqlc.arg(pull_request_id)::bigint, sqlc.arg(summary)::text, sqlc.arg(steps)::text, sqlc.arg(expected)::text,
        sqlc.narg(start_path)::text, sqlc.narg(docs_path)::text, sqlc.arg(updated_by)::bigint)
ON CONFLICT (pull_request_id) DO UPDATE
SET summary = EXCLUDED.summary, steps = EXCLUDED.steps, expected = EXCLUDED.expected, start_path = EXCLUDED.start_path,
    docs_path = EXCLUDED.docs_path, updated_by = EXCLUDED.updated_by, revision = pr_review_instructions.revision + 1,
    updated_at = clock_timestamp()
RETURNING revision;

-- name: SpendWriteBudget :one
-- Counts one review write against the member's hourly allowance; no row when
-- the allowance is spent.
INSERT INTO review_write_budgets (user_id, window_started_at, used)
VALUES (sqlc.arg(user_id)::bigint, clock_timestamp(), 1)
ON CONFLICT (user_id) DO UPDATE SET
 window_started_at = CASE WHEN review_write_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
  THEN clock_timestamp() ELSE review_write_budgets.window_started_at END,
 used = CASE WHEN review_write_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
  THEN 1 ELSE review_write_budgets.used + 1 END
WHERE review_write_budgets.window_started_at <= clock_timestamp() - interval '1 hour'
 OR review_write_budgets.used < sqlc.arg(allowance)::integer
RETURNING used;

-- name: WriteBudgetReset :one
SELECT (window_started_at + interval '1 hour')::timestamptz AS resets_at FROM review_write_budgets WHERE user_id = $1;

-- name: GetTestAccess :one
-- A repository's test access as pages show it: never the secret itself.
SELECT account, (secret_ciphertext IS NOT NULL)::boolean AS has_secret, revision, updated_by, updated_at,
       coalesce(last_revealed_by, 0)::bigint AS last_revealed_by, last_revealed_at
FROM review_test_access WHERE repo_id = $1;

-- name: LockTestAccess :one
SELECT account, secret_ciphertext, key_version, revision FROM review_test_access WHERE repo_id = $1 FOR UPDATE;

-- name: SaveTestAccess :one
INSERT INTO review_test_access (repo_id, account, secret_ciphertext, key_version, updated_by)
VALUES (sqlc.arg(repo_id)::bigint, sqlc.arg(account)::text, sqlc.narg(secret_ciphertext)::bytea, sqlc.narg(key_version)::integer,
        sqlc.arg(updated_by)::bigint)
ON CONFLICT (repo_id) DO UPDATE
SET account = EXCLUDED.account, secret_ciphertext = EXCLUDED.secret_ciphertext, key_version = EXCLUDED.key_version,
    updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp(), revision = review_test_access.revision + 1
RETURNING revision;

-- name: DeleteTestAccess :exec
DELETE FROM review_test_access WHERE repo_id = $1;

-- name: MarkTestAccessRevealed :exec
UPDATE review_test_access SET last_revealed_by = sqlc.arg(user_id)::bigint, last_revealed_at = clock_timestamp()
WHERE repo_id = sqlc.arg(repo_id)::bigint;

-- name: TestAccessNeedingRotation :many
-- Sealed test access not on the active key, for key rotation.
SELECT id, repo_id, secret_ciphertext, key_version FROM review_test_access
WHERE secret_ciphertext IS NOT NULL AND key_version <> sqlc.arg(active_version)::integer AND id > sqlc.arg(after_id)::bigint
ORDER BY id LIMIT sqlc.arg(batch_size)::integer;

-- name: RotateTestAccess :execrows
-- Replaces a sealed value only if it is unchanged since it was read.
UPDATE review_test_access SET secret_ciphertext = sqlc.arg(sealed)::bytea, key_version = sqlc.arg(key_version)::integer
WHERE id = sqlc.arg(id)::bigint AND secret_ciphertext = sqlc.arg(previous)::bytea;

-- name: CountTestAccessByVersion :many
SELECT key_version::integer AS key_version, count(*)::bigint AS values FROM review_test_access
WHERE secret_ciphertext IS NOT NULL GROUP BY key_version;
