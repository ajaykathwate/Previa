-- +goose Up
-- Guided onboarding (README-MVP-PRODUCT-EXPANSION.md, Feature 2). Connection,
-- configuration validity and deployment facts are derived on every view; only
-- what cannot be derived from existing records is stored here.

-- The latest setup check of each pull request: one bounded preflight of the
-- head commit's configuration, requested explicitly and run by a worker. A new
-- request bumps revision, so a delayed job for an older request records nothing.
-- A failed attempt keeps the last completed result (result_sha, result,
-- checked_at) so the page can show it with its time instead of a fake success.
CREATE TABLE repository_setup_checks (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_request_id BIGINT NOT NULL UNIQUE REFERENCES pull_requests(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    head_sha TEXT NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'complete', 'failed')),
    failure_code TEXT NOT NULL DEFAULT '' CHECK (failure_code ~ '^[a-z_]{0,48}$'),
    requested_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    result_sha TEXT NOT NULL DEFAULT '' CHECK (result_sha = '' OR result_sha ~ '^[0-9a-f]{40}$'),
    result JSONB CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    checked_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    -- What the check was validated against (offered services, build deadline,
    -- check rules); a change makes the result stale.
    platform TEXT NOT NULL DEFAULT '' CHECK (length(platform) <= 128),
    CHECK ((result IS NULL) = (checked_at IS NULL) AND (result IS NULL) = (result_sha = ''))
);
CREATE INDEX repository_setup_checks_recent ON repository_setup_checks (requested_at DESC);

-- Who opened which deployment of a pull request through Previa's own Open
-- preview action (router counts include every visitor, so they cannot say
-- this). One row per person and pull request: the latest open.
CREATE TABLE preview_opens (
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deployed_sha TEXT NOT NULL CHECK (length(deployed_sha) BETWEEN 1 AND 64),
    opened_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (pull_request_id, user_id)
);

-- A workspace's setup-check allowance for the current hour, so scripted
-- rechecks cannot spend the GitHub App's rate limit.
CREATE TABLE setup_check_budgets (
    workspace_id BIGINT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    window_started_at TIMESTAMPTZ NOT NULL,
    used INTEGER NOT NULL CHECK (used >= 0)
);

-- Setup progress wakes the repository's live pages like any other change.
-- +goose StatementBegin
CREATE FUNCTION previa_setup_changed() RETURNS trigger
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
CREATE TRIGGER repository_setup_checks_realtime AFTER INSERT OR UPDATE OR DELETE ON repository_setup_checks
    FOR EACH ROW EXECUTE FUNCTION previa_setup_changed();
CREATE TRIGGER preview_opens_realtime AFTER INSERT OR UPDATE OR DELETE ON preview_opens
    FOR EACH ROW EXECUTE FUNCTION previa_setup_changed();

-- +goose Down
DROP TRIGGER preview_opens_realtime ON preview_opens;
DROP TRIGGER repository_setup_checks_realtime ON repository_setup_checks;
DROP FUNCTION previa_setup_changed();
DROP TABLE setup_check_budgets;
DROP TABLE preview_opens;
DROP TABLE repository_setup_checks;
