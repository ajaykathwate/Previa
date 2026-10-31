-- +goose Up
-- Native preview review and feedback (README-MVP-PRODUCT-EXPANSION.md,
-- Feature 3, with the publication references and activity hooks of delivery
-- step B). Previa owns these records; GitHub receives only a link back.

-- The effective configuration a candidate was built with, so a publication
-- can say whether two deployments of one commit ran the same configuration.
ALTER TABLE preview_components
    ADD COLUMN config_hash TEXT CHECK (config_hash IS NULL OR config_hash ~ '^[0-9a-f]{64}$');

-- An immutable reference to each candidate that actually started serving a
-- pull request's preview, written in the transaction that activates it.
-- Every activation (a new commit, or a Rebuild of the same commit) gets its
-- own row; a retried activation of the same candidate does not. Rows are
-- references, not runnable environments: containers, images and logs are not
-- retained. sequence numbers a pull request's deployments 1, 2, 3, ... for
-- display and addresses. workspace_id is provenance (the workspace that
-- published it); access always derives from the repository.
CREATE TABLE preview_publications (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    repo_id BIGINT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    workspace_id BIGINT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    head_sha TEXT NOT NULL CHECK (length(head_sha) BETWEEN 1 AND 64),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    revision BIGINT NOT NULL CHECK (revision >= 0),
    container_id TEXT,
    image_tag TEXT,
    config_hash TEXT CHECK (config_hash IS NULL OR config_hash ~ '^[0-9a-f]{64}$'),
    -- Already live when this migration ran: its real activation time and
    -- configuration are unknown, so pages say "Present at migration".
    backfilled BOOLEAN NOT NULL DEFAULT false,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (pull_request_id, generation, revision),
    UNIQUE (pull_request_id, sequence),
    UNIQUE (id, pull_request_id)
);

-- Existing live previews (including a previous preview still serving after a
-- failed update) receive their actual deployed fence. No review outcome or
-- build event is invented for them.
INSERT INTO preview_publications (pull_request_id, repo_id, workspace_id, sequence, head_sha, generation, revision,
                                  container_id, image_tag, backfilled)
SELECT p.id, p.repo_id, i.workspace_id, 1, e.head_sha, e.applied_generation, e.applied_revision,
       e.container_id, e.image_tag, true
FROM environments e
JOIN pull_requests p ON p.id = e.pull_request_id
JOIN repos r ON r.id = p.repo_id
JOIN installations i ON i.id = r.installation_id
WHERE e.status IN ('running', 'publishing', 'failed') AND e.head_sha IS NOT NULL AND e.container_id IS NOT NULL
  AND i.workspace_id IS NOT NULL;

-- Bounded activity: what people and the system did, for timelines. Events
-- reference objects and carry small, safe metadata (never message bodies,
-- secrets, tokens or app output). A dedup_key makes a retried write record
-- one event. Retention: 90 days and 5,000 events per pull request
-- (internal/activity); active review state never depends on these rows.
CREATE TABLE activity_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id BIGINT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    repo_id BIGINT REFERENCES repos (id) ON DELETE CASCADE,
    pull_request_id BIGINT REFERENCES pull_requests (id) ON DELETE CASCADE,
    publication_id BIGINT REFERENCES preview_publications (id),
    -- NULL: the system did it.
    actor_user_id BIGINT REFERENCES users (id),
    kind TEXT NOT NULL CHECK (kind ~ '^[a-z]+(_[a-z]+)*\.[a-z]+(_[a-z]+)*$'),
    visibility TEXT NOT NULL DEFAULT 'repository' CHECK (visibility IN ('repository', 'operators', 'administrators')),
    metadata JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 2048),
    dedup_key TEXT CHECK (dedup_key IS NULL OR length(dedup_key) BETWEEN 1 AND 200),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX activity_events_dedup ON activity_events (workspace_id, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX activity_events_pull_request ON activity_events (pull_request_id, occurred_at DESC, id DESC)
    WHERE pull_request_id IS NOT NULL;
CREATE INDEX activity_events_occurred ON activity_events (occurred_at);

-- The newest event pruned from a pull request's history, so a timeline can
-- say that older history was removed instead of implying it never existed.
CREATE TABLE activity_pruned (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests (id) ON DELETE CASCADE,
    pruned_through TIMESTAMPTZ NOT NULL
);

-- A pull request's testing instructions: written in Previa by people who can
-- build it, never by repository configuration, and separate from the GitHub
-- description. Edits compare revision.
CREATE TABLE pr_review_instructions (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests (id) ON DELETE CASCADE,
    summary TEXT NOT NULL DEFAULT '',
    steps TEXT NOT NULL DEFAULT '',
    expected TEXT NOT NULL DEFAULT '',
    start_path TEXT CHECK (start_path IS NULL OR (start_path ~ '^/' AND start_path !~ '^//' AND length(start_path) <= 300)),
    docs_path TEXT CHECK (docs_path IS NULL OR (docs_path ~ '^/' AND docs_path !~ '^//' AND length(docs_path) <= 300)),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_by BIGINT NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (octet_length(summary) + octet_length(steps) + octet_length(expected)
        + coalesce(octet_length(start_path), 0) + coalesce(octet_length(docs_path), 0) <= 32768)
);

-- Per pull request: how many findings and messages were ever written. It
-- numbers new findings and enforces the per-PR content limits; removing
-- content does not give allowance back. Writers lock this row, which keeps
-- review writes from contending with the build pipeline's locks.
CREATE TABLE pr_review_counters (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests (id) ON DELETE CASCADE,
    threads INTEGER NOT NULL DEFAULT 0 CHECK (threads >= 0),
    messages INTEGER NOT NULL DEFAULT 0 CHECK (messages >= 0)
);

-- A finding: a conversation about one observation. publication_id is the
-- deployment its author recorded it against (NULL: before any deployment);
-- an edit never changes it. resolved_publication_id is the deployment the
-- resolver had in front of them (NULL when no preview was live). A removed
-- finding keeps its number and who removed it; its messages are erased.
CREATE TABLE preview_feedback_threads (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    number INTEGER NOT NULL CHECK (number > 0),
    publication_id BIGINT,
    author_user_id BIGINT NOT NULL REFERENCES users (id),
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved', 'removed')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    state_changed_by BIGINT REFERENCES users (id),
    state_changed_at TIMESTAMPTZ,
    resolved_publication_id BIGINT,
    -- Set when a moderator removed it; NULL when its author did.
    removal_reason TEXT CHECK (removal_reason IN ('spam', 'abuse', 'sensitive', 'mistake')),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 16 AND 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (pull_request_id, number),
    UNIQUE (pull_request_id, idempotency_key),
    UNIQUE (id, pull_request_id),
    FOREIGN KEY (publication_id, pull_request_id) REFERENCES preview_publications (id, pull_request_id),
    FOREIGN KEY (resolved_publication_id, pull_request_id) REFERENCES preview_publications (id, pull_request_id),
    CHECK (state = 'resolved' OR resolved_publication_id IS NULL),
    CHECK (state = 'open' OR (state_changed_by IS NOT NULL AND state_changed_at IS NOT NULL)),
    CHECK (removal_reason IS NULL OR state = 'removed')
);
CREATE INDEX preview_feedback_threads_listing ON preview_feedback_threads (pull_request_id, state, number DESC);

-- A finding's messages in order: position 1 opens it and alone carries the
-- page path and reproduction details. Only the current text is kept; an edit
-- replaces it and records edited_at, and a removal erases it, leaving who
-- removed it (and a moderator's reason) for the tombstone.
CREATE TABLE preview_feedback_messages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id BIGINT NOT NULL,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position > 0),
    author_user_id BIGINT NOT NULL REFERENCES users (id),
    -- The deployment the author had in front of them when writing it.
    publication_id BIGINT,
    body TEXT NOT NULL CHECK (octet_length(body) <= 8192),
    page_path TEXT CHECK (page_path IS NULL OR (page_path ~ '^/' AND page_path !~ '^//' AND length(page_path) <= 300)),
    details TEXT CHECK (details IS NULL OR octet_length(details) <= 8192),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    -- Replies only; the opening message is deduplicated by its finding.
    idempotency_key TEXT CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 16 AND 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    edited_at TIMESTAMPTZ,
    removed_at TIMESTAMPTZ,
    removed_by BIGINT REFERENCES users (id),
    removal_reason TEXT CHECK (removal_reason IN ('spam', 'abuse', 'sensitive', 'mistake')),
    UNIQUE (thread_id, position),
    UNIQUE (thread_id, idempotency_key),
    FOREIGN KEY (thread_id, pull_request_id) REFERENCES preview_feedback_threads (id, pull_request_id) ON DELETE CASCADE,
    FOREIGN KEY (publication_id, pull_request_id) REFERENCES preview_publications (id, pull_request_id),
    CHECK (position = 1 OR (page_path IS NULL AND details IS NULL)),
    CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
    CHECK (removed_at IS NOT NULL OR btrim(body) <> ''),
    CHECK (removed_at IS NULL OR (body = '' AND page_path IS NULL AND details IS NULL)),
    CHECK (removal_reason IS NULL OR removed_at IS NOT NULL)
);
CREATE INDEX preview_feedback_messages_pull_request ON preview_feedback_messages (pull_request_id, thread_id);

-- A member's review-write allowance for the current hour, so scripted
-- edit/remove loops cannot churn storage or flood activity.
CREATE TABLE review_write_budgets (
    user_id BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    window_started_at TIMESTAMPTZ NOT NULL,
    used INTEGER NOT NULL CHECK (used >= 0)
);

-- Review changes wake the repository's live pages like any other change.
-- +goose StatementBegin
CREATE FUNCTION previa_review_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    changed record;
    repo bigint;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD IS NOT DISTINCT FROM NEW THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        changed := OLD;
    ELSE
        changed := NEW;
    END IF;
    SELECT p.repo_id INTO repo FROM pull_requests p WHERE p.id = changed.pull_request_id;
    PERFORM previa_notify_repository(repo, TG_TABLE_NAME);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER pr_review_instructions_realtime AFTER INSERT OR UPDATE OR DELETE ON pr_review_instructions
    FOR EACH ROW EXECUTE FUNCTION previa_review_changed();
CREATE TRIGGER preview_feedback_threads_realtime AFTER INSERT OR UPDATE OR DELETE ON preview_feedback_threads
    FOR EACH ROW EXECUTE FUNCTION previa_review_changed();
CREATE TRIGGER preview_feedback_messages_realtime AFTER INSERT OR UPDATE OR DELETE ON preview_feedback_messages
    FOR EACH ROW EXECUTE FUNCTION previa_review_changed();

-- +goose Down
-- Reviews, findings and activity written after this migration are discarded
-- by a rollback; take a backup first (docs/25-production-deployment.md).
DROP TRIGGER preview_feedback_messages_realtime ON preview_feedback_messages;
DROP TRIGGER preview_feedback_threads_realtime ON preview_feedback_threads;
DROP TRIGGER pr_review_instructions_realtime ON pr_review_instructions;
DROP FUNCTION previa_review_changed();
DROP TABLE review_write_budgets;
DROP TABLE preview_feedback_messages;
DROP TABLE preview_feedback_threads;
DROP TABLE pr_review_counters;
DROP TABLE pr_review_instructions;
DROP TABLE activity_pruned;
DROP TABLE activity_events;
DROP TABLE preview_publications;
ALTER TABLE preview_components DROP COLUMN config_hash;
